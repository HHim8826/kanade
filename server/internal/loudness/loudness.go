// Package loudness measures how loud each song is (review #136), for the player's volume balance:
// EBU R128 integrated loudness and sample peak, by FFmpeg at low priority. A song is measured where
// it already is when that costs nothing more: as it is imported (the file is local), and once
// playing has cached it whole; one such at a time. The rest is measured by a scan of the library,
// each file streamed from Drive into FFmpeg without being kept: when asked, and by itself (a minute
// after starting, then every Every: songs the Drive inbox imports where they are) only once
// someone turned the volume balance on (review #155). The scan runs beside the local measurements,
// so an import never waits for Drive (review #145).
package loudness

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/ffmpeg"
	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/staging"
)

// Source opens a byte range of a Drive file; end < 0 means to the end (the stream cache's source).
type Source interface {
	OpenRange(ctx context.Context, id string, start, end int64) (*http.Response, error)
}

type Service struct {
	Lib    *library.Store
	DB     *sql.DB      // keeps whether the library is scanned by itself
	FF     *ffmpeg.Tool // nil: nothing is measured
	Source Source
	// Hold gives the path of a wholly cached copy of a Drive file, kept until release (the stream
	// cache's Hold); nil when there is no cache.
	Hold func(driveID string) (path string, release func(), ok bool)
	Temp string // where a file that must be read with seeking (MP4) is put while it is measured
	// Budget is the staging budget a copy in Temp takes its room from (review #144); nil: none.
	Budget *staging.Budget
	Every  time.Duration // how often the library is scanned by itself, when it is; 0: only when asked
	Log    *slog.Logger

	turn   chan struct{} // a turn to measure a file that is here
	queue  chan string
	mu     sync.Mutex
	queued map[string]bool
	scan   *scan
	last   ScanState // the last scan, once it ended
}

// Whether the library is scanned by itself (review #155): AutoUnset until someone turns the volume
// balance on (the web client then turns this on) or off.
const (
	AutoUnset = ""
	AutoOn    = "on"
	AutoOff   = "off"
	autoKey   = "loudness.auto"
)

// ScanState is a scan of the library: running, how far it got, and its errors.
type ScanState struct {
	Running bool   `json:"running"`
	Done    int    `json:"done"`   // files measured by this scan
	Failed  int    `json:"failed"` // files it could not measure
	Error   string `json:"error,omitempty"`
	// Reason says why it ended early or left files for another scan: not_connected, auth_expired
	// (Drive), drive (Drive kept failing, or some files could not be read), room (no staging room
	// for a copy).
	Reason string `json:"reason,omitempty"`
}

// Drive failing this many files in a row ends a scan: it is not going to work now.
const maxDriveFailures = 5

// firstScan is when the scan by itself first runs, once the server has settled (a variable for the
// tests).
var firstScan = time.Minute

type scan struct {
	ScanState
	cancel context.CancelFunc
}

// Available reports whether there is an FFmpeg to measure with.
func (s *Service) Available() bool { return s != nil && s.FF != nil }

// Run measures the songs playing has cached, as they come, and scans the library by itself while
// that is on, until ctx ends.
func (s *Service) Run(ctx context.Context) {
	s.init()
	var scanAt <-chan time.Time
	if s.Every > 0 && s.Available() {
		scanAt = time.After(firstScan)
	}
	for {
		select {
		case <-ctx.Done():
			s.Stop()
			return
		case <-scanAt:
			if s.Auto(ctx) == AutoOn {
				if err := s.Scan(false); err != nil {
					s.Log.Warn("loudness scan", "err", err)
				}
			}
			scanAt = time.After(s.Every)
		case id := <-s.queue:
			s.mu.Lock()
			delete(s.queued, id)
			s.mu.Unlock()
			s.measureCached(ctx, id)
		}
	}
}

func (s *Service) init() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.queue == nil {
		s.queue = make(chan string, 256)
		s.queued = map[string]bool{}
		s.turn = make(chan struct{}, 1)
	}
}

// Auto is whether the library is scanned by itself.
func (s *Service) Auto(ctx context.Context) string {
	if s.DB == nil {
		return AutoUnset
	}
	v, _ := db.GetSetting(ctx, s.DB, autoKey)
	return v
}

// SetAuto turns scanning by itself on (and starts a scan now) or off (and stops the one running).
func (s *Service) SetAuto(ctx context.Context, on bool) error {
	v := AutoOff
	if on {
		v = AutoOn
	}
	if err := db.SetSetting(ctx, s.DB, autoKey, v); err != nil {
		return err
	}
	if !on {
		s.Stop()
		return nil
	}
	return s.Scan(false)
}

// local waits for a turn to measure a file that is here, and gives it back with done.
func (s *Service) local(ctx context.Context) (done func(), err error) {
	s.init()
	select {
	case s.turn <- struct{}{}:
		return func() { <-s.turn }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Cached is told of a Drive file the stream cache now holds whole (stream.Cache.OnWhole): it is
// measured from there unless it already was. Never blocks; a full queue drops it (a scan or a
// later play measures it).
func (s *Service) Cached(driveID string) {
	if !s.Available() {
		return
	}
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.queued[driveID] {
		return
	}
	select {
	case s.queue <- driveID:
		s.queued[driveID] = true
	default:
	}
}

func (s *Service) measureCached(ctx context.Context, driveID string) {
	asset, err := s.Lib.AssetByDriveID(ctx, driveID)
	if err != nil || asset == 0 {
		return
	}
	if known, err := s.Lib.LoudnessKnown(ctx, asset); err != nil || known {
		return
	}
	if err := s.fromCache(ctx, asset, driveID); err != nil && !errors.Is(err, errNotCached) {
		s.Log.Warn("measure loudness", "asset", asset, "err", err)
	}
}

var errNotCached = errors.New("not cached whole")

func (s *Service) fromCache(ctx context.Context, asset int64, driveID string) error {
	if s.Hold == nil {
		return errNotCached
	}
	path, release, ok := s.Hold(driveID)
	if !ok {
		return errNotCached
	}
	defer release()
	done, err := s.local(ctx)
	if err != nil {
		return err
	}
	defer done()
	return s.measure(ctx, asset, path, nil, -1)
}

// File measures a song from its local file (as it is imported), unless it was measured already.
// A failure is recorded and does not stop the import.
func (s *Service) File(ctx context.Context, asset int64, path string) {
	if !s.Available() {
		return
	}
	if known, err := s.Lib.LoudnessKnown(ctx, asset); err != nil || known {
		return
	}
	done, err := s.local(ctx)
	if err != nil {
		return
	}
	defer done()
	if err := s.measure(ctx, asset, path, nil, -1); err != nil {
		s.Log.Warn("measure loudness", "asset", asset, "err", err)
	}
}

// errTransient is a measurement that did not get the whole file (Drive, the network): nothing is
// recorded, so it is tried again later.
type errTransient struct{ err error }

func (e errTransient) Error() string { return e.err.Error() }
func (e errTransient) Unwrap() error { return e.err }

// measure runs FFmpeg on path, or on in when given, which must give size bytes. A measurement is
// recorded; FFmpeg failing on a whole file is recorded as the reason it could not be.
func (s *Service) measure(ctx context.Context, asset int64, path string, in io.Reader, size int64) error {
	var count *counter
	if in != nil {
		count = &counter{r: in}
		in = count
	}
	lufs, peak, err := s.FF.Loudness(ctx, path, in)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case count != nil && count.err != nil:
		return errTransient{fmt.Errorf("reading the file: %w", count.err)}
	case count != nil && count.n != size && err == nil:
		return errTransient{fmt.Errorf("got %d of %d bytes", count.n, size)}
	case err != nil:
		if rerr := s.Lib.LoudnessFailed(ctx, asset, err.Error()); rerr != nil {
			return rerr
		}
		return err
	}
	return s.Lib.SetLoudness(ctx, asset, lufs, peak, library.MethodEBUR128)
}

// counter counts what is read, and keeps the first read error other than EOF.
type counter struct {
	r   io.Reader
	n   int64
	err error
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if err != nil && err != io.EOF && c.err == nil {
		c.err = err
	}
	return n, err
}

// ---- measuring the whole library, when asked ----

// Scan starts measuring every file not measured yet (with failed, also those that could not be),
// one at a time, until done or Stop; one scan at a time.
func (s *Service) Scan(failed bool) error {
	if !s.Available() {
		return errors.New("FFmpeg is not installed, so loudness cannot be measured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scan != nil {
		return nil // already running
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.scan = &scan{ScanState: ScanState{Running: true}, cancel: cancel}
	go s.runScan(ctx, s.scan, failed)
	return nil
}

// Stop ends a scan after the file it is measuring.
func (s *Service) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scan != nil {
		s.scan.cancel()
	}
}

// State is the running scan's, or the last one's.
func (s *Service) State() ScanState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scan != nil {
		return s.scan.ScanState
	}
	return s.last
}

func (s *Service) runScan(ctx context.Context, sc *scan, failed bool) {
	var after int64
	tried := map[int64]bool{}
	var firstErr error
	inARow := 0 // Drive failures
	finish := func(msg, reason string) {
		s.mu.Lock()
		sc.Running = false
		if msg != "" {
			sc.Error, sc.Reason = msg, reason
		}
		st := sc.ScanState
		s.last, s.scan = st, nil
		s.mu.Unlock()
		sc.cancel()
		// One line for the scan, not one for each file (review #155).
		if st.Failed > 0 || st.Error != "" {
			s.Log.Warn("loudness scan ended", "measured", st.Done, "failed", st.Failed, "reason", st.Reason, "first_err", firstErr)
		} else if st.Done > 0 {
			s.Log.Info("loudness scan ended", "measured", st.Done)
		}
	}
	for {
		list, err := s.Lib.Unmeasured(ctx, after, 50, failed)
		if err != nil {
			if ctx.Err() != nil {
				finish("", "")
			} else {
				finish(err.Error(), "")
			}
			return
		}
		if len(list) == 0 {
			finish("", "")
			return
		}
		for _, u := range list {
			after = u.AssetID
			if tried[u.AssetID] {
				continue
			}
			tried[u.AssetID] = true
			err := s.fromCache(ctx, u.AssetID, u.DriveFileID)
			if errors.Is(err, errNotCached) {
				err = s.fromDrive(ctx, u)
			}
			if ctx.Err() != nil {
				finish("", "")
				return
			}
			if errors.Is(err, errNoRoom) {
				s.mu.Lock()
				sc.Error, sc.Reason = "no staging room to copy some files; they are measured on another scan", "room"
				s.mu.Unlock()
				continue
			}
			var t errTransient
			transient := errors.As(err, &t)
			if transient {
				inARow++
			} else {
				inARow = 0
			}
			s.mu.Lock()
			if err != nil {
				sc.Failed++
				if firstErr == nil {
					firstErr = err
				}
				s.Log.Debug("measure loudness", "asset", u.AssetID, "err", err)
				if transient {
					sc.Error, sc.Reason = "some files could not be read from Drive; they are measured on another scan", "drive"
				}
			} else {
				sc.Done++
			}
			s.mu.Unlock()
			// Drive not connected, or failing file after file: the rest would fail too.
			if reason, lasting := gdrive.Explain(err); transient && (lasting || inARow >= maxDriveFailures) {
				if !lasting {
					reason = "drive"
				}
				finish("Drive could not be read: "+err.Error(), reason)
				return
			}
		}
	}
}

// errNoRoom is a copy that does not fit the staging budget now: the file is left for another scan.
var errNoRoom = errors.New("no staging room")

// seeking are the formats FFmpeg may need to seek in (MP4 keeps its index at the end at times):
// they are measured from a copy.
var seeking = map[string]bool{"m4a": true, "mp4": true, "alac": true}

func (s *Service) fromDrive(ctx context.Context, u library.Unmeasured) error {
	open := func() (io.ReadCloser, error) {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		resp, err := s.Source.OpenRange(rctx, u.DriveFileID, 0, -1)
		if err != nil {
			cancel()
			return nil, errTransient{err}
		}
		return struct {
			io.Reader
			io.Closer
		}{resp.Body, closer(func() error { cancel(); return resp.Body.Close() })}, nil
	}
	body, err := open()
	if err != nil {
		return err
	}
	defer body.Close()
	if !seeking[u.Format] {
		return s.measure(ctx, u.AssetID, "", body, u.Size)
	}
	// The copy takes its room from the staging budget and the disk's reserve (review #144); a file
	// bigger than the budget may still be copied by itself.
	if s.Budget != nil {
		release, err := s.Budget.HoldRequest(ctx, staging.Request{Need: u.Size, Alone: true})
		if err != nil {
			return fmt.Errorf("%w: %v", errNoRoom, err)
		}
		defer release()
	}
	if err := os.MkdirAll(s.Temp, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Temp, "loudness-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	n, err := io.Copy(f, body)
	f.Close()
	if err != nil || n != u.Size {
		return errTransient{fmt.Errorf("copying the file: %d of %d bytes: %v", n, u.Size, err)}
	}
	return s.measure(ctx, u.AssetID, filepath.Clean(f.Name()), nil, -1)
}

type closer func() error

func (c closer) Close() error { return c() }
