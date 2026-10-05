package thumbs

import (
	"bytes"
	"context"
	"errors"
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
