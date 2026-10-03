// Package drivesync keeps the library in step with Drive (P2-6). Every ten minutes the Drive change
// feed marks library files that were deleted, trashed or overwritten with other bytes in Drive as
// missing, and missing ones that came back unchanged as verified (nothing is deleted from the
// library); then the inbox folder is checked for new files (decision D6). A full pass checks every
// library file against a complete listing of the Drive: on request, and on its own whenever the
// change feed starts over (first use, or an expired position), since what happened before the new
// starting point never shows up in the feed.
//
// The feed and full passes take turns: a full pass holds off the feed while it lists and applies,
// and the feed position is taken before the listing, so changes made meanwhile are applied after
// it and an older listing never overrides a newer change.
package drivesync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/library"
)

type Drive interface {
	StartPageToken(ctx context.Context) (string, error)
	Changes(ctx context.Context, token string) ([]gdrive.Change, string, error)
	List(ctx context.Context, q string, fn func([]gdrive.File) error) error
	Trash(ctx context.Context, id string) error
}

const (
	settingToken = "drive_changes_token"
	// settingBaseline is "done" once a full pass has followed the change feed's starting point.
	settingBaseline = "drive_sync_baseline"
	settingLastFull = "drive_sync_last_full" // {"at", "result"} of the last full pass, kept across restarts
)

type Syncer struct {
	DB    *sql.DB
	Drive Drive
	Lib   *library.Store
	Log   *slog.Logger
	// Inbox looks for new files in the inbox folder and imports them; nil to skip.
	Inbox func(ctx context.Context) (int, error)
	// Forget drops what the stream cache holds of a file that is gone or changed; nil to skip.
	Forget func(driveID string)
	Every  time.Duration

	mu     sync.Mutex
	status Status
	full   sync.Mutex // one full pass at a time
	turn   sync.Mutex // the feed and full passes apply what they saw one at a time
}

// Result counts what a pass changed.
type Result struct {
	Checked  int `json:"checked"`
	Missing  int `json:"missing"`  // newly marked missing
	Restored int `json:"restored"` // missing files that are back
}

type Status struct {
	LastChecked int64   `json:"last_checked,omitempty"` // the change feed
	LastFull    int64   `json:"last_full,omitempty"`
	FullRunning bool    `json:"full_running"`
	LastFullRes *Result `json:"last_full_result,omitempty"`
	LastInbox   int64   `json:"last_inbox,omitempty"`
	LastError   string  `json:"last_error,omitempty"`
	// BaselinePending: no full pass has checked the library since the change feed (re)started, so
	// files gone before then are not known yet.
	BaselinePending bool `json:"baseline_pending"`
	// TrashPending counts deleted files still to be moved to the Drive trash (retried).
	TrashPending int    `json:"trash_pending,omitempty"`
	TrashError   string `json:"trash_error,omitempty"`
}

func (s *Syncer) Status() Status {
	s.mu.Lock()
	st := s.status
	s.mu.Unlock()
	ctx := context.Background()
	st.BaselinePending = s.baselinePending(ctx)
	st.TrashPending, st.TrashError, _ = s.Lib.TrashPending(ctx)
	if st.LastFull == 0 {
		var last struct {
			At     int64   `json:"at"`
			Result *Result `json:"result"`
		}
		if v, err := db.GetSetting(ctx, s.DB, settingLastFull); err == nil && v != "" && json.Unmarshal([]byte(v), &last) == nil {
			st.LastFull, st.LastFullRes = last.At, last.Result
		}
	}
	return st
}

func (s *Syncer) baselinePending(ctx context.Context) bool {
	v, err := db.GetSetting(ctx, s.DB, settingBaseline)
	return err != nil || v != "done"
}

func (s *Syncer) set(f func(st *Status)) {
	s.mu.Lock()
	f(&s.status)
	s.mu.Unlock()
}

// Run checks the change feed and the inbox every ten minutes, starting a minute after start-up.
func (s *Syncer) Run(ctx context.Context) {
	every := s.Every
	if every <= 0 {
		every = 10 * time.Minute
	}
	wait := time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = every
		s.RetryTrash(ctx)
		if _, err := s.Changes(ctx); err != nil && ctx.Err() == nil {
			s.Log.Warn("drive changes", "err", err)
		}
		if s.baselinePending(ctx) {
			if _, err := s.Full(ctx); err != nil && !errors.Is(err, ErrRunning) && ctx.Err() == nil {
				s.Log.Warn("drive baseline reconcile", "err", err)
			}
		}
		if s.Inbox != nil {
			n, err := s.Inbox(ctx)
			s.set(func(st *Status) { st.LastInbox = db.Now() })
			if err != nil && ctx.Err() == nil {
				s.Log.Warn("drive inbox", "err", err)
			} else if n > 0 {
				s.Log.Info("drive inbox: import queued", "files", n)
			}
		}
	}
}

// Changes reads the change feed since the last time. Without a position (the first time, or after
// it expired) it takes one and leaves the rest to a full pass (the baseline).
func (s *Syncer) Changes(ctx context.Context) (Result, error) {
	s.turn.Lock()
	defer s.turn.Unlock()
	var res Result
	token, err := s.position(ctx)
	if err != nil || token == "" {
		return res, err
	}
	changes, next, err := s.Drive.Changes(ctx, token)
	if err != nil {
		if gdrive.IsNotFound(err) || isBadRequest(err) { // the position expired: start over, with a new baseline
			db.SetSetting(ctx, s.DB, settingToken, "")
		}
		return res, s.fail(err)
	}
	for _, ch := range changes {
		o := library.DriveObservation{ID: ch.FileID}
		if !ch.Removed && ch.File != nil && !ch.File.Trashed {
			o.Present, o.SHA256, o.Size = true, ch.File.SHA256Checksum, ch.File.SizeBytes()
		}
		n, missing, err := s.Lib.ObserveDriveFile(ctx, o)
		if err != nil {
			return res, err
		}
		if missing && s.Forget != nil {
			s.Forget(o.ID)
		}
		res.Checked++
		if n > 0 && missing {
			res.Missing++
			s.Log.Warn("library file gone or changed in drive", "file", ch.FileID)
		} else if n > 0 {
			res.Restored++
			s.Log.Info("library file back in drive", "file", ch.FileID)
		}
	}
	s.set(func(st *Status) { st.LastChecked, st.LastError = db.Now(), "" })
	return res, db.SetSetting(ctx, s.DB, settingToken, next)
}

// position returns the change feed position, or takes a new one ("" returned) and marks the
// baseline as needed. The caller holds turn.
func (s *Syncer) position(ctx context.Context) (string, error) {
	token, err := db.GetSetting(ctx, s.DB, settingToken)
	if err != nil || token != "" {
		return token, err
	}
	if token, err = s.Drive.StartPageToken(ctx); err != nil {
		return "", s.fail(err)
	}
	if err := db.SetSetting(ctx, s.DB, settingBaseline, ""); err != nil {
		return "", err
	}
	if err := db.SetSetting(ctx, s.DB, settingToken, token); err != nil {
		return "", err
	}
	s.set(func(st *Status) { st.LastChecked = db.Now() })
	return "", nil
}

// RetryTrash moves files the library let go of to the Drive trash, where an earlier try failed.
// trash moves a file to the Drive trash; one Drive no longer has counts as done.
func (s *Syncer) trash(ctx context.Context, id string) error {
	if err := s.Drive.Trash(ctx, id); err != nil && !gdrive.IsNotFound(err) {
		return err
	}
	return nil
}

func (s *Syncer) RetryTrash(ctx context.Context) (done int) {
	ids, err := s.Lib.TrashDue(ctx, 100)
	if err != nil {
		return 0
	}
	for _, id := range ids {
		// Checked again under the trash lock: a re-import may have taken the file up (review #44).
		if n, _ := s.Lib.TrashFile(ctx, id, s.trash); n == library.Trashed {
			done++
		}
	}
	if done > 0 {
		s.Log.Info("deleted files moved to the drive trash", "count", done)
	}
	return done
}

func isBadRequest(err error) bool {
	var e *gdrive.APIError
	return errors.As(err, &e) && e.Status == 400
}

var ErrRunning = errors.New("a full check is already running")

func (s *Syncer) fail(err error) error {
	s.set(func(st *Status) { st.LastError = err.Error() })
	return err
}

// Full checks every library file against a complete listing of what the Drive holds outside the
// trash, comparing content as well. Nothing is marked unless the listing finished. It holds off the
// change feed, and first makes sure the feed has a position from before the listing.
func (s *Syncer) Full(ctx context.Context) (Result, error) {
	if !s.full.TryLock() {
		return Result{}, ErrRunning
	}
	defer s.full.Unlock()
	s.set(func(st *Status) { st.FullRunning = true })
	defer s.set(func(st *Status) { st.FullRunning = false })
	s.turn.Lock()
	defer s.turn.Unlock()

	if _, err := s.position(ctx); err != nil {
		return Result{}, err
	}
	known, err := s.Lib.DriveFiles(ctx)
	if err != nil {
		return Result{}, err
	}
	present := map[string]gdrive.File{}
	err = s.Drive.List(ctx, "trashed = false and mimeType != 'application/vnd.google-apps.folder'", func(fs []gdrive.File) error {
		for _, f := range fs {
			if _, ok := known[f.ID]; ok {
				present[f.ID] = f
			}
		}
		return ctx.Err()
	})
	if err != nil {
		return Result{}, s.fail(err)
	}
	res := Result{Checked: len(known)}
	for id := range known {
		o := library.DriveObservation{ID: id}
		if f, ok := present[id]; ok {
			o.Present, o.SHA256, o.Size = true, f.SHA256Checksum, f.SizeBytes()
		}
		n, missing, err := s.Lib.ObserveDriveFile(ctx, o)
		if err != nil {
			return res, err
		}
		if missing && s.Forget != nil {
			s.Forget(o.ID)
		}
		if n > 0 && missing {
			res.Missing++
		} else if n > 0 {
			res.Restored++
		}
	}
	if err := db.SetSetting(ctx, s.DB, settingBaseline, "done"); err != nil {
		return res, err
	}
	now := db.Now()
	if last, err := json.Marshal(map[string]any{"at": now, "result": res}); err == nil {
		db.SetSetting(ctx, s.DB, settingLastFull, string(last))
	}
	s.set(func(st *Status) { st.LastFull, st.LastFullRes, st.LastError = now, &res, "" })
	s.Log.Info("drive full reconcile", "checked", res.Checked, "missing", res.Missing, "restored", res.Restored)
	return res, nil
}
