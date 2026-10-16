package thumbs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func pngData(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		img.Set(x, x%h, color.RGBA{uint8(x), 100, 200, 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newStore(t *testing.T, budget int64) *Store {
	return New(t.TempDir(), budget, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func source(data []byte, opens *atomic.Int32) func(context.Context) (io.ReadCloser, error) {
	return func(context.Context) (io.ReadCloser, error) {
		opens.Add(1)
		time.Sleep(20 * time.Millisecond) // Drive takes a moment
		return io.NopCloser(bytes.NewReader(data)), nil
	}
}

func TestSize(t *testing.T) {
	for n, want := range map[int]int{0: 0, -1: 300, 1: 96, 96: 96, 97: 256, 300: 300, 512: 512, 601: 1024, 5000: 1024} {
		if got := Size(n); got != want {
			t.Errorf("Size(%d) = %d, want %d", n, got, want)
		}
	}
}

// The same cover asked for at once is made once, and every answer is whole (review #157).
func TestOnceForMany(t *testing.T) {
	s := newStore(t, 1<<30)
	orig := pngData(t, 800, 800)
	var opens atomic.Int32
	var wg sync.WaitGroup
	got := make([][]byte, 50)
	for i := range got {
		wg.Go(func() {
			data, err := s.Get(context.Background(), "abc", 300, source(orig, &opens))
			if err != nil {
				t.Error(err)
			}
			got[i] = data
		})
	}
	wg.Wait()
	if n := opens.Load(); n != 1 {
		t.Fatalf("made %d times", n)
	}
	for _, d := range got {
		if !bytes.Equal(d, got[0]) || len(d) == 0 {
			t.Fatal("answers differ")
		}
	}
	if cfg, err := jpegConfig(got[0]); err != nil || cfg.Width != 300 {
		t.Fatalf("%+v %v", cfg, err)
	}
	// From the folder after that.
	if _, err := s.Get(context.Background(), "abc", 300, source(orig, &opens)); err != nil || opens.Load() != 1 {
		t.Fatalf("%v, %d", err, opens.Load())
	}
}

func jpegConfig(b []byte) (image.Config, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	return cfg, err
}

// A file that cannot be written is served all the same and leaves nothing behind.
func TestWriteFailureLeavesNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	s := newStore(t, 1<<30)
	os.Chmod(s.dir, 0o500)
	defer os.Chmod(s.dir, 0o700)
	var opens atomic.Int32
	data, err := s.Get(context.Background(), "abc", 96, source(pngData(t, 200, 200), &opens))
	if err != nil || len(data) == 0 {
		t.Fatal(err)
	}
	if des, _ := os.ReadDir(s.dir); len(des) != 0 {
		t.Fatalf("left %v", des)
	}
}

// What is no image is not fetched again for a while; the original is still served.
func TestUndecodable(t *testing.T) {
	s := newStore(t, 1<<30)
	var opens atomic.Int32
	junk := []byte("not an image at all")
	for range 3 {
		if _, err := s.Get(context.Background(), "bad", 300, source(junk, &opens)); !errors.Is(err, ErrUndecodable) {
			t.Fatalf("junk: %v", err)
		}
	}
	if n := opens.Load(); n != 1 {
		t.Fatalf("fetched %d times", n)
	}
	if data, err := s.Get(context.Background(), "bad", 0, source(junk, &opens)); err != nil || !bytes.Equal(data, junk) {
		t.Fatalf("original: %v", err)
	}
}

// The folder is held to its budget, the oldest files going first; Trim empties it.
func TestBudget(t *testing.T) {
	orig := pngData(t, 600, 600)
	s := newStore(t, 1<<30)
	var opens atomic.Int32
	one, _ := s.Get(context.Background(), "size", 600, source(orig, &opens))
	budget := int64(len(one))*3 + 10
	s = newStore(t, budget)
	for i, sha := range []string{"a", "b", "c", "d", "e"} {
		if _, err := s.Get(context.Background(), sha, 600, source(orig, &opens)); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(filepath.Join(s.dir, sha+"-600.jpg"), time.Now(), time.Now().Add(time.Duration(i)*time.Second))
	}
	var total int64
	des, _ := os.ReadDir(s.dir)
	for _, de := range des {
		info, _ := de.Info()
		total += info.Size()
	}
	if total > budget {
		t.Fatalf("%d bytes in a budget of %d", total, budget)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "e-600.jpg")); err != nil {
		t.Fatal("the newest went")
	}
	if freed := s.Trim(); freed != total {
		t.Fatalf("trim freed %d of %d", freed, total)
	}
}

// Only a few originals are read at once.
func TestFetchesAtOnce(t *testing.T) {
	s := newStore(t, 1<<30)
	orig := pngData(t, 50, 50)
	var now, most atomic.Int32
	open := func(context.Context) (io.ReadCloser, error) {
		n := now.Add(1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		now.Add(-1)
		return io.NopCloser(bytes.NewReader(orig)), nil
	}
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Go(func() { s.Get(context.Background(), string(rune('a'+i)), 96, open) })
	}
	wg.Wait()
	if m := most.Load(); m > int32(cap(s.fetches)) {
		t.Fatalf("%d at once", m)
	}
}

// Originals wait for their turn unread (review #176): with resizing slow, only a few are read and
// held at once; a cover nobody waits for any more stops waiting for its turn.
func TestOriginalsWaitUnread(t *testing.T) {
	s := newStore(t, 1<<30)
	orig := pngData(t, 50, 50)
	for range cap(s.decodes) { // resizing is slow
		s.decodes <- struct{}{}
	}
	var held atomic.Int32 // originals read whole
	open := func(ctx context.Context) (io.ReadCloser, error) {
		select { // as Drive does: given up, it stops
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		held.Add(1)
		return io.NopCloser(bytes.NewReader(orig)), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Go(func() { s.Get(ctx, string(rune('a'+i)), 96, open) })
	}
	time.Sleep(300 * time.Millisecond)
	if n := held.Load(); n > int32(cap(s.jobs)) {
		t.Fatalf("%d originals held while %d can be resized", n, 3)
	}
	cancel()
	wg.Wait()
	for range cap(s.decodes) {
		<-s.decodes
	}
	time.Sleep(200 * time.Millisecond)
	s.mu.Lock()
	calls := len(s.calls)
	s.mu.Unlock()
	if n := held.Load(); n > int32(cap(s.jobs)) || calls != 0 {
		t.Fatalf("covers nobody waits for went on: %d read, %d being made", n, calls)
	}
	// Asked again: made.
	if data, err := s.Get(context.Background(), "a", 96, open); err != nil || len(data) == 0 {
		t.Fatal(err)
	}
}

// A gallery picture's viewing copy: made by the PictureFunc from the original copied aside, once,
// and kept; one it cannot make is undecodable, with nothing left aside; without one, decoded here.
// A copy a stop left half made goes when the folder is opened again.
func TestViewingCopy(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1<<30)
	orig := pngData(t, 120, 40)
	var made atomic.Int32
	s.SetPictures(func(ctx context.Context, src, dst string, longest int) error {
		made.Add(1)
		got, err := os.ReadFile(src)
		if err != nil || !bytes.Equal(got, orig) || longest != ViewSize {
			return errors.New("not the original")
		}
		return os.WriteFile(dst, []byte("view of "+filepath.Base(filepath.Dir(src))[:6]), 0o644)
	})
	var opens atomic.Int32
	for range 2 {
		data, err := s.Get(ctx, "pic", ViewSize, source(orig, &opens))
		if err != nil || string(data) != "view of .view-" {
			t.Fatalf("%q %v", data, err)
		}
	}
	if made.Load() != 1 || opens.Load() != 1 {
		t.Fatalf("made %d times, %d opens", made.Load(), opens.Load())
	}
	if _, err := os.Stat(filepath.Join(s.dir, "pic-view.jpg")); err != nil {
		t.Fatal(err)
	}
	s.SetPictures(func(ctx context.Context, src, dst string, longest int) error {
		return errors.New("ffmpeg: invalid data")
	})
	if _, err := s.Get(ctx, "bad", ViewSize, source([]byte("junk"), &opens)); !errors.Is(err, ErrUndecodable) {
		t.Fatal(err)
	}
	des, _ := os.ReadDir(s.dir)
	for _, de := range des {
		if de.IsDir() {
			t.Fatalf("%s left", de.Name())
		}
	}

	s.SetPictures(nil)
	data, err := s.Get(ctx, "wide", ViewSize, source(pngData(t, ViewSize+200, 50), &opens))
	if err != nil {
		t.Fatal(err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != "jpeg" || cfg.Width != ViewSize {
		t.Fatalf("decoded here: %s %d %v", format, cfg.Width, err)
	}

	os.Mkdir(filepath.Join(s.dir, ".view-123"), 0o700)
	os.WriteFile(filepath.Join(s.dir, ".view-123", "original"), orig, 0o644)
	New(s.dir, 1<<30, s.log)
	if _, err := os.Stat(filepath.Join(s.dir, ".view-123")); !os.IsNotExist(err) {
		t.Fatal("a half made copy stayed:", err)
	}
}

// A gallery picture's sizes (review #248): made by the PictureFunc from the original at the size
// asked for, not by way of a viewing copy. Of those waiting for their turn, the one looked at goes
// first, then those for a page, then those made ahead, each the longest waiting first; one asked
// for again by someone looking at it moves up. Its viewing copy here, a size is resized from it;
// without a PictureFunc, too.
func TestPictureSizes(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1<<30)
	s.reads = newGate(10) // the order is the makes' here
	pics := map[string][]byte{}
	for i, name := range []string{"a", "b", "c", "p", "l"} {
		pics[name] = pngData(t, 30+i, 10)
	}
	var mu sync.Mutex
	var order []string
	longest := map[string]int{}
	hold := make(chan struct{})
	s.SetPictures(func(ctx context.Context, src, dst string, n int) error {
		got, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		mu.Lock()
		for name, p := range pics {
			if bytes.Equal(p, got) {
				order = append(order, name)
				longest[name] = n
			}
		}
		first := len(order) == 1
		mu.Unlock()
		if first {
			<-hold
		}
		return os.WriteFile(dst, got, 0o644) // a PNG: as good as a JPEG to resize from
	})
	opens := map[string]*atomic.Int32{}
	for name := range pics {
		opens[name] = &atomic.Int32{}
	}
	until := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("never ", what)
			}
		}
	}
	queued := func(n int) func() bool {
		return func() bool {
			s.makes.mu.Lock()
			defer s.makes.mu.Unlock()
			return len(s.makes.waiting) == n
		}
	}
	var wg sync.WaitGroup
	ask := func(ctx context.Context, name string, size int) {
		wg.Go(func() {
			if _, err := s.Picture(ctx, name, size, source(pics[name], opens[name])); err != nil {
				t.Error(name, err)
			}
		})
	}
	ask(ctx, "a", 256)
	until("made", func() bool { mu.Lock(); defer mu.Unlock(); return len(order) == 1 })
	ask(Ahead(ctx), "b", 256)
	until("b waits", queued(1))
	ask(Ahead(ctx), "c", 256)
	until("c waits", queued(2))
	ask(ctx, "p", 96)
	until("p waits", queued(3))
	ask(Looking(ctx), "l", ViewSize)
	until("l waits", queued(4))
	ask(Looking(ctx), "c", 256) // looked at now: it waited longer than l
	until("c asked again", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.calls["c-256.jpg"].waiting == 2 })
	close(hold)
	wg.Wait()
	if fmt.Sprint(order) != "[a c l p b]" {
		t.Fatalf("made in order %v", order)
	}
	if fmt.Sprint(longest) != "map[a:256 b:256 c:256 l:4096 p:96]" || opens["c"].Load() != 1 {
		t.Fatalf("made at %v; c read %d times", longest, opens["c"].Load())
	}
	if _, err := os.Stat(filepath.Join(s.dir, "a-256.jpg")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "a-view.jpg")); !os.IsNotExist(err) {
		t.Fatal("a viewing copy made on the way:", err)
	}

	// Its viewing copy here: resized from it, the original not read.
	data, err := s.Picture(ctx, "l", 96, source(pics["l"], opens["l"]))
	if cfg, format, _ := image.DecodeConfig(bytes.NewReader(data)); err != nil || format != "jpeg" || cfg.Width != 34 || len(order) != 5 || opens["l"].Load() != 1 {
		t.Fatalf("%s %d %v; made %v, read %d", format, cfg.Width, err, order, opens["l"].Load())
	}
	// Without FFmpeg: from the viewing copy decoded here.
	s.SetPictures(nil)
	var n atomic.Int32
	data, err = s.Picture(ctx, "wide", 256, source(pngData(t, 600, 60), &n))
	if cfg, _, _ := image.DecodeConfig(bytes.NewReader(data)); err != nil || cfg.Width != 256 || n.Load() != 1 {
		t.Fatalf("%d %v, read %d", cfg.Width, err, n.Load())
	}
}

// The next picture's original is read while one is being made; no more than two are held.
func TestPictureReadWhileMade(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1<<30)
	hold := make(chan struct{})
	var made atomic.Int32
	s.SetPictures(func(ctx context.Context, src, dst string, n int) error {
		if made.Add(1) == 1 {
			<-hold
		}
		return os.WriteFile(dst, []byte("made"), 0o644)
	})
	var opens [3]atomic.Int32
	var wg sync.WaitGroup
	for i := range opens {
		wg.Go(func() { s.Picture(ctx, string(rune('a'+i)), 256, source(pngData(t, 20, 20), &opens[i])) })
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if opens[0].Load() != 1 || opens[1].Load() != 1 || opens[2].Load() != 0 {
		t.Fatalf("read %d %d %d while the first is made", opens[0].Load(), opens[1].Load(), opens[2].Load())
	}
	close(hold)
	wg.Wait()
	if made.Load() != 3 {
		t.Fatal(made.Load())
	}
}
