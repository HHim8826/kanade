// Package storesync keeps the library in step with its storage locations besides Drive (#99):
// every few minutes each is checked (is it there; how much room it has); a source whose files are
// imported by themselves is scanned every few hours (importer.ScanSource), once it was scanned by
// hand; and a managed location is checked to hold the library's files once a day. Each is done on
// request too. Drive keeps its own sync (drivesync).
package storesync

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/importer"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/storage"
)

type Syncer struct {
	Set *storage.Set
	Lib *library.Store
	Imp *importer.Importer
	Log *slog.Logger
	// Forget drops what the stream cache holds of a file that is gone; nil to skip.
	Forget func(ref string)

	CheckEvery     time.Duration // 10 minutes when 0
	ScanEvery      time.Duration // 6 hours when 0
	ReconcileEvery time.Duration // a day when 0

	mu      sync.Mutex
	running map[int64]bool // reconciles
}

// Running says whether a managed location's files are being checked now.
func (s *Syncer) Running(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[id]
}

// Result is what checking that a managed location holds the library's files found.
type Result struct {
	At       int64 `json:"at"`
	Files    int   `json:"files"`    // files there
	Checked  int   `json:"checked"`  // the library's songs there
	Missing  int   `json:"missing"`  // newly missing
	Restored int   `json:"restored"` // missing ones that are back
	Held     int   `json:"held,omitempty"`
	Lost     int   `json:"lost,omitempty"` // covers and sidecars not there
	// Gallery pictures newly missing, missing ones back, and those gone but held (review #246).
	PicturesGone int    `json:"pictures_gone,omitempty"`
	PicturesBack int    `json:"pictures_back,omitempty"`
	HeldPictures int    `json:"held_pictures,omitempty"`
	Error        string `json:"error,omitempty"`
}

// ErrRunning is a check asked for while one of the same location runs.
var ErrRunning = errors.New("this storage location is being checked already")

const heldMin = 20

func every(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// Run checks every minute what is due, starting a minute after start-up.
func (s *Syncer) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.round(ctx)
	}
}

func lastAt(raw json.RawMessage) int64 {
	var v struct{ At int64 }
	json.Unmarshal(raw, &v)
	return v.At
}

func (s *Syncer) round(ctx context.Context) {
	stores, err := s.Set.Stores(ctx)
	if err != nil {
		s.Log.Warn("storage locations", "err", err)
		return
	}
	now := db.Now()
	for _, st := range stores {
		if ctx.Err() != nil {
			return
		}
		if now-st.CheckedAt >= every(s.CheckEvery, 10*time.Minute).Milliseconds() {
			was := st.State
			if checked, err := s.Set.Check(ctx, st.ID); err == nil && checked.State != was && was != "" {
				s.Log.Info("storage location", "store", st.Name, "state", checked.State, "error", checked.Error)
			}
		}
		last := lastAt(st.Scan)
		switch {
		case st.Mode == storage.Source && st.AutoScan && last > 0 && now-last >= every(s.ScanEvery, 6*time.Hour).Milliseconds():
			res, err := s.Imp.ScanSource(ctx, s.Set, st.ID, false)
			if err != nil && !errors.Is(err, importer.ErrScanning) && ctx.Err() == nil {
				s.Log.Warn("source scan", "store", st.Name, "err", err)
			} else if res.New+res.Changed+res.Moved+res.Gone+res.Back > 0 {
				s.Log.Info("source scan", "store", st.Name, "new", res.New, "changed", res.Changed, "moved", res.Moved,
					"gone", res.Gone, "back", res.Back, "held", res.Held)
			}
		case st.Mode == storage.Managed && now-max(last, st.CreatedAt) >= every(s.ReconcileEvery, 24*time.Hour).Milliseconds():
			if _, err := s.Reconcile(ctx, st.ID); err != nil && !errors.Is(err, ErrRunning) && ctx.Err() == nil {
				s.Log.Warn("storage check", "store", st.Name, "err", err)
			}
		}
	}
}

// Reconcile checks that a managed location holds the library's files there, at their sizes:
// songs that are not there are missing, and missing ones that are back are restored. When too
// many are gone at once, the location is more likely not all there than emptied: nothing is
// marked. Only a complete listing marks anything.
func (s *Syncer) Reconcile(ctx context.Context, id int64) (res Result, err error) {
	s.mu.Lock()
	if s.running == nil {
		s.running = map[int64]bool{}
	}
	if s.running[id] {
		s.mu.Unlock()
		return res, ErrRunning
	}
	s.running[id] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, id)
		s.mu.Unlock()
	}()
	res.At = db.Now()
	defer func() {
		if err != nil {
			res.Error = err.Error()
		}
		if e := s.Set.SetScan(context.WithoutCancel(ctx), id, res); e != nil {
			s.Log.Warn("record storage check", "store", id, "err", e)
		}
	}()
	b, st, err := s.Set.Backend(ctx, id)
	if err != nil {
		return res, err
	}
	if st.Mode != storage.Managed {
		return res, errors.New("a source is scanned, not checked")
	}
	if now, err := s.Set.Check(ctx, id); err != nil {
		return res, err
	} else if now.State != storage.StateOK {
		return res, errors.New(now.Error)
	}
	// What the library keeps there is read first: a file an import or a move puts there while it is
	// listed is checked next time, not taken for one gone.
	uses, err := s.Lib.StoreRefs(ctx, st.Kind, id)
	if err != nil {
		return res, err
	}
	there := map[string]int64{}
	if err := b.Walk(ctx, func(o storage.Object) error { there[o.Key] = o.Size; return nil }); err != nil {
		return res, err // a partial listing marks nothing
	}
	res.Files = len(there)
	var gone, back, picsGone, picsBack []string
	for _, u := range uses {
		_, _, key, _ := storage.Parse(u.Ref)
		size, ok := there[key]
		present := ok && (u.Size <= 0 || size == u.Size)
		switch {
		case u.Picture: // missing from its gallery while it is not there (review #246)
			if !present && u.State == library.AssetVerified {
				picsGone = append(picsGone, u.Ref)
			} else if present && u.State == library.AssetMissing {
				picsBack = append(picsBack, u.Ref)
			}
		case u.State == "":
			if !present {
				res.Lost++
			}
		case !present && u.State == library.AssetVerified:
			res.Checked++
			gone = append(gone, u.Ref)
		case present && u.State == library.AssetMissing:
			res.Checked++
			back = append(back, u.Ref)
		default:
			res.Checked++
		}
	}
	// Pictures follow the songs: held with them, and when there are none, when nothing at all is
	// there (review #246).
	if len(gone) > heldMin && len(gone)*2 > res.Checked || len(there) == 0 && len(gone)+len(picsGone) > heldMin {
		res.Held, res.HeldPictures = len(gone), len(picsGone)
		s.Log.Warn("storage check: too many files gone at once; none marked missing", "store", st.Name, "gone", len(gone),
			"pictures", len(picsGone))
		gone, picsGone = nil, nil
	}
	if len(picsGone) > 0 {
		if res.PicturesGone, err = s.Lib.SetImagesMissing(ctx, picsGone, true); err != nil {
			return res, err
		}
		s.Log.Warn("gallery pictures gone from the storage location", "store", st.Name, "count", len(picsGone), "first", picsGone[0])
	}
	if len(picsBack) > 0 {
		if res.PicturesBack, err = s.Lib.SetImagesMissing(ctx, picsBack, false); err != nil {
			return res, err
		}
	}
	if len(gone) > 0 {
		if res.Missing, err = s.Lib.MarkRefsMissing(ctx, gone); err != nil {
			return res, err
		}
		for _, ref := range gone {
			if s.Forget != nil {
				s.Forget(ref)
			}
		}
		s.Log.Warn("library files gone from the storage location", "store", st.Name, "count", res.Missing, "first", gone[0])
	}
	if len(back) > 0 {
		if res.Restored, err = s.Lib.RestoreRefs(ctx, back); err != nil {
			return res, err
		}
	}
	return res, nil
}
