package lite

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/ffmpeg"
)

// source serves originals from memory, each with padding the encoder drops (so what it makes is
// smaller); a song with a gate sends its first half, then waits for the gate to close (or the copy
// to be cancelled).
type source struct {
	mu     sync.Mutex
	files  map[string][]byte
	gates  map[string]chan struct{}
	fail   map[string]error // fails after the first half
	copies atomic.Int32
	dir    string
}

func newSource(t *testing.T) *source {
	return &source{files: map[string][]byte{}, gates: map[string]chan struct{}{}, fail: map[string]error{}, dir: t.TempDir()}
}

const padding = "~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~"

func (s *source) add(id string, data []byte) Song {
	s.mu.Lock()
	defer s.mu.Unlock()
	data = append(data, padding...)
	s.files[id] = data
	return Song{DriveID: id, SHA256: "sha-" + id, Size: int64(len(data)), Format: "flac", Channels: 2}
}

// raw adds a song as it is, without padding (real files, for FFmpeg).
func (s *source) raw(id string, data []byte) Song {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[id] = data
	return Song{DriveID: id, SHA256: "sha-" + id, Size: int64(len(data)), Format: "flac", Channels: 2}
}

func (s *source) gate(id string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := make(chan struct{})
	s.gates[id] = g
	return g
}

func (s *source) Copy(ctx context.Context, w io.Writer, id string, size int64) error {
	s.copies.Add(1)
	s.mu.Lock()
	data, gate, fail := s.files[id], s.gates[id], s.fail[id]
	s.mu.Unlock()
	half := len(data) / 2
	if _, err := w.Write(data[:half]); err != nil {
		return err
	}
	if fail != nil {
		return fail
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	_, err := w.Write(data[half:])
	return err
}

func (s *source) Hold(id string) (string, func(), bool) {
	s.mu.Lock()
	data := s.files[id]
	s.mu.Unlock()
	p := filepath.Join(s.dir, id)
	if os.WriteFile(p, data, 0o600) != nil {
		return "", nil, false
	}
	return p, func() {}, true
}

// encoder "encodes" by prefixing what it reads, less the padding; it gives up on inputs starting
// with "bad" once it read that much, the rest unread (as FFmpeg does data it cannot make out), and
// keeps the padding of those starting with "big" (they come out larger).
type encoder struct {
	runs      atomic.Int32
	cancelled atomic.Int32
}

func (e *encoder) Opus(ctx context.Context, src string, in io.Reader, dst string, kbps int, downmix bool) error {
	e.runs.Add(1)
	if in == nil {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		in = f
	}
	done := make(chan struct{})
	var data []byte
	var err error
	go func() {
		defer close(done)
		head := make([]byte, 3)
		n, err1 := io.ReadFull(in, head)
		data, err = head[:n], err1
		if err != nil || string(data) == "bad" {
			return
		}
		rest, err2 := io.ReadAll(in)
		data, err = append(data, rest...), err2
	}()
	select {
	case <-done:
	case <-ctx.Done():
		e.cancelled.Add(1)
		return ctx.Err()
	}
	if bytes.HasPrefix(data, []byte("bad")) {
		return errors.New("cannot decode")
	}
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(data, []byte("big")) {
		data = bytes.TrimRight(data, "~")
	}
	return os.WriteFile(dst, append([]byte("opus:"), data...), 0o600)
}

func store(t *testing.T, budget int64, enc Encoder, src Source) *Store {
	t.Helper()
	s, err := New(t.TempDir(), budget, enc, src, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func read(t *testing.T, f *os.File, err error) string {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// got is song o's lite file from s, read.
func got(t *testing.T, s *Store, ctx context.Context, o Song) string {
	t.Helper()
	f, err := s.Get(ctx, o)
	return read(t, f, err)
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !ok(); {
		if time.Now().After(end) {
			t.Fatal(what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestShrinks(t *testing.T) {
	for _, c := range []struct {
		format  string
		bitrate int
		want    bool
	}{{"flac", 900_000, true}, {"flac", 0, true}, {"flac", 108_521, false}, {"flac", 160_000, false}, {"mp3", 128_000, false},
		{"mp3", 160_000, false}, {"mp3", 320_000, true}, {"m4a", 256_000, true}, {"ogg", 0, false}} {
		if got := Shrinks(c.format, c.bitrate); got != c.want {
			t.Errorf("Shrinks(%s, %d) = %v", c.format, c.bitrate, got)
		}
	}
}

func TestMadeOnceAndKept(t *testing.T) {
	src, enc := newSource(t), &encoder{}
	s := store(t, 1<<20, enc, src)
	song := src.add("a", []byte("first song"))
	gate := src.gate("a")
	var wg sync.WaitGroup
	outs := make([]string, 3)
	for i := range outs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, err := s.Get(context.Background(), song)
			outs[i] = read(t, f, err)
		}()
	}
	eventually(t, "the copy started", func() bool { return src.copies.Load() == 1 })
	close(gate)
	wg.Wait()
	for _, g := range outs {
		if g != "opus:first song" {
			t.Fatalf("got %q", g)
		}
	}
	if got(t, s, context.Background(), song); enc.runs.Load() != 1 || src.copies.Load() != 1 {
		t.Fatalf("made %d times, read %d times", enc.runs.Load(), src.copies.Load())
	}
	// Kept across a restart.
	again, _ := New(s.dir, 1<<20, enc, src, s.log)
	if g := got(t, again, context.Background(), song); g != "opus:first song" || enc.runs.Load() != 1 {
		t.Fatalf("after a restart: %q, made %d times", g, enc.runs.Load())
	}
}

func TestSkippedSongStops(t *testing.T) {
	src, enc := newSource(t), &encoder{}
	s := store(t, 1<<20, enc, src)
	a, b := src.add("a", []byte("song a")), src.add("b", []byte("song b"))
	src.gate("a")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { _, err := s.Get(ctx, a); done <- err }()
	eventually(t, "a started", func() bool { return src.copies.Load() == 1 })
	cancel() // the request goes; a is still the song played, so it goes on
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if enc.cancelled.Load() != 0 {
		t.Fatal("a stopped while it was still the song played")
	}
	if g := got(t, s, context.Background(), b); g != "opus:song b" { // another song is played: a stops
		t.Fatalf("b: %q", g)
	}
	eventually(t, "a stopped", func() bool { return enc.cancelled.Load() == 1 })
	if _, err := os.Stat(s.path(a.SHA256)); !os.IsNotExist(err) {
		t.Fatal("a skipped song was kept")
	}
	if names, _ := os.ReadDir(s.dir); len(names) != 1 {
		t.Fatalf("left in the folder: %v", names)
	}
}

func TestPrefetch(t *testing.T) {
	src, enc := newSource(t), &encoder{}
	s := store(t, 1<<20, enc, src)
	a, b, c := src.add("a", []byte("song a")), src.add("b", []byte("song b")), src.add("c", []byte("song c"))
	s.Prefetch(a)
	eventually(t, "a made ahead", func() bool { _, err := os.Stat(s.path(a.SHA256)); return err == nil })
	src.gate("b")
	s.Prefetch(b)
	eventually(t, "b started", func() bool { return src.copies.Load() == 2 })
	s.Prefetch(c) // the next song changed: b stops
	eventually(t, "b stopped", func() bool { return enc.cancelled.Load() == 1 })
	eventually(t, "c made ahead", func() bool { _, err := os.Stat(s.path(c.SHA256)); return err == nil })
	src.gate("a")
	os.Remove(s.path(a.SHA256))
	s.Prefetch(a)
	eventually(t, "a started again", func() bool { return src.copies.Load() == 4 })
	s.Unprefetch()
	eventually(t, "a stopped", func() bool { return enc.cancelled.Load() == 2 })
	s.SetLean(true)
	s.Prefetch(b)
	time.Sleep(20 * time.Millisecond)
	if src.copies.Load() != 4 {
		t.Fatal("made ahead on a low disk")
	}
}

func TestPlayedWhilePrefetchedGoesOn(t *testing.T) {
	src, enc := newSource(t), &encoder{}
	s := store(t, 1<<20, enc, src)
	a, b := src.add("a", []byte("song a")), src.add("b", []byte("song b"))
	gate := src.gate("a")
	s.Prefetch(a)
	eventually(t, "a started", func() bool { return src.copies.Load() == 1 })
	done := make(chan string)
	go func() { f, err := s.Get(context.Background(), a); done <- read(t, f, err) }()
	time.Sleep(20 * time.Millisecond)
	s.Prefetch(b) // a is played now, and waited for
	time.Sleep(20 * time.Millisecond)
	close(gate)
	if g := <-done; g != "opus:song a" || enc.cancelled.Load() != 0 || src.copies.Load() != 2 {
		t.Fatalf("got %q, %d stopped, %d copies", g, enc.cancelled.Load(), src.copies.Load())
	}
}

func TestSourceFailure(t *testing.T) {
	src, enc := newSource(t), &encoder{}
	s := store(t, 1<<20, enc, src)
	a := src.add("a", []byte("song a"))
	drive := errors.New("drive keeps failing")
	src.fail["a"] = drive
	_, err := s.Get(context.Background(), a)
	var se *SourceError
	if !errors.As(err, &se) || !errors.Is(err, drive) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(s.path(a.SHA256)); !os.IsNotExist(err) {
		t.Fatal("a song cut short was kept")
	}
	delete(src.fail, "a") // Drive back: tried again at once
	if g := got(t, s, context.Background(), a); g != "opus:song a" {
		t.Fatalf("got %q", g)
	}
}

func TestEncodeFailureSentAsIs(t *testing.T) {
	src, enc := newSource(t), &encoder{}
	s := store(t, 1<<20, enc, src)
	a := src.add("a", []byte("bad song"))
	if _, err := s.Get(context.Background(), a); err == nil || errors.Is(err, ErrFailed) {
		t.Fatalf("got %v", err)
	}
	if _, err := s.Get(context.Background(), a); !errors.Is(err, ErrFailed) || enc.runs.Load() != 1 {
		t.Fatalf("again: %v, made %d times", err, enc.runs.Load())
	}
	s.Prefetch(a)
	time.Sleep(20 * time.Millisecond)
	if enc.runs.Load() != 1 {
		t.Fatal("a song that failed was tried again ahead")
	}
	s.mu.Lock()
	s.bad[a.SHA256] = time.Now().Add(-time.Second) // a while later
	s.mu.Unlock()
	if _, err := s.Get(context.Background(), a); errors.Is(err, ErrFailed) || enc.runs.Load() != 2 {
		t.Fatalf("later: %v, made %d times", err, enc.runs.Load())
	}
}

// FFmpeg giving up early on what it cannot make out, while the original waits for more of it (Drive
// slow), stops the copy at once: the song is sent as it is, and its turn is free (review #207).
func TestEncoderStopsEarly(t *testing.T) {
	src, enc := newSource(t), &encoder{}
	s := store(t, 1<<20, enc, src)
	a := src.add("a", []byte("bad song"))
	src.gate("a") // the rest never comes
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.Get(ctx, a)
	var se *SourceError
	if err == nil || ctx.Err() != nil || errors.As(err, &se) || errors.Is(err, ErrFailed) {
		t.Fatalf("got %v (%v)", err, ctx.Err())
	}
	if _, err := s.Get(ctx, a); !errors.Is(err, ErrFailed) {
		t.Fatalf("again: %v", err)
	}
	eventually(t, "its turn is free", func() bool { return len(s.turns) == 0 })
}

// What the original does wrong is still its own: a copy failing on its own, before FFmpeg stopped,
// is the original's fault, not the song's (TestSourceFailure); one the request ended is the request's.
func TestEncodeEndsWithRequest(t *testing.T) {
	src, enc := newSource(t), &encoder{}
	s := store(t, 1<<20, enc, src)
	a, b := src.add("a", []byte("song a")), src.add("b", []byte("song b"))
	src.gate("a")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { _, err := s.Get(ctx, a); done <- err }()
	eventually(t, "a started", func() bool { return src.copies.Load() == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	got(t, s, context.Background(), b) // a stops: not counted as a song that failed
	eventually(t, "a stopped", func() bool { return enc.cancelled.Load() == 1 })
	s.mu.Lock()
	_, bad := s.bad[a.SHA256]
	s.mu.Unlock()
	if bad {
		t.Fatal("a song stopped was taken for one that failed")
	}
}

// A song that comes out no smaller re-encoded is sent as it is, now and later, and is not made
// ahead again (review #212).
func TestNotSmallerSentAsIs(t *testing.T) {
	src, enc := newSource(t), &encoder{}
	s := store(t, 1<<20, enc, src)
	a := src.add("a", []byte("big song"))
	for i := 0; i < 2; i++ {
		if _, err := s.Get(context.Background(), a); !errors.Is(err, ErrNotSmaller) {
			t.Fatalf("%d: %v", i, err)
		}
	}
	s.Prefetch(a)
	time.Sleep(20 * time.Millisecond)
	if enc.runs.Load() != 1 || s.Has(a.SHA256) {
		t.Fatalf("made %d times; kept: %v", enc.runs.Load(), s.Has(a.SHA256))
	}
	if names, _ := os.ReadDir(s.dir); len(names) != 0 {
		t.Fatalf("left in the folder: %v", names)
	}
	b := src.add("b", []byte("song b")) // smaller: kept
	if got(t, s, context.Background(), b); !s.Has(b.SHA256) {
		t.Fatal("not kept")
	}
}

// A song played as it is (lite mode turned off, a song lite mode sends as it is) is the song played:
// the one being made before it stops, unless waited for or preloaded (review #208).
func TestPlayedAsItIsStopsTheOneBefore(t *testing.T) {
	src, enc := newSource(t), &encoder{}
	s := store(t, 1<<20, enc, src)
	a, n := src.add("a", []byte("song a")), src.add("n", []byte("song n"))
	src.gate("a")
	gate := src.gate("n")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { _, err := s.Get(ctx, a); done <- err }()
	eventually(t, "a started", func() bool { return src.copies.Load() == 1 })
	s.Prefetch(n) // the next song, preloaded
	eventually(t, "n started", func() bool { return src.copies.Load() == 2 })
	s.Played("sha-b") // b plays as it is, while a's request still waits: a goes on
	time.Sleep(20 * time.Millisecond)
	if enc.cancelled.Load() != 0 {
		t.Fatal("a stopped while waited for")
	}
	cancel() // a's request goes too: a stops, n (next) goes on
	<-done
	eventually(t, "a stopped", func() bool { return enc.cancelled.Load() == 1 })
	s.Played("sha-b") // the same again: nothing more
	close(gate)
	eventually(t, "n made", func() bool { return s.Has(n.SHA256) })
	if enc.cancelled.Load() != 1 {
		t.Fatalf("%d stopped", enc.cancelled.Load())
	}
	// A request of the same song going and coming back (a seek) leaves it be.
	c := src.add("c", []byte("song c"))
	gc := src.gate("c")
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() { s.Get(ctx2, c) }()
	eventually(t, "c started", func() bool { return src.copies.Load() == 3 })
	cancel2()
	time.Sleep(20 * time.Millisecond)
	close(gc)
	if g := got(t, s, context.Background(), c); g != "opus:song c" || enc.cancelled.Load() != 1 || src.copies.Load() != 3 {
		t.Fatalf("got %q, %d stopped, %d copies", g, enc.cancelled.Load(), src.copies.Load())
	}
}

func TestBudgetKeepsSongsPlayedLast(t *testing.T) {
	src, enc := newSource(t), &encoder{}
	s := store(t, 3*(5+100), enc, src) // three files of 105 bytes
	var songs []Song
	for _, id := range []string{"a", "b", "c", "d"} {
		songs = append(songs, src.add(id, bytes.Repeat([]byte(id), 100)))
	}
	for i, song := range songs[:3] {
		got(t, s, context.Background(), song)
		os.Chtimes(s.path(song.SHA256), time.Now().Add(time.Duration(i-10)*time.Minute), time.Now().Add(time.Duration(i-10)*time.Minute))
	}
	got(t, s, context.Background(), songs[0]) // a played again: b is the oldest now
	got(t, s, context.Background(), songs[3]) // over the budget: down to 80%, oldest first
	have := ""
	for _, song := range songs {
		if _, err := os.Stat(s.path(song.SHA256)); err == nil {
			have += song.DriveID
		}
	}
	if have != "ad" || s.Used() != 210 {
		t.Fatalf("kept %q, %d bytes", have, s.Used())
	}
	if freed := s.Trim(); freed != 105 || s.Used() != 105 { // the song played last stays
		t.Fatalf("trim freed %d, %d left", freed, s.Used())
	}
}

func TestUnavailableWithoutEncoder(t *testing.T) {
	src := newSource(t)
	s := store(t, 1<<20, nil, src)
	a := src.add("a", []byte("song a"))
	if s.Available() {
		t.Fatal("available without an encoder")
	}
	if _, err := s.Get(context.Background(), a); !errors.Is(err, ErrFailed) {
		t.Fatal(err)
	}
	s.Prefetch(a)
	if src.copies.Load() != 0 {
		t.Fatal("read an original")
	}
}

// With FFmpeg: a FLAC original read from a pipe and an M4A one read from the cached copy become
// Ogg Opus as long as the original, the same bytes each time.
func TestFFmpeg(t *testing.T) {
	ff := withFFmpeg(t)
	ctx := context.Background()
	dir := t.TempDir()
	src := newSource(t)
	var songs []Song
	for _, f := range []struct{ name, codec, format string }{{"a.flac", "flac", "flac"}, {"b.m4a", "aac", "m4a"}} {
		p := filepath.Join(dir, f.name)
		makeTone(t, ff, p, f.codec)
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		song := src.raw(f.name, data)
		song.Format, song.Channels = f.format, 6
		songs = append(songs, song)
	}
	s := store(t, 1<<30, ff, src)
	for _, song := range songs {
		out := filepath.Join(dir, song.DriveID+".out")
		b := got(t, s, ctx, song)
		if !strings.HasPrefix(b, "OggS") || !strings.Contains(b[:200], "OpusHead") {
			t.Fatalf("%s: not Ogg Opus", song.DriveID)
		}
		os.WriteFile(out, []byte(b), 0o600)
		st, err := ff.Probe(ctx, out)
		if err != nil || st.Codec != "opus" || st.Channels != 2 || st.SampleRate != 48000 {
			t.Fatalf("%s: %+v %v", song.DriveID, st, err)
		}
		if secs := float64(st.Samples) / 48000; secs < 2.95 || secs > 3.05 {
			t.Fatalf("%s: %.3f s", song.DriveID, secs)
		}
		if len(st.Tags) > 1 { // the encoder's own, at most
			t.Fatalf("%s: tags %v", song.DriveID, st.Tags)
		}
		s.Trim()
		os.Remove(s.path(song.SHA256))
		if again := got(t, s, ctx, song); again != b {
			t.Fatalf("%s: made again, different bytes", song.DriveID)
		}
	}
}

// With FFmpeg: a FLAC of a low bitrate (a plain tone, stereo) comes out larger as Ogg Opus, and is
// sent as it is (review #212); data FFmpeg cannot make out, the rest of it slow to come, ends at once
// as a song that failed (review #207).
func TestFFmpegEdges(t *testing.T) {
	ff := withFFmpeg(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	src := newSource(t)
	p := filepath.Join(t.TempDir(), "low.flac")
	out, err := exec.Command(ff.Path(), "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=20",
		"-ac", "2", "-c:a", "flac", p).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	data, _ := os.ReadFile(p)
	low := src.raw("low", data)
	s := store(t, 1<<30, ff, src)
	if rate := len(data) * 8 / 20; rate > keepRate {
		t.Fatalf("not a low bitrate: %d", rate)
	}
	if _, err := s.Get(ctx, low); !errors.Is(err, ErrNotSmaller) || s.Has(low.SHA256) {
		t.Fatalf("got %v; kept: %v", err, s.Has(low.SHA256))
	}

	junk := append([]byte("fLaC"), make([]byte, 128<<10)...) // FFmpeg gives up on it (exit status 183)
	bad := src.raw("bad", junk)                              // 64 KiB of it, then nothing more
	src.gate("bad")
	start := time.Now()
	_, err = s.Get(ctx, bad)
	var se *SourceError
	if err == nil || ctx.Err() != nil || errors.As(err, &se) || errors.Is(err, ErrFailed) {
		t.Fatalf("got %v (%v)", err, ctx.Err())
	}
	if _, err := s.Get(ctx, bad); !errors.Is(err, ErrFailed) {
		t.Fatalf("again: %v", err)
	}
	if len(s.turns) != 0 || time.Since(start) > 10*time.Second {
		t.Fatalf("%d turns held; took %v", len(s.turns), time.Since(start))
	}
}

func withFFmpeg(t *testing.T) *ffmpeg.Tool {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	ff := ffmpeg.Find(filepath.Join(filepath.Dir(file), "..", "..", "..", "var")) // tools/ is next to the data directory
	if ff == nil || !ff.CanOpus() {
		t.Skip("no ffmpeg with libopus")
	}
	return ff
}

// makeTone makes a three-second 5.1 test file with tags (lite files have none).
func makeTone(t *testing.T, ff *ffmpeg.Tool, dst, codec string) {
	t.Helper()
	out, err := exec.Command(ff.Path(), "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=3",
		"-af", "pan=5.1|c0=c0|c1=c0|c2=c0|c3=c0|c4=c0|c5=c0", "-c:a", codec, "-metadata", "title=Tone", "-metadata", "album=Test", dst).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
}
