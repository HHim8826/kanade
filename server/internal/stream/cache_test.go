package stream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

// A file wholly cached is told of once, and can be held to read its copy: a held copy is not
// evicted, and a partial one cannot be held (review #136).
func TestWholeFilesCanBeHeld(t *testing.T) {
	const size = 3*blockSize + 100
	c, src := newCache(t, 2*size, size+blockSize, 0)
	whole := make(chan string, 4)
	c.OnWhole = func(id string) { whole <- id }
	get(c, "a", size, "bytes=0-99")
	if _, _, ok := c.Hold("b"); ok {
		t.Fatal("held a file never cached")
	}
	select {
	case id := <-whole:
		if id != "a" {
			t.Fatalf("whole: %q", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not told the file is whole")
	}
	path, release, ok := c.Hold("a")
	if !ok {
		t.Fatal("cannot hold a whole file")
	}
	if b, err := os.ReadFile(path); err != nil || !bytes.Equal(b, src.data[:size]) {
		t.Fatalf("held copy: %d bytes, %v", len(b), err)
	}
	// Held, it stays while another file wants the room; once released it can go.
	c.mu.Lock()
	c.files["a"].lastUse = time.Now().Add(-time.Hour)
	c.mu.Unlock()
	if rec := get(c, "b", size, "bytes=0-9"); rec.Code != http.StatusPartialContent {
		t.Fatalf("b: %d", rec.Code)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("held copy gone: %v", err)
	}
	release()
	select {
	case id := <-whole:
		if id != "a" {
			t.Fatalf("told twice, or of %q", id)
		}
		t.Fatal("told of a again")
	case <-time.After(200 * time.Millisecond):
	}
}

// slowBody gives its bytes a little at a time, like a long download.
type slowBody struct {
	ctx  context.Context
	data []byte
}

func (b *slowBody) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		return 0, io.EOF
	}
	select {
	case <-time.After(5 * time.Millisecond):
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	}
	n := copy(p[:min(len(p), 16<<10)], b.data)
	b.data = b.data[n:]
	return n, nil
}

func (b *slowBody) Close() error { return nil }

type slowSource struct{ data []byte }

func (s *slowSource) OpenRange(ctx context.Context, _ string, start, end int64) (*http.Response, error) {
	if end < 0 {
		end = int64(len(s.data)) - 1
	}
	return &http.Response{StatusCode: 206, Body: &slowBody{ctx, s.data[start : end+1]}}, nil
}

// Songs skipped after a few seconds stop downloading (review #153): only the one played last and
// the one prefetched last go on; what the others have stays.
func TestSkippedSongsStopDownloading(t *testing.T) {
	const size = 64 * blockSize
	src := &slowSource{data: make([]byte, size)}
	c, err := NewCache(src, t.TempDir(), 1<<30, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		id := fmt.Sprint("song", i)
		if rec := get(c, id, size, "bytes=0-1000"); rec.Code != 206 {
			t.Fatalf("%s: %d", id, rec.Code)
		}
		c.Prefetch(fmt.Sprint("song", i+1), size)
	}
	c.mu.Lock()
	running := map[string]int{}
	for id, cf := range c.files {
		cf.mu.Lock()
		for _, fl := range cf.fillers {
			if !fl.done {
				running[id]++
			}
		}
		if id != "song9" && id != "song10" && cf.missing == 0 {
			t.Errorf("%s was downloaded whole", id)
		}
		cf.mu.Unlock()
	}
	c.mu.Unlock()
	if len(running) > 2 || (len(running) == 2 && (running["song9"] == 0 || running["song10"] == 0)) {
		t.Fatalf("downloading %v", running)
	}
	// The one playing goes on to the end without a request holding it.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, release, ok := c.Hold("song9"); ok {
			release()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the song playing stopped downloading")
}

// A preloaded song that is no longer next stops downloading (review #193): another one preloaded
// instead, or none.
func TestPreloadNoLongerNextStops(t *testing.T) {
	const size = 64 * blockSize
	src := &slowSource{data: make([]byte, size)}
	c, err := NewCache(src, t.TempDir(), 1<<30, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	running := func(id string) bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		cf := c.files[id]
		if cf == nil {
			return false
		}
		cf.mu.Lock()
		defer cf.mu.Unlock()
		for _, fl := range cf.fillers {
			if !fl.done {
				return true
			}
		}
		return false
	}
	get(c, "a", size, "bytes=0-1000")
	c.Prefetch("b", size)
	if !running("b") {
		t.Fatal("b is not preloaded")
	}
	c.Prefetch("c", size) // c moved before b
	if running("b") || !running("c") {
		t.Fatalf("another next: b %v, c %v", running("b"), running("c"))
	}
	c.Unprefetch() // c removed, and nothing after a
	if _, next := c.Playing(); next != "" || running("c") {
		t.Fatalf("none next: %q, c %v", next, running("c"))
	}
	if !running("a") {
		t.Fatal("the song playing stopped downloading")
	}
}

// A prefetch waits while downloads are running; a request never does.
func TestPrefetchWaitsItsTurn(t *testing.T) {
	const size = 64 * blockSize
	src := &slowSource{data: make([]byte, size)}
	c, _ := NewCache(src, t.TempDir(), 1<<30, slog.New(slog.NewTextHandler(io.Discard, nil)))
	get(c, "a", size, "bytes=0-10")
	get(c, "a", size, fmt.Sprintf("bytes=%d-%d", 20*blockSize, 20*blockSize+10))
	get(c, "a", size, fmt.Sprintf("bytes=%d-%d", 40*blockSize, 40*blockSize+10))
	c.Prefetch("b", size)
	c.mu.Lock()
	_, started := c.files["b"]
	c.mu.Unlock()
	if started {
		t.Fatal("prefetched beside 3 downloads")
	}
	if rec := get(c, "b", size, "bytes=0-10"); rec.Code != 206 {
		t.Fatalf("a request waited: %d", rec.Code)
	}
}

type failingSource struct {
	fail  int // first calls that fail
	err   error
	calls atomic.Int64
	data  []byte
}

func (s *failingSource) OpenRange(ctx context.Context, _ string, start, end int64) (*http.Response, error) {
	if s.calls.Add(1) <= int64(s.fail) {
		return nil, s.err
	}
	if end < 0 {
		end = int64(len(s.data)) - 1
	}
	return &http.Response{StatusCode: 206, Body: io.NopCloser(bytes.NewReader(s.data[start : end+1]))}, nil
}

// Drive failing is answered before any header goes out (review #167): a 503 that says why, after
// tries spaced out; a failure that passes is not seen at all.
func TestDriveFailureIsAnswered(t *testing.T) {
	const size = 4 * blockSize
	notConnected := fmt.Errorf("google drive is not connected")
	src := &failingSource{fail: 100, err: notConnected, data: make([]byte, size)}
	c, _ := NewCache(src, t.TempDir(), 1<<30, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.Explain = func(err error) (string, bool) {
		if errors.Is(err, notConnected) {
			return "not_connected", true
		}
		return "drive", false
	}
	t0 := time.Now()
	rec := get(c, "f", size, "bytes=0-")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Content-Length") != "" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d, headers %v", rec.Code, rec.Header())
	}
	if body := rec.Body.String(); !strings.Contains(body, `"reason":"not_connected"`) || rec.Header().Get("Retry-After") != "" {
		t.Fatalf("body %s, Retry-After %q", body, rec.Header().Get("Retry-After"))
	}
	if n := src.calls.Load(); n != 3 {
		t.Fatalf("%d tries", n)
	}
	if d := time.Since(t0); d < 1400*time.Millisecond {
		t.Fatalf("3 tries in %v: not spaced out", d)
	}

	src = &failingSource{fail: 2, err: fmt.Errorf("drive 503"), data: make([]byte, size)}
	for i := range src.data {
		src.data[i] = byte(i)
	}
	c, _ = NewCache(src, t.TempDir(), 1<<30, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec = get(c, "f", size, "")
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), src.data) {
		t.Fatalf("after two failures: %d", rec.Code)
	}
}
