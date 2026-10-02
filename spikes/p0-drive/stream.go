package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
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

// P0 §2: stream Drive files to players, either straight through ("direct") or via an
// on-disk cache that one background download per file fills ahead of playback ("cache").

const (
	streamKeyFile = projectDir + "/secrets/p0-stream-key"
	blockSize     = 256 << 10
	// A request waits for an existing filler if the filler will reach the block within this distance;
	// otherwise (a seek far ahead) it starts a new filler at the block.
	fillerReach = 2 << 20
	maxFillers  = 3
)

func cmdDownload(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: download FILE_ID")
	}
	d, err := newDrive()
	if err != nil {
		return err
	}
	t0 := time.Now()
	resp, err := d.do(ctx, http.MethodGet, mediaURL(args[0]), nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	ttfb := time.Since(t0)
	n, err := io.CopyBuffer(io.Discard, resp.Body, make([]byte, 1<<20))
	if err != nil {
		return err
	}
	secs := time.Since(t0).Seconds()
	fmt.Printf("downloaded %.1f MiB: headers after %dms, total %.1fs, %.1f MiB/s\n",
		float64(n)/(1<<20), ttfb.Milliseconds(), secs, float64(n)/(1<<20)/secs)
	return nil
}

type stats struct {
	DriveRequests atomic.Int64
	DriveBytes    atomic.Int64
	ServedBytes   atomic.Int64
	ClientReqs    atomic.Int64
}

type server struct {
	d        *drive
	st       *state
	key      string
	cacheDir string
	budget   int64
	stats    stats

	mu    sync.Mutex
	files map[string]*cacheFile
	meta  map[string]driveFile
}

func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8082", "address to listen on")
	cacheDir := fs.String("cache-dir", projectDir+"/cache-p0", "cache directory")
	budgetMiB := fs.Int64("cache-mib", 512, "cache budget in MiB (plan default 512)")
	fs.Parse(args)

	st, err := loadState()
	if err != nil {
		return err
	}
	if err := st.requireFolder(); err != nil {
		return err
	}
	d, err := newDrive()
	if err != nil {
		return err
	}
	if err := os.RemoveAll(*cacheDir); err != nil { // the spike keeps no cache index across runs
		return err
	}
	if err := os.MkdirAll(*cacheDir, 0o700); err != nil {
		return err
	}
	raw := make([]byte, 16)
	rand.Read(raw)
	s := &server{d: d, st: st, key: hex.EncodeToString(raw), cacheDir: *cacheDir, budget: *budgetMiB << 20,
		files: map[string]*cacheFile{}, meta: map[string]driveFile{}}
	if err := os.WriteFile(streamKeyFile, []byte(s.key), 0o600); err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /stream/{id}", s.handleStream)
	mux.HandleFunc("GET /stats", s.handleStats)
	log.Printf("serving on http://%s (key in %s, cache %s, budget %d MiB)", *listen, streamKeyFile, *cacheDir, *budgetMiB)
	return http.ListenAndServe(*listen, mux)
}

func (s *server) authorized(r *http.Request) bool {
	k := r.URL.Query().Get("k")
	if k == "" {
		k = r.Header.Get("X-P0-Key")
	}
	return k == s.key
}

func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	json.NewEncoder(w).Encode(map[string]int64{
		"drive_requests": s.stats.DriveRequests.Load(),
		"drive_bytes":    s.stats.DriveBytes.Load(),
		"served_bytes":   s.stats.ServedBytes.Load(),
		"client_reqs":    s.stats.ClientReqs.Load(),
	})
}

// fileMeta loads and caches metadata, refusing anything outside the test folder:
// the stream endpoint can be reached through the tunnel.
func (s *server) fileMeta(ctx context.Context, id string) (driveFile, error) {
	s.mu.Lock()
	m, ok := s.meta[id]
	s.mu.Unlock()
	if ok {
		return m, nil
	}
	inside, err := s.d.inTestTree(ctx, s.st, id)
	if err != nil {
		return driveFile{}, err
	}
	if !inside {
		return driveFile{}, errors.New("file is outside the test folder")
	}
	m, err = s.d.getFile(ctx, id, "id,name,size,mimeType,sha256Checksum")
	if err != nil {
		return driveFile{}, err
	}
	s.mu.Lock()
	s.meta[id] = m
	s.mu.Unlock()
	return m, nil
}

// parseRange handles the single "bytes=a-b" / "bytes=a-" / "bytes=-n" forms players send.
func parseRange(h string, size int64) (start, end int64, partial bool, err error) {
	if h == "" {
		return 0, size - 1, false, nil
	}
	spec, ok := strings.CutPrefix(h, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return 0, 0, false, errors.New("unsupported range")
	}
	a, b, _ := strings.Cut(spec, "-")
	switch {
	case a == "":
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false, errors.New("bad range")
		}
		return max(size-n, 0), size - 1, true, nil
	default:
		start, err = strconv.ParseInt(a, 10, 64)
		if err != nil || start >= size {
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
}

// timedWriter records when the first body byte goes out.
type timedWriter struct {
	http.ResponseWriter
	first time.Time
	n     int64
}

func (w *timedWriter) Write(p []byte) (int, error) {
	if w.first.IsZero() {
		w.first = time.Now()
	}
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)
	return n, err
}

func (s *server) handleStream(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	if !s.authorized(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.stats.ClientReqs.Add(1)
	id, mode := r.PathValue("id"), r.URL.Query().Get("mode")
	if mode == "" {
		mode = "cache"
	}
	m, err := s.fileMeta(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	size := m.size()
	start, end, partial, err := parseRange(r.Header.Get("Range"), size)
	if err != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		http.Error(w, err.Error(), http.StatusRequestedRangeNotSatisfiable)
		return
	}
	driveBefore := s.stats.DriveRequests.Load()

	h := w.Header()
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Type", contentType(m))
	h.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	if m.SHA256Checksum != "" {
		h.Set("ETag", `"`+m.SHA256Checksum+`"`) // content version: changes whenever the bytes change
	}
	status := http.StatusOK
	if partial {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		status = http.StatusPartialContent
	}
	tw := &timedWriter{ResponseWriter: w}

	if mode == "direct" {
		err = s.serveDirect(r.Context(), tw, status, id, start, end)
	} else {
		var cf *cacheFile
		if cf, err = s.cacheFor(m); err != nil {
			failBeforeBody(w, err)
		} else {
			w.WriteHeader(status)
			err = cf.serve(r.Context(), tw, start, end)
		}
	}
	s.stats.ServedBytes.Add(tw.n)
	ttfb := "-"
	if !tw.first.IsZero() {
		ttfb = strconv.FormatInt(tw.first.Sub(t0).Milliseconds(), 10) + "ms"
	}
	errText := ""
	if err != nil && !errors.Is(err, context.Canceled) {
		errText = " err=" + err.Error()
	}
	log.Printf("%-6s %-24s bytes=%d-%d sent=%d ttfb=%s total=%dms drive_reqs=+%d%s",
		mode, m.Name, start, end, tw.n, ttfb, time.Since(t0).Milliseconds(),
		s.stats.DriveRequests.Load()-driveBefore, errText)
}

// failBeforeBody replaces the success headers already set with a plain 502.
func failBeforeBody(w http.ResponseWriter, err error) {
	h := w.Header()
	h.Del("Content-Length")
	h.Del("Content-Range")
	h.Del("ETag")
	http.Error(w, err.Error(), http.StatusBadGateway)
}

func contentType(m driveFile) string {
	switch strings.ToLower(filepath.Ext(m.Name)) {
	case ".flac":
		return "audio/flac"
	case ".mp3":
		return "audio/mpeg"
	case ".m4a":
		return "audio/mp4"
	case ".ogg", ".opus":
		return "audio/ogg"
	}
	if m.MimeType != "" {
		return m.MimeType
	}
	return "application/octet-stream"
}

func (s *server) serveDirect(ctx context.Context, w *timedWriter, status int, id string, start, end int64) error {
	h := http.Header{}
	h.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	s.stats.DriveRequests.Add(1)
	resp, err := s.d.do(ctx, http.MethodGet, mediaURL(id), nil, h)
	if err != nil {
		failBeforeBody(w, err)
		return err
	}
	defer resp.Body.Close()
	w.WriteHeader(status)
	n, err := io.CopyBuffer(w, resp.Body, make([]byte, 64<<10))
	s.stats.DriveBytes.Add(n)
	return err
}

// ---- cache ----

type filler struct {
	pos    int64 // next byte this filler will write
	done   bool
	cancel context.CancelFunc
	start  time.Time
}

type cacheFile struct {
	s        *server
	meta     driveFile
	size     int64
	f        *os.File
	mu       sync.Mutex
	cond     *sync.Cond
	have     []bool
	fillers  []*filler
	failures int
	lastUse  time.Time
}

func (s *server) cacheFor(m driveFile) (*cacheFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cf, ok := s.files[m.ID]; ok {
		cf.lastUse = time.Now()
		return cf, nil
	}
	if m.size() > s.budget {
		return nil, fmt.Errorf("file larger than the cache budget")
	}
	s.evictLocked(m.size())
	f, err := os.OpenFile(filepath.Join(s.cacheDir, m.ID), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(m.size()); err != nil { // sparse: disk use grows only as blocks arrive
		f.Close()
		return nil, err
	}
	cf := &cacheFile{s: s, meta: m, size: m.size(), f: f, have: make([]bool, (m.size()+blockSize-1)/blockSize), lastUse: time.Now()}
	cf.cond = sync.NewCond(&cf.mu)
	s.files[m.ID] = cf
	return cf, nil
}

// evictLocked drops least-recently-used idle files until need more bytes fit in the budget.
func (s *server) evictLocked(need int64) {
	var used int64
	list := make([]*cacheFile, 0, len(s.files))
	for _, cf := range s.files {
		used += cf.size
		list = append(list, cf)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].lastUse.Before(list[j].lastUse) })
	for _, cf := range list {
		if used+need <= s.budget {
			return
		}
		cf.mu.Lock()
		busy := time.Since(cf.lastUse) < 30*time.Second
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
		delete(s.files, cf.meta.ID)
		used -= cf.size
		log.Printf("evicted %s (%d MiB)", cf.meta.Name, cf.size>>20)
	}
}

// coveredLocked reports whether a running filler will reach block blk soon.
func (cf *cacheFile) coveredLocked(blk int64) bool {
	off := blk * blockSize
	for _, fl := range cf.fillers {
		if !fl.done && fl.pos <= off && off-fl.pos <= fillerReach {
			return true
		}
	}
	return false
}

func (cf *cacheFile) startFillerLocked(blk int64) {
	live := cf.fillers[:0]
	for _, fl := range cf.fillers {
		if !fl.done {
			live = append(live, fl)
		}
	}
	cf.fillers = live
	if len(cf.fillers) >= maxFillers {
		cf.fillers[0].cancel() // oldest; its waiters will start another if still needed
		cf.fillers[0].done = true
		cf.fillers = cf.fillers[1:]
	}
	ctx, cancel := context.WithCancel(context.Background())
	fl := &filler{pos: blk * blockSize, cancel: cancel, start: time.Now()}
	cf.fillers = append(cf.fillers, fl)
	go cf.fill(ctx, fl)
}

// fill downloads from fl.pos towards the end of the file, block by block,
// stopping when it reaches a block that is already cached.
func (cf *cacheFile) fill(ctx context.Context, fl *filler) {
	s := cf.s
	var err error
	defer func() {
		cf.mu.Lock()
		fl.done = true
		if err != nil && !errors.Is(err, context.Canceled) {
			cf.failures++
			log.Printf("filler %s@%d failed: %v", cf.meta.Name, fl.pos, err)
		}
		cf.cond.Broadcast()
		cf.mu.Unlock()
		fl.cancel()
	}()
	h := http.Header{}
	h.Set("Range", fmt.Sprintf("bytes=%d-", fl.pos))
	s.stats.DriveRequests.Add(1)
	resp, err := s.d.do(ctx, http.MethodGet, mediaURL(cf.meta.ID), nil, h)
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
		s.stats.DriveBytes.Add(n)
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

func (cf *cacheFile) serve(ctx context.Context, w io.Writer, start, end int64) error {
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
					cf.mu.Unlock()
					return errors.New("drive fetch keeps failing")
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
		off += n
	}
	return nil
}
