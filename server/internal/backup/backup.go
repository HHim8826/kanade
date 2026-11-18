// Package backup keeps copies of the database off the server (review #161). The database is what
// holds the library together (albums and their edits, playlists, favorites, bookmarks, listening,
// the authorizations); the storage locations hold only the files. Once a day a consistent copy
// (VACUUM INTO) goes to the backups folder of Google Drive's Kanade folder or, when chosen, of a
// managed storage location (a library kept elsewhere had no copy off the server), and the latest
// Keep are kept there: older ones go to its trash. A copy holds the authorizations, the locations'
// keys and the password hashes. To restore one, download it next to the data directory's backups
// and use kanade-manager restore.
package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/staging"
	"github.com/HHim8826/kanade/server/internal/storage"
)

// Drive is what a copy needs of Drive.
type Drive interface {
	Folder(ctx context.Context, path string) (string, error)
	Upload(ctx context.Context, u gdrive.Upload) (gdrive.File, error)
	Children(ctx context.Context, folder string) ([]gdrive.File, error)
	Trash(ctx context.Context, id string) error
}

// Places is where else copies can go: managed storage locations (storage.Set).
type Places interface {
	Destination(ctx context.Context, id string) (storage.Dest, error)
	Trash(ctx context.Context, ref string) error
}

const (
	Folder    = "backups" // in Kanade's folder in Drive, at the root of a storage location
	prefix    = "kanade-db-"
	suffix    = ".sqlite"
	lastKey   = "backup.last"   // when the last copy was kept (ms) and its name
	placeKey  = "backup.place"  // where copies go: storage.DriveID (the default) or a managed location's ID
	copiesKey = "backup.copies" // the copies kept in storage locations, to let the old ones go: []stored
)

// stored is a copy in a storage location. Drive's are found by looking in its folder.
type stored struct {
	Place string `json:"place"`
	Ref   string `json:"ref"`
	Name  string `json:"name"`
}

// ErrNoPlace is a place for copies that is not one: not there, or not a managed location.
var ErrNoPlace = errors.New("copies can go to Google Drive or a managed storage location")

// ErrPlaceInUse is the storage location the copies go to: it stays until they go elsewhere (#276).
var ErrPlaceInUse = errors.New("the database backups go to this storage location: choose another place for them first")

// PlaceIs says whether the copies go to the storage location id, read in q (the transaction that
// removes it).
func PlaceIs(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id int64) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM settings WHERE key = ? AND value = ?`, placeKey, strconv.FormatInt(id, 10)).Scan(&n)
	return n > 0, err
}

// firstRun is when Run first looks, once the server has settled (a variable for the tests).
var firstRun = 5 * time.Minute

type Service struct {
	DB     *sql.DB
	Drive  Drive
	Places Places          // nil: Drive alone
	Dir    string          // where a copy is made before it goes up
	Budget *staging.Budget // the copy takes its room from it; nil: none
	Keep   int             // copies kept
	Every  time.Duration   // how often a copy is made
	Log    *slog.Logger

	mu    sync.Mutex
	state State
}

// State is the backups' state: the last copy made, and the last failure since.
type State struct {
	Running bool   `json:"running"`
	Place   string `json:"place"`           // where copies go: "drive" or a storage location's ID
	Where   string `json:"where,omitempty"` // its name
	Last    int64  `json:"last,omitempty"`  // when the last copy was kept (ms)
	Name    string `json:"name,omitempty"`  // its name in the backups folder
	Kept    int    `json:"kept"`            // copies there after the last one, -1 unknown
	Error   string `json:"error,omitempty"`
	// not_connected, auth_expired or drive (gdrive.Explain); room (staging); no_place, offline,
	// store_auth or store_room (the storage location).
	Reason  string `json:"reason,omitempty"`
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
	st.Place = s.place(ctx)
	if st.Place == storage.DriveID {
		st.Where = "Google Drive"
	} else if s.Places != nil {
		if d, err := s.Places.Destination(ctx, st.Place); err == nil {
			st.Where = d.Name()
		}
	}
	return st
}

func (s *Service) place(ctx context.Context) string {
	if v, _ := db.GetSetting(ctx, s.DB, placeKey); v != "" {
		return v
	}
	return storage.DriveID
}

// SetPlace sends the copies from now on to Google Drive (storage.DriveID) or to a managed storage
// location. Copies already made stay where they are.
func (s *Service) SetPlace(ctx context.Context, id string) error {
	if id != storage.DriveID {
		if s.Places == nil {
			return ErrNoPlace
		}
		if _, err := s.Places.Destination(ctx, id); err != nil {
			if errors.Is(err, storage.ErrNoStore) || errors.Is(err, storage.ErrReadOnly) {
				return fmt.Errorf("%w: %v", ErrNoPlace, err)
			}
			return err
		}
	}
	return db.SetSetting(ctx, s.DB, placeKey, id)
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
		switch {
		case errors.Is(err, errNoRoom):
			reason = "room"
		case errors.Is(err, storage.ErrNoStore), errors.Is(err, storage.ErrReadOnly):
			reason = "no_place"
		case errors.Is(err, storage.ErrOffline):
			reason = "offline"
		case errors.Is(err, storage.ErrAuth):
			reason = "store_auth"
		case errors.Is(err, storage.ErrNoRoom):
			reason = "store_room"
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
	// Where it goes, found first: a place that cannot take it fails before the copy is made.
	var put func(path, name, sum string, size int64) error
	var rotate func() (int, error)
	if place := s.place(ctx); place == storage.DriveID {
		folder, err := s.Drive.Folder(ctx, Folder)
		if err != nil {
			return "", -1, err
		}
		put = func(path, name, sum string, size int64) error {
			_, err := s.Drive.Upload(ctx, gdrive.Upload{Path: path, Name: name, ParentID: folder, MIME: "application/vnd.sqlite3",
				Size: size, SHA256: sum, Sessions: &memSessions{}})
			return err
		}
		rotate = func() (int, error) { return s.rotate(ctx, folder) }
	} else {
		if s.Places == nil {
			return "", -1, ErrNoPlace
		}
		dest, err := s.Places.Destination(ctx, place)
		if err != nil {
			return "", -1, err
		}
		var made stored
		put = func(path, name, sum string, size int64) error {
			ref, err := dest.Put(ctx, storage.PutFile{Path: path, Dir: Folder, Name: name, MIME: "application/vnd.sqlite3",
				Size: size, SHA256: sum, Sessions: &memSessions{}})
			if err != nil {
				return err
			}
			made = stored{Place: place, Ref: ref, Name: name}
			return s.keepCopy(ctx, made)
		}
		rotate = func() (int, error) { return s.rotateKept(ctx, place, dest, made) }
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
	if err := put(path, name, sum, n); err != nil {
		return "", -1, fmt.Errorf("uploading the copy: %w", err)
	}
	if err := db.SetSetting(ctx, s.DB, lastKey, strconv.FormatInt(now.UnixMilli(), 10)+" "+name); err != nil {
		return "", -1, err
	}
	kept, err = rotate()
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

func (s *Service) copies(ctx context.Context) ([]stored, error) {
	var list []stored
	v, err := db.GetSetting(ctx, s.DB, copiesKey)
	if err != nil || v == "" {
		return nil, err
	}
	return list, json.Unmarshal([]byte(v), &list)
}

func (s *Service) saveCopies(ctx context.Context, list []stored) error {
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return db.SetSetting(ctx, s.DB, copiesKey, string(b))
}

// keepCopy writes down a copy kept in a storage location.
func (s *Service) keepCopy(ctx context.Context, c stored) error {
	list, err := s.copies(ctx)
	if err != nil {
		return err
	}
	return s.saveCopies(ctx, append(list, c))
}

// lister is a place that says which files are in one of its folders (storage.Set's).
type lister interface {
	Files(ctx context.Context, dir string) ([]storage.Listed, error)
}

// copyName is a copy's name, as made: other files in the folder are not copies.
var copyName = regexp.MustCompile(`^kanade-db-\d{8}-\d{6}( \(\d+\))?\.sqlite$`)

// rotateKept moves the copies in place beyond the Keep latest to its trash, and says how many are
// kept. The copies there are those found in its backups folder, when it can say: a database restored
// from a copy wrote down only the copies made before it (#279); the one just made (made) is among
// them even if the folder does not show it yet. Other files there stay, and copies elsewhere (a
// place used before) stay, as written down.
func (s *Service) rotateKept(ctx context.Context, place string, dest storage.Dest, made stored) (int, error) {
	list, err := s.copies(ctx)
	if err != nil {
		return -1, err
	}
	var here, others []stored
	for _, c := range list {
		if c.Place == place {
			here = append(here, c)
		} else {
			others = append(others, c)
		}
	}
	if l, ok := dest.(lister); ok {
		found, err := l.Files(ctx, Folder)
		if err != nil {
			s.Log.Warn("database backup: reading the backups folder", "err", err) // the copies written down, then
		} else {
			here = here[:0]
			listed := false
			for _, f := range found {
				if copyName.MatchString(f.Name) {
					here = append(here, stored{Place: place, Ref: f.Ref, Name: f.Name})
					listed = listed || f.Ref == made.Ref
				}
			}
			if !listed && made.Ref != "" {
				here = append(here, made)
			}
		}
	}
	sort.Slice(here, func(i, j int) bool { return here[i].Name > here[j].Name }) // the names sort by time
	keep := here[:min(s.Keep, len(here))]
	for _, c := range here[len(keep):] {
		if err := s.Places.Trash(ctx, c.Ref); err != nil {
			keep = append(keep, c) // tried again after the next copy
		}
	}
	if err := s.saveCopies(ctx, append(others, keep...)); err != nil {
		return -1, err
	}
	return min(s.Keep, len(here)), nil
}

// memSessions keeps an upload session only for the one upload.
type memSessions struct{ uri string }

func (m *memSessions) LoadSession(context.Context) (string, error)     { return m.uri, nil }
func (m *memSessions) SaveSession(_ context.Context, uri string) error { m.uri = uri; return nil }
func (m *memSessions) DeleteSession(context.Context) error             { m.uri = ""; return nil }
