// Package drivesync keeps the library in step with Drive (P2-6). Every ten minutes the Drive change
// feed marks library files that were deleted or trashed in Drive as missing, and missing ones that
// came back as verified (nothing is deleted from the library); then the inbox folder is checked for
// new files (decision D6). A full pass, on request, checks every library file against a complete
// listing of the Drive.
package drivesync

import (
	"context"
	"database/sql"
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
}

const settingToken = "drive_changes_token"

type Syncer struct {
	DB    *sql.DB
	Drive Drive
	Lib   *library.Store
	Log   *slog.Logger
	// Inbox looks for new files in the inbox folder and imports them; nil to skip.
	Inbox func(ctx context.Context) (int, error)
	Every time.Duration

	mu     sync.Mutex
	status Status
	full   sync.Mutex // one full pass at a time
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
}

func (s *Syncer) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
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
		if _, err := s.Changes(ctx); err != nil && ctx.Err() == nil {
			s.Log.Warn("drive changes", "err", err)
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

// Changes reads the change feed since the last time. The first call only takes the starting point.
func (s *Syncer) Changes(ctx context.Context) (Result, error) {
	var res Result
	token, err := db.GetSetting(ctx, s.DB, settingToken)
	if err != nil {
		return res, err
	}
	if token == "" {
		if token, err = s.Drive.StartPageToken(ctx); err != nil {
			return res, s.fail(err)
		}
		s.set(func(st *Status) { st.LastChecked = db.Now() })
		return res, db.SetSetting(ctx, s.DB, settingToken, token)
	}
	changes, next, err := s.Drive.Changes(ctx, token)
	if err != nil {
		if gdrive.IsNotFound(err) || isBadRequest(err) { // the token expired: start over (a full pass finds what was missed)
			db.SetSetting(ctx, s.DB, settingToken, "")
		}
		return res, s.fail(err)
	}
	for _, ch := range changes {
		gone := ch.Removed || ch.File == nil || ch.File.Trashed
		changed, err := s.Lib.MarkDriveFile(ctx, ch.FileID, gone)
		if err != nil {
			return res, err
		}
		res.Checked++
		if changed && gone {
			res.Missing++
			s.Log.Warn("library file gone from drive", "file", ch.FileID)
		} else if changed {
			res.Restored++
			s.Log.Info("library file back in drive", "file", ch.FileID)
		}
	}
	s.set(func(st *Status) { st.LastChecked, st.LastError = db.Now(), "" })
	return res, db.SetSetting(ctx, s.DB, settingToken, next)
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
// trash. Nothing is marked unless the listing finished.
func (s *Syncer) Full(ctx context.Context) (Result, error) {
	if !s.full.TryLock() {
		return Result{}, ErrRunning
	}
	defer s.full.Unlock()
	s.set(func(st *Status) { st.FullRunning = true })
	defer s.set(func(st *Status) { st.FullRunning = false })

	known, err := s.Lib.DriveFiles(ctx)
	if err != nil {
		return Result{}, err
	}
	present := map[string]bool{}
	err = s.Drive.List(ctx, "trashed = false and mimeType != 'application/vnd.google-apps.folder'", func(fs []gdrive.File) error {
		for _, f := range fs {
			present[f.ID] = true
		}
		return ctx.Err()
	})
	if err != nil {
		return Result{}, s.fail(err)
	}
	res := Result{Checked: len(known)}
	for id, state := range known {
		switch {
		case present[id] && state == library.AssetMissing:
			if ok, err := s.Lib.MarkDriveFile(ctx, id, false); err != nil {
				return res, err
			} else if ok {
				res.Restored++
			}
		case !present[id] && state == library.AssetVerified:
			if ok, err := s.Lib.MarkDriveFile(ctx, id, true); err != nil {
				return res, err
			} else if ok {
				res.Missing++
			}
		}
	}
	s.set(func(st *Status) { st.LastFull, st.LastFullRes, st.LastError = db.Now(), &res, "" })
	s.Log.Info("drive full reconcile", "checked", res.Checked, "missing", res.Missing, "restored", res.Restored)
	return res, nil
}
