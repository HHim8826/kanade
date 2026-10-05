// Package thumbs keeps the covers the web client shows: resized to a few sizes, and the originals
// the cover viewer opens, each made once from the original in Drive (review #157). A file appears
// whole or not at all (written aside, then renamed), so a request never gets one half written; the
// same cover asked for at once is made once; only a few covers are made at a time, from reading the
// original to its last resize, so originals wait for their turn unread (decoding a large scan takes
// about 100 MB; review #176), and one nobody waits for any more stops; the folder is held to a
// budget, the oldest files going first; and an image that cannot be decoded is not fetched again
// for a while.
package thumbs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // covers are JPEG or PNG
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/draw"
)

// Sizes are the sizes made (the longest side, in pixels); 0 is the original.
var Sizes = []int{96, 256, 300, 512, 600, 1024}

// Size is what a request for n pixels gets: the smallest size made that is at least n (the largest
// for more), 0 for the original, and 300 for anything else.
func Size(n int) int {
	if n == 0 {
		return 0
	}
	if n < 0 {
		return 300
	}
	for _, s := range Sizes {
		if s >= n {
			return s
		}
	}
	return Sizes[len(Sizes)-1]
}

const (
	maxOriginal = 32 << 20
	maxPixels   = 25_000_000 // refuse to decode huge scans on a 1.5 GB VPS (plan §5)
	badFor      = 10 * time.Minute
)

// ErrUndecodable is a cover that is no image this can read (or too large to resize).
var ErrUndecodable = errors.New("the cover image cannot be read")

type Store struct {
	dir    string
	budget int64
	log    *slog.Logger

	jobs    chan struct{} // covers resized at once, from reading the original to the end
	fetches chan struct{} // originals read from Drive at once
	decodes chan struct{} // images decoded at once

	mu    sync.Mutex
	calls map[string]*call
	bad   map[string]time.Time // covers that could not be decoded -> when to try again
	used  int64                // bytes in dir; -1 until counted
}

type call struct {
	done    chan struct{}
	data    []byte
	err     error
	waiting int                // askers waiting for it
	stop    context.CancelFunc // once none is
}

// New keeps covers in dir, at most budget bytes. What a stop left half written, or an earlier
// version wrote empty, goes.
func New(dir string, budget int64, log *slog.Logger) *Store {
	s := &Store{dir: dir, budget: budget, log: log, jobs: make(chan struct{}, 3), fetches: make(chan struct{}, 6), decodes: make(chan struct{}, 2),
		calls: map[string]*call{}, bad: map[string]time.Time{}, used: -1}
	for _, e := range s.list() {
		if e.size == 0 || strings.HasPrefix(filepath.Base(e.path), ".new-") {
			os.Remove(e.path)
		}
	}
	return s
}

// Get is cover sha (an image of type mime) at size (a Size), from the folder or made from the
// original that open reads.
func (s *Store) Get(ctx context.Context, sha string, size int, open func(context.Context) (io.ReadCloser, error)) ([]byte, error) {
	name := fmt.Sprintf("%s-%d.jpg", sha, size)
	if size == 0 {
		name = sha + "-orig"
	}
	path := filepath.Join(s.dir, name)
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		return data, nil
	}
	s.mu.Lock()
	if until, ok := s.bad[sha]; ok && size != 0 {
		if time.Now().Before(until) {
			s.mu.Unlock()
			return nil, ErrUndecodable
		}
		delete(s.bad, sha)
	}
	c, ok := s.calls[name]
	if !ok {
		// Not the first asker's to cancel: others may wait for it. It stops once none does.
		mctx, stop := context.WithCancel(context.WithoutCancel(ctx))
		c = &call{done: make(chan struct{}), stop: stop}
		s.calls[name] = c
		go func() {
			defer stop()
			c.data, c.err = s.make(mctx, sha, size, path, open)
			s.mu.Lock()
			if s.calls[name] == c {
				delete(s.calls, name)
			}
			s.mu.Unlock()
			close(c.done)
		}()
	}
	c.waiting++
	s.mu.Unlock()
	select {
	case <-c.done:
		return c.data, c.err
	case <-ctx.Done():
		s.mu.Lock()
		if c.waiting--; c.waiting == 0 {
			c.stop()
			if s.calls[name] == c {
				delete(s.calls, name) // the next asker starts anew
			}
		}
		s.mu.Unlock()
		return nil, ctx.Err()
	}
}

// take waits for a turn: none for one given up (a turn free and the end both there, it may be
// either).
func take(ctx context.Context, sem chan struct{}) (func(), error) {
	select {
	case sem <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-sem
			return nil, err
		}
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Store) make(ctx context.Context, sha string, size int, path string, open func(context.Context) (io.ReadCloser, error)) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if size != 0 {
		// Its turn first: the original is read only when it can be resized soon after.
		done, err := take(ctx, s.jobs)
		if err != nil {
			return nil, err
		}
		defer done()
	}
	orig, err := s.original(ctx, open)
	if err != nil {
		return nil, err
	}
	data := orig
	if size != 0 {
		if data, err = s.resize(ctx, orig, size); errors.Is(err, ErrUndecodable) {
			s.mu.Lock()
			s.bad[sha] = time.Now().Add(badFor)
			s.mu.Unlock()
		}
		if err != nil {
			return nil, err
		}
	}
	if err := s.keep(path, data); err != nil {
		s.log.Warn("keep cover", "file", filepath.Base(path), "err", err) // served all the same
	}
	return data, nil
}

func (s *Store) original(ctx context.Context, open func(context.Context) (io.ReadCloser, error)) ([]byte, error) {
	done, err := take(ctx, s.fetches)
	if err != nil {
		return nil, err
	}
	defer done()
	rc, err := open(ctx)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxOriginal+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxOriginal {
		return nil, fmt.Errorf("%w: larger than %d MB", ErrUndecodable, maxOriginal>>20)
	}
	return data, nil
}

func (s *Store) resize(ctx context.Context, orig []byte, size int) ([]byte, error) {
	done, err := take(ctx, s.decodes)
	if err != nil {
		return nil, err
	}
	defer done()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(orig))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUndecodable, err)
	}
	if cfg.Width*cfg.Height > maxPixels {
		return nil, fmt.Errorf("%w: too large to resize", ErrUndecodable)
	}
	img, _, err := image.Decode(bytes.NewReader(orig))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUndecodable, err)
	}
	b := img.Bounds()
	scale := min(float64(size)/float64(max(b.Dx(), b.Dy())), 1)
	dst := image.NewRGBA(image.Rect(0, 0, max(int(float64(b.Dx())*scale), 1), max(int(float64(b.Dy())*scale), 1)))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// keep writes a file whole (aside, synced, then renamed in place), and holds the folder to its
// budget.
func (s *Store) keep(path string, data []byte) error {
	tmp, err := os.CreateTemp(s.dir, ".new-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used < 0 {
		s.used = s.countLocked()
	} else {
		s.used += int64(len(data))
	}
	if s.used > s.budget {
		s.evictLocked(s.budget * 8 / 10)
	}
	return nil
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
		n += e.size
	}
	return n
}

// evictLocked removes the oldest files until the folder holds at most target bytes.
func (s *Store) evictLocked(target int64) int64 {
	list := s.list()
	sort.Slice(list, func(i, j int) bool { return list[i].mod.Before(list[j].mod) })
	var total, freed int64
	for _, e := range list {
		total += e.size
	}
	for _, e := range list {
		if total <= target {
			break
		}
		if os.Remove(e.path) == nil {
			total -= e.size
			freed += e.size
		}
	}
	s.used = total
	return freed
}

// Trim empties the folder, for the disk guard; it says how many bytes that freed.
func (s *Store) Trim() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.evictLocked(0)
}
