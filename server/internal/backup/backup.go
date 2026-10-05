// Package backup keeps copies of the database in Drive (review #161). The database is what holds
// the library together (albums and their edits, playlists, favorites, bookmarks, listening, the
// Drive authorization); Drive itself holds only the files. Once a day a consistent copy (VACUUM
// INTO) goes to Kanade/backups, and the latest Keep are kept: older ones go to Drive's trash. A
// copy holds the Google authorization and the password hashes, in the Google account the music is
// in. To restore one, download it next to the data directory's backups and use kanade-manager
// restore.
package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/staging"
)

// Drive is what a copy needs of Drive.
type Drive interface {
	Folder(ctx context.Context, path string) (string, error)
	Upload(ctx context.Context, u gdrive.Upload) (gdrive.File, error)
	Children(ctx context.Context, folder string) ([]gdrive.File, error)
	Trash(ctx context.Context, id string) error
}

const (
	Folder  = "backups" // in Kanade's folder in Drive
	prefix  = "kanade-db-"
	suffix  = ".sqlite"
	lastKey = "backup.last" // when the last copy went to Drive (ms) and its name
)

// firstRun is when Run first looks, once the server has settled (a variable for the tests).
var firstRun = 5 * time.Minute

type Service struct {
	DB     *sql.DB
	Drive  Drive
	Dir    string          // where a copy is made before it goes up
	Budget *staging.Budget // the copy takes its room from it; nil: none
	Keep   int             // copies kept in Drive
	Every  time.Duration   // how often a copy is made
	Log    *slog.Logger

	mu    sync.Mutex
	state State
}

// State is the backups' state: the last copy made, and the last failure since.
type State struct {
	Running bool   `json:"running"`
	Last    int64  `json:"last,omitempty"` // when the last copy went to Drive (ms)
	Name    string `json:"name,omitempty"` // its name in Kanade/backups
	Kept    int    `json:"kept"`           // copies in Drive after the last one, -1 unknown
	Error   string `json:"error,omitempty"`
	Reason  string `json:"reason,omitempty"` // not_connected, auth_expired or drive (gdrive.Explain), or room
	ErrorAt int64  `json:"error_at,omitempty"`
	Keep    int    `json:"keep"`
}

// State is now's.
func (s *Service) State(ctx context.Context) State {
	s.mu.Lock()
	st := s.state
	s.mu.Unlock()
	st.Last, st.Name = s.last(ctx)
	st.Keep = s.Keep
	return st
}

func (s *Service) last(ctx context.Context) (int64, string) {
	v, _ := db.GetSetting(ctx, s.DB, lastKey)
	at, name, _ := strings.Cut(v, " ")
	n, _ := strconv.ParseInt(at, 10, 64)
	return n, name
}

// Run makes a copy whenever the last one is Every old (looked at every hour, the first time a few
// minutes after starting), until ctx ends.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTimer(firstRun)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if last, _ := s.last(ctx); time.Since(time.UnixMilli(last)) >= s.Every {
			s.Now(ctx)
		}
		t.Reset(time.Hour)
	}
}

// Start makes a copy now, in the background; false when one is being made. It is running once Start
// returns, so the state read right after says so (review #180).
func (s *Service) Start(ctx context.Context) bool {
	if !s.begin() {
		return false
	}
	go s.make(context.WithoutCancel(ctx))
	return true
}

// Now makes a copy and keeps it in Drive, unless one is being made; the error is also in State.
func (s *Service) Now(ctx context.Context) error {
	if !s.begin() {
		return nil
	}
	return s.make(ctx)
}

// begin marks a copy as being made; false when one is already.
func (s *Service) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Running {
		return false
	}
	s.state.Running = true
	return true
}

// make makes the copy begun, and says how it went.
func (s *Service) make(ctx context.Context) error {
	name, kept, err := s.copy(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Running = false
	if err != nil {
		reason, _ := gdrive.Explain(err)
		if errors.Is(err, errNoRoom) {
			reason = "room"
		}
		if err.Error() != s.state.Error { // one line for a failure that goes on
			s.Log.Warn("database backup", "err", err)
		}
		s.state.Error, s.state.Reason, s.state.ErrorAt = err.Error(), reason, time.Now().UnixMilli()
		return err
	}
	s.state.Error, s.state.Reason, s.state.ErrorAt, s.state.Kept = "", "", 0, kept
	s.Log.Info("database backup", "name", name, "kept", kept)
	return nil
}

var errNoRoom = errors.New("no staging room for the copy")

func (s *Service) copy(ctx context.Context) (name string, kept int, err error) {
	folder, err := s.Drive.Folder(ctx, Folder)
	if err != nil {
		return "", -1, err
	}
	var size int64
	if err := s.DB.QueryRowContext(ctx, `SELECT page_count * page_size FROM pragma_page_count(), pragma_page_size()`).Scan(&size); err != nil {
		return "", -1, err
	}
	if s.Budget != nil {
		release, err := s.Budget.HoldRequest(ctx, staging.Request{Need: size, Alone: true})
		if err != nil {
			return "", -1, fmt.Errorf("%w: %v", errNoRoom, err)
		}
		defer release()
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return "", -1, err
	}
	now := time.Now().UTC()
	name = prefix + now.Format("20060102-150405") + suffix
	path := filepath.Join(s.Dir, name)
	os.Remove(path) // VACUUM INTO wants a new file
	defer os.Remove(path)
	if _, err := s.DB.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return "", -1, fmt.Errorf("copying the database: %w", err)
	}
	if err := check(ctx, path); err != nil {
		return "", -1, err
	}
	sum, n, err := hashFile(path)
	if err != nil {
		return "", -1, err
	}
	if _, err := s.Drive.Upload(ctx, gdrive.Upload{Path: path, Name: name, ParentID: folder, MIME: "application/vnd.sqlite3",
		Size: n, SHA256: sum, Sessions: &memSessions{}}); err != nil {
		return "", -1, fmt.Errorf("uploading the copy: %w", err)
	}
	if err := db.SetSetting(ctx, s.DB, lastKey, strconv.FormatInt(now.UnixMilli(), 10)+" "+name); err != nil {
		return "", -1, err
	}
	kept, err = s.rotate(ctx, folder)
	if err != nil {
		s.Log.Warn("database backup: old copies", "err", err) // the copy is there all the same
		kept = -1
	}
	return name, kept, nil
}

// check reads the copy back as a database.
func check(ctx context.Context, path string) error {
	d, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return err
	}
	defer d.Close()
	var ok string
	if err := d.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&ok); err != nil {
		return fmt.Errorf("checking the copy: %w", err)
	}
	if ok != "ok" {
		return fmt.Errorf("the copy is damaged: %s", ok)
	}
	return nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// rotate moves the copies beyond the Keep latest to Drive's trash, and says how many are kept.
// Only copies by their name: other files in the folder stay.
func (s *Service) rotate(ctx context.Context, folder string) (int, error) {
	files, err := s.Drive.Children(ctx, folder)
	if err != nil {
		return -1, err
	}
	var copies []gdrive.File
	for _, f := range files {
		if strings.HasPrefix(f.Name, prefix) && strings.HasSuffix(f.Name, suffix) && !f.Trashed {
			copies = append(copies, f)
		}
	}
	sort.Slice(copies, func(i, j int) bool { return copies[i].Name > copies[j].Name }) // the names sort by time
	for _, f := range copies[min(s.Keep, len(copies)):] {
		if err := s.Drive.Trash(ctx, f.ID); err != nil {
			return -1, err
		}
	}
	return min(s.Keep, len(copies)), nil
}

// memSessions keeps an upload session only for the one upload.
type memSessions struct{ uri string }

func (m *memSessions) LoadSession(context.Context) (string, error)     { return m.uri, nil }
func (m *memSessions) SaveSession(_ context.Context, uri string) error { m.uri = uri; return nil }
func (m *memSessions) DeleteSession(context.Context) error             { m.uri = ""; return nil }
