// Package stream serves audio to players from a local cache that is filled from Drive in the
// background (decision D7): playing a track downloads the whole file once, so seeks after the
// first few seconds are local reads instead of 0.6-second Drive round trips.
package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	blockSize = 256 << 10
	// A request waits for a running download that will reach its block within this distance;
	// a seek further ahead starts a second download at that point.
	fillerReach = 2 << 20
	maxFillers  = 3 // per file
	idleGrace   = 30 * time.Second
)

// Source opens a byte range of a remote file; end < 0 means to the end of the file.
type Source interface {
	OpenRange(ctx context.Context, id string, start, end int64) (*http.Response, error)
}

type Stats struct {
	SourceRequests atomic.Int64
	SourceBytes    atomic.Int64
	ServedBytes    atomic.Int64
}

type Cache struct {
	src    Source
	dir    string
	budget int64
	log    *slog.Logger
	Stats  Stats

	mu    sync.Mutex
	files map[string]*file
}

// NewCache starts with an empty cache directory: block maps are not persisted, so a
// leftover file could not be trusted after a restart.
func NewCache(src Source, dir string, budget int64, log *slog.Logger) (*Cache, error) {
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Cache{src: src, dir: dir, budget: budget, log: log, files: map[string]*file{}}, nil
}

type filler struct {
	pos    int64
	done   bool
	cancel context.CancelFunc
}

type file struct {
	c        *Cache
	id       string
	size     int64
	f        *os.File
	mu       sync.Mutex
	cond     *sync.Cond
	have     []bool
	fillers  []*filler
	failures int
	lastErr  error
	lastUse  time.Time
	readers  int
}

func (c *Cache) open(id string, size int64) (*file, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cf, ok := c.files[id]; ok {
		if cf.size != size {
			return nil, fmt.Errorf("cached size %d differs from %d", cf.size, size)
		}
		return cf, nil
	}
	if size > c.budget {
		return nil, errors.New("file is larger than the stream cache")
	}
	c.evictLocked(size)
	f, err := os.OpenFile(filepath.Join(c.dir, strings.NewReplacer("/", "_", "..", "_").Replace(id)), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(size); err != nil { // sparse: disk use grows only as blocks arrive
		f.Close()
		return nil, err
	}
	cf := &file{c: c, id: id, size: size, f: f, have: make([]bool, (size+blockSize-1)/blockSize), lastUse: time.Now()}
	cf.cond = sync.NewCond(&cf.mu)
	c.files[id] = cf
	return cf, nil
}

// evictLocked drops idle files, least recently used first, until need more bytes fit.
func (c *Cache) evictLocked(need int64) {
	var used int64
	list := make([]*file, 0, len(c.files))
	for _, cf := range c.files {
		used += cf.size
		list = append(list, cf)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].lastUse.Before(list[j].lastUse) })
	for _, cf := range list {
		if used+need <= c.budget {
			return
		}
		cf.mu.Lock()
		busy := cf.readers > 0 || time.Since(cf.lastUse) < idleGrace
		if !busy {
			for _, fl := range cf.fillers {
				fl.cancel()
			}
		}
		cf.mu.Unlock()
		if busy {
			continue
		}
		cf.f.Close()
		os.Remove(cf.f.Name())
		delete(c.files, cf.id)
		used -= cf.size
	}
}

// Prefetch starts filling a file without a client request (plan: preload at most the next track).
func (c *Cache) Prefetch(id string, size int64) error {
	cf, err := c.open(id, size)
	if err != nil {
		return err
	}
	cf.mu.Lock()
	defer cf.mu.Unlock()
	cf.lastUse = time.Now()
	if !cf.have[0] && !cf.coveredLocked(0) {
		cf.startFillerLocked(0)
	}
	return nil
}

func (cf *file) coveredLocked(blk int64) bool {
	off := blk * blockSize
	for _, fl := range cf.fillers {
		if !fl.done && fl.pos <= off && off-fl.pos <= fillerReach {
			return true
		}
	}
	return false
}

func (cf *file) startFillerLocked(blk int64) {
	live := cf.fillers[:0]
	for _, fl := range cf.fillers {
		if !fl.done {
			live = append(live, fl)
		}
	}
	cf.fillers = live
	if len(cf.fillers) >= maxFillers {
		cf.fillers[0].cancel()
		cf.fillers[0].done = true
		cf.fillers = cf.fillers[1:]
	}
	ctx, cancel := context.WithCancel(context.Background())
	fl := &filler{pos: blk * blockSize, cancel: cancel}
	cf.fillers = append(cf.fillers, fl)
	go cf.fill(ctx, fl)
}

// fill downloads from fl.pos toward the end, stopping at a block that is already cached
// (another filler got there first) so nothing is fetched twice.
func (cf *file) fill(ctx context.Context, fl *filler) {
	var err error
	defer func() {
		cf.mu.Lock()
		fl.done = true
		if err != nil && !errors.Is(err, context.Canceled) {
			cf.failures++
			cf.lastErr = err
			cf.c.log.Warn("stream fill failed", "file", cf.id, "at", fl.pos, "err", err)
		}
		cf.cond.Broadcast()
		cf.mu.Unlock()
		fl.cancel()
	}()
	cf.c.Stats.SourceRequests.Add(1)
	resp, err := cf.c.src.OpenRange(ctx, cf.id, fl.pos, -1)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	buf := make([]byte, blockSize)
	for pos := fl.pos; pos < cf.size; {
		blk := pos / blockSize
		cf.mu.Lock()
		already := cf.have[blk]
		cf.mu.Unlock()
		if already {
			return
		}
		n := min(int64(blockSize), cf.size-pos)
		if _, err = io.ReadFull(resp.Body, buf[:n]); err != nil {
			return
		}
		cf.c.Stats.SourceBytes.Add(n)
		if _, err = cf.f.WriteAt(buf[:n], pos); err != nil {
			return
		}
		pos += n
		cf.mu.Lock()
		cf.have[blk] = true
		cf.failures = 0
		fl.pos = pos
		cf.cond.Broadcast()
		cf.mu.Unlock()
	}
}

// copyRange writes bytes [start, end] to w as they become available.
func (cf *file) copyRange(ctx context.Context, w io.Writer, start, end int64) error {
	cf.mu.Lock()
	cf.readers++
	cf.mu.Unlock()
	defer func() {
		cf.mu.Lock()
		cf.readers--
		cf.lastUse = time.Now()
		cf.mu.Unlock()
	}()
	stop := context.AfterFunc(ctx, func() {
		cf.mu.Lock()
		cf.cond.Broadcast()
		cf.mu.Unlock()
	})
	defer stop()
	buf := make([]byte, blockSize)
	for off := start; off <= end; {
		blk := off / blockSize
		cf.mu.Lock()
		for !cf.have[blk] {
			if err := ctx.Err(); err != nil {
				cf.mu.Unlock()
				return err
			}
			if !cf.coveredLocked(blk) {
				if cf.failures >= 3 {
					err := cf.lastErr
					cf.failures = 0 // let a later request try again
					cf.mu.Unlock()
					return fmt.Errorf("drive keeps failing: %w", err)
				}
				cf.startFillerLocked(blk)
			}
			cf.cond.Wait()
		}
		cf.lastUse = time.Now()
		cf.mu.Unlock()
		n := min((blk+1)*blockSize, end+1) - off
		if _, err := cf.f.ReadAt(buf[:n], off); err != nil {
			return err
		}
		if _, err := w.Write(buf[:n]); err != nil {
			return err
		}
		cf.c.Stats.ServedBytes.Add(n)
		off += n
	}
	return nil
}

// parseRange handles the single-range forms players send: bytes=a-b, bytes=a-, bytes=-n.
func parseRange(h string, size int64) (start, end int64, partial bool, err error) {
	if h == "" {
		return 0, size - 1, false, nil
	}
	spec, ok := strings.CutPrefix(h, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return 0, 0, false, errors.New("unsupported range")
	}
	a, b, _ := strings.Cut(spec, "-")
	if a == "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false, errors.New("bad range")
		}
		return max(size-n, 0), size - 1, true, nil
	}
	start, err = strconv.ParseInt(a, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false, errors.New("range not satisfiable")
	}
	end = size - 1
	if b != "" {
		if end, err = strconv.ParseInt(b, 10, 64); err != nil || end < start {
			return 0, 0, false, errors.New("bad range")
		}
		end = min(end, size-1)
	}
	return start, end, true, nil
}

// Serve answers a GET or HEAD for a remote file of the given size.
// etag identifies the content version (the asset's SHA-256).
func (c *Cache) Serve(w http.ResponseWriter, r *http.Request, id string, size int64, contentType, etag string) {
	h := w.Header()
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", "private, max-age=86400")
	if etag != "" {
		h.Set("ETag", `"`+etag+`"`)
	}
	start, end, partial, err := parseRange(r.Header.Get("Range"), size)
	if err != nil {
		h.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		http.Error(w, err.Error(), http.StatusRequestedRangeNotSatisfiable)
		return
	}
	cf, err := c.open(id, size)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	h.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	status := http.StatusOK
	if partial {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		status = http.StatusPartialContent
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	w.WriteHeader(status)
	if err := cf.copyRange(r.Context(), w, start, end); err != nil && r.Context().Err() == nil {
		c.log.Warn("stream", "file", id, "err", err)
	}
}
