// Package lite keeps songs re-encoded for lite mode, which a player turns on to use less data: Ogg
// Opus at 128 kbit/s, about a sixth of a CD-quality FLAC. A song is made as it is played, or ahead
// as the song preloaded next, from its original read through the stream cache into FFmpeg as it
// arrives, and is served whole like an original (ranges, seeking, a known length): written aside,
// then renamed in place, so a request never gets one half made. One that does not come out smaller
// is not kept: the original is sent instead. The same song asked for at once is made once; two are
// made at a time; one that nobody waits for, plays or preloads any more stops; and the folder is
// held to a budget, the songs played longest ago going first. Files are named after their original's
// checksum, so they stay good across restarts.
package lite

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Quality is what a player asks for (?q=) to get a song as lite mode plays it.
const Quality = "opus128"

const (
	kbps = 128
	// An original of at most this many bits a second is sent as it is: lite mode's Ogg Opus comes
	// to about as much (VBR, and Ogg's own), so a lossy one would lose some for little, and a lossless
	// one would grow (review #212).
	keepRate = 160_000
	workers  = 2
	timeout  = 15 * time.Minute
	// A song that could not be made is sent as it is for this long, so the requests of one playing
	// never get two different files (a range of each).
	badFor = time.Hour
	ext    = ".opus.ogg"
)

// Shrinks reports whether lite mode re-encodes an original of this format and bitrate (bits a
// second, on average): those well above the lite bitrate, and lossless ones of a bitrate not known.
// What comes out no smaller is sent as it is all the same (ErrNotSmaller). The web client says the
// same (player.js shrinks).
func Shrinks(format string, bitrate int) bool {
	return bitrate > keepRate || format == "flac" && bitrate <= 0
}

// Song is an original to re-encode.
type Song struct {
	DriveID  string
	SHA256   string // names what is made from it
	Size     int64
	Format   string // flac, mp3, m4a, ogg, opus
	Channels int
}

// Source reads originals: the stream cache.
type Source interface {
	// Copy writes the whole of Drive file id to w as it arrives.
	Copy(ctx context.Context, w io.Writer, id string, size int64) error
	// Hold gives the path of a wholly cached copy of a Drive file, kept until release.
	Hold(id string) (path string, release func(), ok bool)
}

// Encoder makes a lite file: FFmpeg (ffmpeg.Tool.Opus).
type Encoder interface {
	Opus(ctx context.Context, src string, in io.Reader, dst string, kbps int, downmix bool) error
}

// SourceError is the original not arriving (Drive failing, or not connected): the player is told
// why, as it is for an original.
type SourceError struct{ Err error }

func (e *SourceError) Error() string { return "reading the original: " + e.Err.Error() }
func (e *SourceError) Unwrap() error { return e.Err }

// ErrFailed is a song that could not be made a short while ago: it is sent as it is.
var ErrFailed = errors.New("this song could not be re-encoded")

// ErrNotSmaller is a song that, re-encoded, came out not even a tenth smaller than its original
// (review #212): lossless at a low bitrate, say. It is sent as it is from then on, every request of
// it alike.
var ErrNotSmaller = errors.New("this song re-encoded is not smaller")

type Store struct {
	dir    string
	budget int64
	enc    Encoder
	src    Source
	log    *slog.Logger
	turns  chan struct{}
	lean   atomic.Bool // low disk: nothing is made ahead

	mu            sync.Mutex
	calls         map[string]*call
	bad           map[string]time.Time // songs that could not be made -> until when they are sent as they are
	plain         map[string]bool      // songs not smaller re-encoded: sent as they are until a restart
	current, next string               // the song played last and the one preloaded last (checksums)
	used          int64                // bytes in dir; -1 until counted
}

type call struct {
	done    chan struct{}
	err     error
	waiting int // requests waiting for it
	stop    context.CancelFunc
}

// New keeps lite files in dir, at most budget bytes. enc nil: there is no lite mode (no FFmpeg
// with Opus), and players get the originals. What a stop left half made goes.
func New(dir string, budget int64, enc Encoder, src Source, log *slog.Logger) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, budget: budget, enc: enc, src: src, log: log, turns: make(chan struct{}, workers),
		calls: map[string]*call{}, bad: map[string]time.Time{}, plain: map[string]bool{}, used: -1}
	for _, e := range s.list() {
		if !strings.HasSuffix(e.path, ext) || e.size == 0 {
			os.Remove(e.path)
		}
	}
	return s, nil
}

// Available reports whether songs can be re-encoded.
func (s *Store) Available() bool { return s != nil && s.enc != nil }

func (s *Store) path(sha string) string { return filepath.Join(s.dir, sha+ext) }

// Get opens the lite file of song o, made first if it is not here. o is the song played: it is
// still made when its request goes (a player asks again), until another song is played.
// ErrFailed: it could not be made a short while ago; ErrNotSmaller: it is not worth it; a
// *SourceError: the original did not arrive.
func (s *Store) Get(ctx context.Context, o Song) (*os.File, error) {
	if !s.Available() {
		return nil, ErrFailed
	}
	key := o.SHA256
	s.mu.Lock()
	s.playLocked(key)
	if f, err := s.openLocked(key); err == nil {
		s.mu.Unlock()
		return f, nil
	}
	if s.plain[key] {
		s.mu.Unlock()
		return nil, ErrNotSmaller
	}
	if until, ok := s.bad[key]; ok {
		if time.Now().Before(until) {
			s.mu.Unlock()
			return nil, ErrFailed
		}
		delete(s.bad, key)
	}
	c := s.startLocked(o)
	c.waiting++
	s.mu.Unlock()
	select {
	case <-c.done:
	case <-ctx.Done():
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c.waiting--
	s.settleLocked()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.err != nil {
		return nil, c.err
	}
	return s.openLocked(key)
}

// Played tells the store that song sha (a checksum) is played as it is: lite mode is off, or sends
// it as it is. What was being made for the song played before stops, unless it is waited for or
// preloaded, as when another song is played re-encoded (review #208).
func (s *Store) Played(sha string) {
	if !s.Available() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.playLocked(sha)
}

func (s *Store) playLocked(key string) {
	if s.current != key {
		s.current = key
		s.settleLocked()
	}
}

// Has reports whether song sha's lite file is here: once a request for it was answered with the
// song re-encoded, the song it plays (review #209).
func (s *Store) Has(sha string) bool {
	if !s.Available() {
		return false
	}
	_, err := os.Stat(s.path(sha))
	return err == nil
}

// openLocked opens a lite file that is here, marking it played now (the budget keeps the songs
// played last). Under mu, so it is not evicted between.
func (s *Store) openLocked(key string) (*os.File, error) {
	f, err := os.Open(s.path(key))
	if err == nil {
		now := time.Now()
		os.Chtimes(f.Name(), now, now)
	}
	return f, err
}

// Prefetch makes the lite file of the song preloaded next, unless it is here (or the disk is low);
// the one preloaded before stops, unless it is played or waited for.
func (s *Store) Prefetch(o Song) {
	if !s.Available() {
		return
	}
	key := o.SHA256
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next = key
	s.settleLocked()
	if s.lean.Load() {
		return
	}
	if f, err := s.openLocked(key); err == nil {
		f.Close()
		return
	}
	if until, ok := s.bad[key]; (ok && time.Now().Before(until)) || s.plain[key] {
		return
	}
	s.startLocked(o)
}

// Unprefetch lets go of the song preloaded last: the next one is another now, or none (or the
// player gets it as it is), so it stops being made unless it is played or waited for.
func (s *Store) Unprefetch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next = ""
	s.settleLocked()
}

// SetLean stops making songs ahead while the disk is low.
func (s *Store) SetLean(on bool) { s.lean.Store(on) }

// startLocked starts making song o unless it is being made, and returns the call to wait for.
func (s *Store) startLocked(o Song) *call {
	key := o.SHA256
	if c, ok := s.calls[key]; ok {
		return c
	}
	ctx, stop := context.WithTimeout(context.Background(), timeout)
	c := &call{done: make(chan struct{}), stop: stop}
	s.calls[key] = c
	go func() {
		defer stop()
		err := s.make(ctx, o)
		s.mu.Lock()
		if s.calls[key] == c {
			delete(s.calls, key)
		}
		var se *SourceError
		switch {
		case errors.Is(err, ErrNotSmaller):
			s.plain[key] = true
			s.log.Info("lite: not smaller re-encoded; sent as it is", "file", o.DriveID, "size", o.Size)
		case err != nil && ctx.Err() == nil && !errors.As(err, &se):
			s.bad[key] = time.Now().Add(badFor)
			s.log.Warn("lite: re-encoding failed; sent as it is", "file", o.DriveID, "err", err)
		}
		c.err = err
		s.mu.Unlock()
		close(c.done)
	}()
	return c
}

// settleLocked stops making the songs nobody waits for that are neither the one played last nor the
// one preloaded last (songs skipped); a later request starts anew.
func (s *Store) settleLocked() {
	for key, c := range s.calls {
		if c.waiting == 0 && key != s.current && key != s.next {
			c.stop()
			delete(s.calls, key)
		}
	}
}

// make re-encodes o in its turn, then puts the file in place and holds the folder to its budget.
func (s *Store) make(ctx context.Context, o Song) error {
	select {
	case s.turns <- struct{}{}:
		defer func() { <-s.turns }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil { // a turn free and the end both there: it may have been either
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".new-*")
	if err != nil {
		return err
	}
	tmp.Close()
	defer os.Remove(tmp.Name()) // nothing once renamed
	if err := s.encode(ctx, o, tmp.Name()); err != nil {
		return err
	}
	info, err := os.Stat(tmp.Name())
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return errors.New("ffmpeg made an empty file")
	}
	if o.Size > 0 && info.Size()*10 > o.Size*9 {
		return ErrNotSmaller
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Rename(tmp.Name(), s.path(o.SHA256)); err != nil {
		return err
	}
	if s.used < 0 {
		s.used = s.countLocked()
	} else {
		s.used += info.Size()
	}
	if s.used > s.budget {
		s.evictLocked(s.budget * 8 / 10)
	}
	return nil
}

// encode writes o re-encoded to dst: read from the original as it arrives, or, for MP4 (which
// FFmpeg reads with seeking), from the whole copy the stream cache then keeps.
func (s *Store) encode(ctx context.Context, o Song, dst string) error {
	downmix := o.Channels > 2
	if o.Format == "m4a" {
		if err := s.src.Copy(ctx, io.Discard, o.DriveID, o.Size); err != nil {
			return s.sourceErr(ctx, err)
		}
		path, release, ok := s.src.Hold(o.DriveID)
		if !ok {
			return errors.New("the original is not cached whole (no room)")
		}
		defer release()
		return s.enc.Opus(ctx, path, nil, dst, kbps, downmix)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	// The copy has a context of its own: one waiting for more of the original (the cache, Drive) when
	// FFmpeg stops would not see the pipe closed until it wrote again (review #207).
	copyCtx, stopCopy := context.WithCancel(ctx)
	defer stopCopy()
	var srcErr error
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		srcErr = s.src.Copy(copyCtx, pw, o.DriveID, o.Size)
		pw.Close() // the end: FFmpeg reads to it
	}()
	err = s.enc.Opus(ctx, "pipe:0", pr, dst, kbps, downmix)
	pr.Close() // FFmpeg is done: a copy still writing fails (EPIPE)
	stopCopy() // and one waiting stops
	<-copied
	stopped := errors.Is(srcErr, syscall.EPIPE) || ctx.Err() == nil && errors.Is(srcErr, context.Canceled)
	if srcErr != nil && !stopped {
		// Not all of it arrived: what FFmpeg made of it, if anything, is cut short.
		return s.sourceErr(ctx, srcErr)
	}
	return err // what FFmpeg said, stopping early (bad data, say) or not
}

func (s *Store) sourceErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return &SourceError{err}
}

type entry struct {
	path string
	size int64
	mod  time.Time
}

func (s *Store) list() []entry {
	des, _ := os.ReadDir(s.dir)
	var out []entry
	for _, de := range des {
		if info, err := de.Info(); err == nil && info.Mode().IsRegular() {
			out = append(out, entry{filepath.Join(s.dir, de.Name()), info.Size(), info.ModTime()})
		}
	}
	return out
}

func (s *Store) countLocked() int64 {
	var n int64
	for _, e := range s.list() {
		if strings.HasSuffix(e.path, ext) {
			n += e.size
		}
	}
	return n
}

// evictLocked removes the files played longest ago until the folder holds at most target bytes,
// keeping the song played last and the one preloaded last. A file being served is read to its end
// all the same (it is open). It returns the bytes freed.
func (s *Store) evictLocked(target int64) int64 {
	var list []entry
	var total, freed int64
	for _, e := range s.list() {
		if strings.HasSuffix(e.path, ext) {
			list = append(list, e)
			total += e.size
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].mod.Before(list[j].mod) })
	keep := map[string]bool{s.path(s.current): s.current != "", s.path(s.next): s.next != ""}
	for _, e := range list {
		if total <= target {
			break
		}
		if keep[e.path] {
			continue
		}
		if os.Remove(e.path) == nil {
			total -= e.size
			freed += e.size
		}
	}
	s.used = total
	return freed
}

// Used is the space the lite files take.
func (s *Store) Used() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used < 0 {
		s.used = s.countLocked()
	}
	return s.used
}

// Trim removes every lite file but those of the song played last and the one preloaded last (the
// disk guard, and emptying the cache from the settings page); it says how many bytes that freed.
func (s *Store) Trim() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.evictLocked(0)
}
