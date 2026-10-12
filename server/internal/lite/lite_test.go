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

// source serves originals from memory; a song with a gate sends its first half, then waits for the
// gate to close (or the copy to be cancelled).
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

func (s *source) add(id string, data []byte) Song {
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

// encoder "encodes" by prefixing what it reads; it fails for inputs starting with "bad".
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
	go func() { data, err = io.ReadAll(in); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		e.cancelled.Add(1)
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	if bytes.HasPrefix(data, []byte("bad")) {
		return errors.New("cannot decode")
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
	}{{"flac", 900_000, true}, {"flac", 0, true}, {"mp3", 128_000, false}, {"mp3", 160_000, false}, {"mp3", 320_000, true},
		{"m4a", 256_000, true}, {"ogg", 0, false}} {
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
	_, file, _, _ := runtime.Caller(0)
	ff := ffmpeg.Find(filepath.Join(filepath.Dir(file), "..", "..", "..", "var")) // tools/ is next to the data directory
	if ff == nil || !ff.CanOpus() {
		t.Skip("no ffmpeg with libopus")
	}
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
		song := src.add(f.name, data)
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

// makeTone makes a three-second 5.1 test file with tags (lite files have none).
func makeTone(t *testing.T, ff *ffmpeg.Tool, dst, codec string) {
	t.Helper()
	out, err := exec.Command(ff.Path(), "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=3",
		"-af", "pan=5.1|c0=c0|c1=c0|c2=c0|c3=c0|c4=c0|c5=c0", "-c:a", codec, "-metadata", "title=Tone", "-metadata", "album=Test", dst).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
}
