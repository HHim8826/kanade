// Package stream serves audio to players from a local cache that is filled from Drive in the
// background (decision D7): playing a track downloads the whole file once, so seeks after the
// first few seconds are local reads instead of 0.6-second Drive round trips.
//
// The budget is a hard limit on the space the cached files may take (their full sizes, as each
// grows to it). When files being played leave no room, a request is passed straight through from
// Drive instead of cached, and a prefetch is skipped.
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

	// OnWhole, when set, is told of a file once it is wholly cached (its id), from a goroutine of
	// its own: a copy that can be read through (Hold) without fetching it again.
	OnWhole func(id string)

	mu      sync.Mutex
	files   map[string]*file
	retired map[*file]bool // forgotten while being read; still counted until the last reader leaves
	seq     int64          // names cache files, so a new copy never reuses an old one's
	lean    atomic.Bool    // low disk: keep only what is being played
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
	return &Cache{src: src, dir: dir, budget: budget, log: log, files: map[string]*file{}, retired: map[*file]bool{}}, nil
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
	missing  int // blocks not cached yet
	fillers  []*filler
	failures int
	lastErr  error
	lastUse  time.Time
	readers  int  // requests and prefetches holding it; it is not dropped while held
	stale    bool // its Drive file changed: dropped once the last reader leaves, never handed out again
}

// errFull means the file does not fit in the budget beside the files in use.
var errFull = errors.New("the stream cache is full")

// acquire returns the cached file for id, adding it if room can be made, and holds it (readers)
// until release. errFull: no room, even after dropping everything not in use.
func (c *Cache) acquire(id string, size int64) (*file, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cf, ok := c.files[id]; ok {
		cf.mu.Lock()
		defer cf.mu.Unlock()
		if cf.size != size {
			return nil, fmt.Errorf("cached size %d differs from %d", cf.size, size)
		}
		cf.readers++
		cf.lastUse = time.Now()
		return cf, nil
	}
	if size > c.budget {
		return nil, errFull
	}
	if !c.evictLocked(size, c.lean.Load()) { // lean: everything not in use goes first
		return nil, errFull
	}
	f, err := os.OpenFile(c.path(id), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(size); err != nil { // sparse: disk use grows only as blocks arrive
		f.Close()
		return nil, err
	}
	blocks := int((size + blockSize - 1) / blockSize)
	cf := &file{c: c, id: id, size: size, f: f, have: make([]bool, blocks), missing: blocks, lastUse: time.Now(), readers: 1}
	cf.cond = sync.NewCond(&cf.mu)
	c.files[id] = cf
	return cf, nil
}

// Hold returns the path of id's cached copy when the whole file is there, kept (not dropped or
// evicted) until release is called.
func (c *Cache) Hold(id string) (path string, release func(), ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cf, found := c.files[id]
	if !found {
		return "", nil, false
	}
	cf.mu.Lock()
	defer cf.mu.Unlock()
	if cf.missing > 0 || cf.stale {
		return "", nil, false
	}
	cf.readers++
	return cf.f.Name(), func() { c.release(cf) }, true
}

func (c *Cache) path(id string) string {
	c.seq++
	return filepath.Join(c.dir, strings.NewReplacer("/", "_", "..", "_").Replace(id)+"."+strconv.FormatInt(c.seq, 10))
}

// release lets go of a file from acquire.
func (c *Cache) release(cf *file) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cf.mu.Lock()
	cf.readers--
	cf.lastUse = time.Now()
	drop := cf.stale && cf.readers == 0
	cf.mu.Unlock()
	if drop {
		c.dropLocked(cf)
	}
}

// usedLocked is the space taken by every cached file, forgotten ones still being read included.
func (c *Cache) usedLocked() int64 {
	var used int64
	for _, cf := range c.files {
		used += cf.size
	}
	for cf := range c.retired {
		used += cf.size
	}
	return used
}

// evictLocked drops files nobody holds, least recently used first, until need more bytes fit:
// first those idle for a while, then, if that is not enough, recently used ones too. With all, it
// drops every file nobody holds. It reports whether need fits.
func (c *Cache) evictLocked(need int64, all bool) bool {
	type entry struct {
		cf      *file
		lastUse time.Time
		readers int
	}
	used := c.usedLocked()
	list := make([]entry, 0, len(c.files))
	for _, cf := range c.files {
		cf.mu.Lock() // lastUse and readers change under the file's own lock
		list = append(list, entry{cf, cf.lastUse, cf.readers})
		cf.mu.Unlock()
	}
	sort.Slice(list, func(i, j int) bool { return list[i].lastUse.Before(list[j].lastUse) })
	for pass := 0; pass < 2; pass++ {
		for i, e := range list {
			if !all && used+need <= c.budget {
				return true
			}
			// Nobody can take a reader while c.mu is held, so a file seen free stays free.
			if e.cf == nil || e.readers > 0 || (pass == 0 && time.Since(e.lastUse) < idleGrace) {
				continue
			}
			c.dropLocked(e.cf)
			used -= e.cf.size
			list[i].cf = nil
		}
	}
	return used+need <= c.budget
}

// dropLocked removes a file from the cache, stopping its downloads.
func (c *Cache) dropLocked(cf *file) {
	cf.mu.Lock()
	for _, fl := range cf.fillers {
		fl.cancel()
	}
	cf.mu.Unlock()
	cf.f.Close()
	os.Remove(cf.f.Name())
	if c.files[cf.id] == cf {
		delete(c.files, cf.id)
	}
	delete(c.retired, cf)
}

// Used is the space the cached files take once complete; never more than the budget.
func (c *Cache) Used() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usedLocked()
}

// Trim drops every file nobody is using and returns how many bytes of cache that freed (the disk
// guard).
func (c *Cache) Trim() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	before := c.usedLocked()
	c.evictLocked(0, true)
	return before - c.usedLocked()
}

// Forget drops what is cached of a Drive file whose content changed or that is gone: at once, or
// when its last reader leaves. Later requests fetch it afresh.
func (c *Cache) Forget(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cf, ok := c.files[id]
	if !ok {
		return
	}
	cf.mu.Lock()
	cf.stale = true
	held := cf.readers > 0
	cf.mu.Unlock()
	if !held {
		c.dropLocked(cf)
		return
	}
	delete(c.files, id)
	c.retired[cf] = true
}

// Budget is the most the cache keeps.
func (c *Cache) Budget() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.budget
}

// SetBudget changes the most the cache keeps; files nobody is using go until the cache fits it
// (review #74).
func (c *Cache) SetBudget(n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.budget = n
	c.evictLocked(0, false)
}

// SetLean keeps the cache to the files being played while the disk is low; prefetching stops.
func (c *Cache) SetLean(on bool) { c.lean.Store(on) }

// Prefetch starts filling a file without a client request (plan: preload at most the next track).
func (c *Cache) Prefetch(id string, size int64) error {
	if c.lean.Load() {
		return nil
	}
	cf, err := c.acquire(id, size)
	if errors.Is(err, errFull) {
		return nil // no room: the track will be passed through when played
	}
	if err != nil {
		return err
	}
	defer c.release(cf)
	cf.mu.Lock()
	defer cf.mu.Unlock()
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
		if !cf.have[blk] {
			cf.have[blk] = true
			if cf.missing--; cf.missing == 0 && cf.c.OnWhole != nil {
				go cf.c.OnWhole(cf.id)
			}
		}
		cf.failures = 0
		fl.pos = pos
		cf.cond.Broadcast()
		cf.mu.Unlock()
	}
}

// copyRange writes bytes [start, end] to w as they become available.
// The caller holds the file (acquire).
func (cf *file) copyRange(ctx context.Context, w io.Writer, start, end int64) error {
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
	status := http.StatusOK
	if partial {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		status = http.StatusPartialContent
	}
	h.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	cf, err := c.acquire(id, size)
	if errors.Is(err, errFull) {
		c.passThrough(w, r, id, start, end, status)
		return
	}
	if err != nil {
		h.Del("Content-Length")
		h.Del("Content-Range")
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer c.release(cf)
	w.WriteHeader(status)
	if err := cf.copyRange(r.Context(), w, start, end); err != nil && r.Context().Err() == nil {
		c.log.Warn("stream", "file", id, "err", err)
	}
}

// passThrough serves a range straight from Drive, without caching, when the cache has no room.
func (c *Cache) passThrough(w http.ResponseWriter, r *http.Request, id string, start, end int64, status int) {
	c.Stats.SourceRequests.Add(1)
	resp, err := c.src.OpenRange(r.Context(), id, start, end)
	if err != nil {
		h := w.Header()
		h.Del("Content-Length")
		h.Del("Content-Range")
		http.Error(w, err.Error(), http.StatusServiceUnavailable) // not 502: Cloudflare replaces those
		return
	}
	defer resp.Body.Close()
	w.WriteHeader(status)
	n, err := io.Copy(w, io.LimitReader(resp.Body, end-start+1))
	c.Stats.SourceBytes.Add(n)
	c.Stats.ServedBytes.Add(n)
	if err != nil && r.Context().Err() == nil {
		c.log.Warn("stream pass-through", "file", id, "err", err)
	}
}
