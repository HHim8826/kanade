package stream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSource serves a byte slice with an artificial first-byte delay, like Drive.
type fakeSource struct {
	data  []byte
	delay time.Duration
	calls atomic.Int64
}

func (s *fakeSource) OpenRange(ctx context.Context, _ string, start, end int64) (*http.Response, error) {
	s.calls.Add(1)
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if end < 0 {
		end = int64(len(s.data)) - 1
	}
	return &http.Response{StatusCode: 206, Body: io.NopCloser(bytes.NewReader(s.data[start : end+1]))}, nil
}

func newCache(t *testing.T, size int, budget int64, delay time.Duration) (*Cache, *fakeSource) {
	t.Helper()
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(rand.IntN(256))
	}
	src := &fakeSource{data: data, delay: delay}
	c, err := NewCache(src, t.TempDir(), budget, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return c, src
}

func get(c *Cache, id string, size int64, rangeHeader string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/x", nil)
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	rec := httptest.NewRecorder()
	c.Serve(rec, req, id, size, "audio/flac", "abc")
	return rec
}

func TestRangesReturnExactBytes(t *testing.T) {
	const size = 5*blockSize + 1234
	c, src := newCache(t, size, 64<<20, 0)
	cases := []struct {
		hdr        string
		start, end int
		code       int
	}{
		{"", 0, size - 1, 200},
		{"bytes=0-0", 0, 0, 206},
		{"bytes=100-", 100, size - 1, 206},
		{"bytes=-500", size - 500, size - 1, 206},
		{fmt.Sprintf("bytes=%d-%d", blockSize-10, 3*blockSize+10), blockSize - 10, 3*blockSize + 10, 206},
		{"bytes=0-99999999", 0, size - 1, 206},
	}
	for _, tc := range cases {
		rec := get(c, "f", size, tc.hdr)
		if rec.Code != tc.code {
			t.Fatalf("%q: status %d", tc.hdr, rec.Code)
		}
		if !bytes.Equal(rec.Body.Bytes(), src.data[tc.start:tc.end+1]) {
			t.Fatalf("%q: wrong bytes", tc.hdr)
		}
	}
	if rec := get(c, "f", size, fmt.Sprintf("bytes=%d-", size)); rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("past end: %d", rec.Code)
	}
}

func TestConcurrentRequestsShareOneDownload(t *testing.T) {
	const size = 8 * blockSize
	c, src := newCache(t, size, 64<<20, 50*time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rec := get(c, "f", size, "bytes=0-1000"); rec.Code != 206 {
				t.Errorf("status %d", rec.Code)
			}
		}()
	}
	wg.Wait()
	if n := src.calls.Load(); n != 1 {
		t.Fatalf("source requests = %d, want 1", n)
	}
}

func TestFarSeekStartsSecondFillerWithoutRefetching(t *testing.T) {
	const size = 64 * blockSize // 16 MiB
	c, src := newCache(t, size, 64<<20, 20*time.Millisecond)
	get(c, "f", size, "bytes=0-100")
	far := size - blockSize
	if rec := get(c, "f", size, fmt.Sprintf("bytes=%d-%d", far, far+100)); !bytes.Equal(rec.Body.Bytes(), src.data[far:far+101]) {
		t.Fatal("wrong bytes after seek")
	}
	// Let both fillers finish; together they must not fetch more than the file.
	deadline := time.Now().Add(5 * time.Second)
	for c.Stats.SourceBytes.Load() < size && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := c.Stats.SourceBytes.Load(); got != size {
		t.Fatalf("fetched %d bytes for a %d-byte file", got, size)
	}
	if n := src.calls.Load(); n != 2 {
		t.Fatalf("source requests = %d, want 2", n)
	}
	if !bytes.Equal(get(c, "f", size, "").Body.Bytes(), src.data) {
		t.Fatal("full read after fill differs")
	}
}

func TestBudgetEvictsIdleFiles(t *testing.T) {
	const size = 4 * blockSize
	c, src := newCache(t, 3*size, 2*size, 0)
	for _, id := range []string{"a", "b"} {
		get(c, id, size, "")
	}
	c.mu.Lock()
	for _, cf := range c.files {
		cf.lastUse = time.Now().Add(-time.Hour) // both idle
	}
	c.mu.Unlock()
	get(c, "c", size, "")
	c.mu.Lock()
	n := len(c.files)
	c.mu.Unlock()
	if n != 2 {
		t.Fatalf("files in cache = %d, want 2 within budget", n)
	}
	// A file larger than the whole cache is passed straight through.
	rec := get(c, "big", 3*size, "")
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), src.data) || c.Used() > 2*size {
		t.Fatalf("file over budget: %d, %d bytes, used %d", rec.Code, rec.Body.Len(), c.Used())
	}
}

// The budget holds even when the cached files were used moments ago, and with files in use a new
// one is passed through rather than cached.
func TestBudgetIsHard(t *testing.T) {
	const size = 2 * blockSize
	c, src := newCache(t, size, size, 0)
	get(c, "a", size, "")
	rec := get(c, "b", size, "") // a was just played: still dropped, since b does not fit beside it
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), src.data) || c.Used() > size {
		t.Fatalf("recently used: %d, used %d", rec.Code, c.Used())
	}
	held, err := c.acquire("b", size) // b is being played
	if err != nil {
		t.Fatal(err)
	}
	rec = get(c, "c", size, "bytes=10-99")
	if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), src.data[10:100]) || c.Used() > size {
		t.Fatalf("pass-through: %d, used %d", rec.Code, c.Used())
	}
	c.mu.Lock()
	_, cached := c.files["c"]
	c.mu.Unlock()
	if cached {
		t.Fatal("c was cached over the budget")
	}
	c.release(held)
	if err := c.Prefetch("d", size); err != nil || c.Used() > size {
		t.Fatalf("prefetch: %v, used %d", err, c.Used())
	}
	// Low disk: the same limits, and nothing idle is kept.
	c.SetLean(true)
	get(c, "e", size, "")
	if c.Used() > size {
		t.Fatalf("lean: used %d", c.Used())
	}
}

// A file whose Drive content changed is dropped, at once or after its last reader.
func TestForget(t *testing.T) {
	const size = 2 * blockSize
	c, src := newCache(t, size, 2*size, 0)
	get(c, "a", size, "")
	c.Forget("a")
	if c.Used() != 0 {
		t.Fatalf("used %d after forget", c.Used())
	}
	held, _ := c.acquire("a", size)
	c.Forget("a")
	if rec := get(c, "a", size, ""); !bytes.Equal(rec.Body.Bytes(), src.data) || c.Used() != 2*size {
		t.Fatalf("fresh copy beside the old one: used %d", c.Used()) // both counted
	}
	old := held.f.Name()
	c.release(held)
	if c.Used() != size {
		t.Fatalf("old copy kept: used %d", c.Used())
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old file still on disk: %v", err)
	}
}

func TestClientDisconnectStopsWaiting(t *testing.T) {
	const size = 4 * blockSize
	c, _ := newCache(t, size, 64<<20, 2*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/x", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		c.Serve(httptest.NewRecorder(), req, "f", size, "audio/flac", "")
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("request kept waiting after the client left")
	}
}

// Playing, prefetching, trimming and forgetting at once stays within the budget (run with -race).
func TestConcurrentUseKeepsBudget(t *testing.T) {
	const size = blockSize
	c, src := newCache(t, size, 2*size, time.Millisecond)
	var wg sync.WaitGroup
	var over atomic.Bool
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				id := fmt.Sprint((g + i) % 5)
				switch i % 6 {
				case 0:
					c.Prefetch(id, size)
				case 1:
					c.Trim()
				case 2:
					c.Forget(id)
				default:
					if rec := get(c, id, size, "bytes=0-1023"); !bytes.Equal(rec.Body.Bytes(), src.data[:1024]) {
						t.Errorf("wrong bytes for %s: %d", id, rec.Code)
					}
				}
				if c.Used() > 2*size {
					over.Store(true)
				}
			}
		}(g)
	}
	wg.Wait()
	if over.Load() {
		t.Fatal("went over the budget")
	}
}
