package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/gdrive"
)

// Drive is what the Set needs of *gdrive.Client.
type Drive interface {
	OpenRange(ctx context.Context, id string, start, end int64) (*http.Response, error)
	Folder(ctx context.Context, path string) (string, error)
	Upload(ctx context.Context, u gdrive.Upload) (gdrive.File, error)
	Trash(ctx context.Context, id string) error
}

// Store is a stores row.
type Store struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	Name      string          `json:"name"`
	Mode      string          `json:"mode"`
	Config    json.RawMessage `json:"config"`
	State     string          `json:"state"` // the last check: ok | offline | error; "" before the first
	Error     string          `json:"error,omitempty"`
	CheckedAt int64           `json:"checked_at,omitempty"`
	Usage     *Usage          `json:"usage,omitempty"`
	AutoScan  bool            `json:"auto_scan"`
	Scan      json.RawMessage `json:"scan,omitempty"`
	Category  int64           `json:"category,omitempty"` // a source: the category its albums go in (#92)
	CreatedAt int64           `json:"created_at"`
	secret    string
}

const (
	StateOK      = "ok"
	StateOffline = "offline"
	StateError   = "error"

	settingDefault = "storage_default"
)

// Set is every place the library's files are: Google Drive and the stores. It reads any reference,
// writes where imports go, and keeps the stores' records.
type Set struct {
	DB      *sql.DB
	Drive   Drive // nil: Google Drive is not used (tests)
	Log     *slog.Logger
	DataDir string // a folder store may be inside it, but not in what Kanade keeps there
	// Reserve is the free space a write must leave on the disk the data directory is on (the disk
	// guard's reserve); nil: none.
	Reserve func() int64
	// Forget is told of a source's file the library let go of: scans leave it alone until it changes.
	Forget func(ctx context.Context, store int64, key string) error
	HTTP   *http.Client // for buckets and OneDrive; nil: http.DefaultClient

	mu    sync.Mutex
	built map[int64]built
}

type built struct {
	b     Backend
	stamp string // what it was built from: built again when the row changes
}

// reserved are the data directory's own folders: a folder store may not be one or be in one.
var reserved = []string{"staging", "downloads", "cache", "thumbs", "lite", "imports", "aria2", "logs", "backups"}

func (s *Set) httpClient() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return storeClient
}

// storeClient reaches buckets and OneDrive: a server that takes a request and does not answer it
// is given up on (and the request tried again as a failure to connect would be), instead of
// holding it, and whatever waits on it, for good.
var storeClient = &http.Client{Transport: func() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 2 * time.Minute // a copy of a large object is answered when it is done
	return t
}()}

// tryFor bounds trying a new bucket's keys or a OneDrive app's client secret: someone waits on it.
var tryFor = 45 * time.Second

// tried says what became of trying a store that did not answer in time.
func tried(err error, where string) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return offline("%s did not answer within %d seconds", where, int(tryFor.Seconds()))
	}
	return err
}

const storeCols = `id, kind, name, mode, config, secret, state, error, checked_at, usage, auto_scan, scan, coalesce(category, 0), created_at`

func scanStore(row interface{ Scan(...any) error }) (*Store, error) {
	var st Store
	var config, usage, scan string
	if err := row.Scan(&st.ID, &st.Kind, &st.Name, &st.Mode, &config, &st.secret, &st.State, &st.Error, &st.CheckedAt,
		&usage, &st.AutoScan, &scan, &st.Category, &st.CreatedAt); err != nil {
		return nil, err
	}
	st.Config = json.RawMessage(config)
	if usage != "" {
		st.Usage = &Usage{}
		json.Unmarshal([]byte(usage), st.Usage)
	}
	if scan != "" {
		st.Scan = json.RawMessage(scan)
	}
	return &st, nil
}

// Stores lists the stores, oldest first.
func (s *Set) Stores(ctx context.Context) ([]Store, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+storeCols+` FROM stores ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Store{}
	for rows.Next() {
		st, err := scanStore(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *st)
	}
	return out, rows.Err()
}

// Get is one store, nil when there is none.
func (s *Set) Get(ctx context.Context, id int64) (*Store, error) {
	st, err := scanStore(s.DB.QueryRowContext(ctx, `SELECT `+storeCols+` FROM stores WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return st, err
}

// Backend is a store's, made from its row (again when the row changed).
func (s *Set) Backend(ctx context.Context, id int64) (Backend, *Store, error) {
	st, err := s.Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if st == nil {
		return nil, nil, ErrNoStore
	}
	stamp := st.Kind + "\x00" + st.Mode + "\x00" + st.Name + "\x00" + string(st.Config)
	if st.Kind != KindOneDrive { // a OneDrive's refresh token changes as it is used: it reads it as kept
		stamp += "\x00" + st.secret
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.built[id]; ok && b.stamp == stamp {
		return b.b, st, nil
	}
	b, err := s.build(st)
	if err != nil {
		return nil, st, err
	}
	if s.built == nil {
		s.built = map[int64]built{}
	}
	s.built[id] = built{b, stamp}
	return b, st, nil
}

func (s *Set) build(st *Store) (Backend, error) {
	switch st.Kind {
	case KindLocal:
		var cfg LocalConfig
		if err := json.Unmarshal(st.Config, &cfg); err != nil {
			return nil, fmt.Errorf("store %d: %w", st.ID, err)
		}
		return newLocal(st.Name, st.Mode, cfg, s.reserveFor), nil
	case KindS3:
		var cfg S3Config
		var sec S3Secret
		if err := json.Unmarshal(st.Config, &cfg); err != nil {
			return nil, fmt.Errorf("store %d: %w", st.ID, err)
		}
		json.Unmarshal([]byte(st.secret), &sec)
		return newS3(st.Name, st.Mode, cfg, sec, s.httpClient())
	case KindOneDrive:
		var cfg OneDriveConfig
		if err := json.Unmarshal(st.Config, &cfg); err != nil {
			return nil, fmt.Errorf("store %d: %w", st.ID, err)
		}
		id := st.ID
		return newOneDrive(st.Name, st.Mode, cfg, s.httpClient(),
			func(ctx context.Context) (OneDriveSecret, error) { return s.oneDriveSecret(ctx, id) },
			func(ctx context.Context, old, new string) (bool, error) { return s.rotateRefresh(ctx, id, old, new) }), nil
	}
	return nil, fmt.Errorf("store %d: unknown kind %q", st.ID, st.Kind)
}

// reserveFor is the free space a write to dir must leave: the disk guard's reserve on the data
// directory's disk, a small margin elsewhere.
func (s *Set) reserveFor(dir string) int64 {
	if s.Reserve != nil && s.DataDir != "" && sameDevice(dir, s.DataDir) {
		return max(s.Reserve(), smallMargin)
	}
	return smallMargin
}

// resolve is the store and key a reference names.
func (s *Set) resolve(ctx context.Context, ref string) (Backend, *Store, string, error) {
	kind, id, key, ok := Parse(ref)
	if !ok {
		return nil, nil, "", fmt.Errorf("%w: %q", ErrNoStore, ref)
	}
	b, st, err := s.Backend(ctx, id)
	if err != nil {
		return nil, st, "", err
	}
	if st.Kind != kind {
		return nil, st, "", fmt.Errorf("%w: %q", ErrNoStore, ref)
	}
	return b, st, key, nil
}

func named(st *Store, err error) error {
	if err == nil || st == nil {
		return err
	}
	var e *Error
	if errors.As(err, &e) {
		return err
	}
	return &Error{Store: st.Name, Err: err}
}

// Open reads bytes [start, end] (end < 0: to the end) of the file a reference names.
func (s *Set) Open(ctx context.Context, ref string, start, end int64) (io.ReadCloser, error) {
	if IsDrive(ref) {
		if s.Drive == nil {
			return nil, ErrNoStore
		}
		resp, err := s.Drive.OpenRange(ctx, ref, start, end)
		if err != nil {
			return nil, err
		}
		return resp.Body, nil
	}
	b, st, key, err := s.resolve(ctx, ref)
	if err != nil {
		return nil, named(st, err)
	}
	rc, err := b.Open(ctx, key, start, end)
	return rc, named(st, err)
}

// OpenRange is Open as the stream cache and Drive readers take it.
func (s *Set) OpenRange(ctx context.Context, ref string, start, end int64) (*http.Response, error) {
	if IsDrive(ref) && s.Drive != nil {
		return s.Drive.OpenRange(ctx, ref, start, end)
	}
	rc, err := s.Open(ctx, ref, start, end)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: http.StatusPartialContent, Body: rc}, nil
}

// LocalFile opens a file of a folder store, to be read where it is rather than through the stream
// cache; ok is false for a file elsewhere.
func (s *Set) LocalFile(ctx context.Context, ref string) (f *os.File, ok bool, err error) {
	l, st, key, err := s.local(ctx, ref)
	if l == nil {
		return nil, false, err
	}
	f, err = l.File(key)
	return f, true, named(st, err)
}

// LocalPath is the path of a file of a folder store on the server; ok is false for a file elsewhere.
func (s *Set) LocalPath(ctx context.Context, ref string) (p string, ok bool, err error) {
	l, st, key, err := s.local(ctx, ref)
	if l == nil {
		return "", false, err
	}
	p, err = l.Path(key)
	return p, true, named(st, err)
}

func (s *Set) local(ctx context.Context, ref string) (*Local, *Store, string, error) {
	if kind, _, _, ok := Parse(ref); !ok || kind != KindLocal {
		return nil, nil, "", nil
	}
	b, st, key, err := s.resolve(ctx, ref)
	if err != nil {
		return nil, st, "", named(st, err)
	}
	l, _ := b.(*Local)
	return l, st, key, nil
}

// Stat says what a store holds at a reference (not for Drive's).
func (s *Set) Stat(ctx context.Context, ref string) (Object, error) {
	b, st, key, err := s.resolve(ctx, ref)
	if err != nil {
		return Object{}, named(st, err)
	}
	o, err := b.Stat(ctx, key)
	return o, named(st, err)
}

// FileName is the name a library file has where it is kept: what an import named it (its file's
// name; a name next to it when that was taken).
func (s *Set) FileName(ctx context.Context, ref string) (string, error) {
	if IsDrive(ref) {
		d, ok := s.Drive.(interface {
			GetFile(ctx context.Context, id, fields string) (gdrive.File, error)
		})
		if !ok {
			return "", ErrNoStore
		}
		f, err := d.GetFile(ctx, ref, "name")
		return f.Name, err
	}
	o, err := s.Stat(ctx, ref)
	if err != nil {
		return "", err
	}
	return path.Base(o.Path), nil
}

// Name is the name of the place a reference is in, for people: "Google Drive" or the store's.
func (s *Set) Name(ctx context.Context, ref string) string {
	if p := s.Place(ctx, ref); p != nil {
		return p.Name
	}
	return ""
}

// Place is where a library file is, for people.
type Place struct {
	ID   string `json:"id"` // "drive" or the store's ID
	Kind string `json:"kind"`
	Name string `json:"name"`
	Mode string `json:"mode"`
}

// Place says where a reference is; nil for none, or a store since removed.
func (s *Set) Place(ctx context.Context, ref string) *Place {
	if IsDrive(ref) {
		return &Place{ID: DriveID, Kind: KindDrive, Name: "Google Drive", Mode: Managed}
	}
	if _, id, _, ok := Parse(ref); ok {
		if st, err := s.Get(ctx, id); err == nil && st != nil {
			return &Place{ID: strconv.FormatInt(st.ID, 10), Kind: st.Kind, Name: st.Name, Mode: st.Mode}
		}
	}
	return nil
}

// ---- where imports go ----

// Dest is where an import keeps a file: Google Drive or a managed store.
type Dest interface {
	ID() string // DriveID or the store's ID
	Name() string
	// Put keeps f and returns the reference to the stored copy, known to hold f's bytes.
	Put(ctx context.Context, f PutFile) (ref string, err error)
}

// Default is where new imports go: DriveID or a managed store's ID.
func (s *Set) Default(ctx context.Context) (string, error) {
	v, err := db.GetSetting(ctx, s.DB, settingDefault)
	if err != nil || v == "" {
		return DriveID, err
	}
	return v, nil
}

// SetDefault sends new imports to Google Drive (DriveID) or to a managed store. Files already in
// the library stay where they are.
func (s *Set) SetDefault(ctx context.Context, id string) error {
	if id != DriveID {
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return ErrNoStore
		}
		st, err := s.Get(ctx, n)
		if err != nil {
			return err
		}
		if st == nil {
			return ErrNoStore
		}
		if st.Mode != Managed {
			return ErrReadOnly
		}
	}
	return db.SetSetting(ctx, s.DB, settingDefault, id)
}

// Destination is the place with ID id (DriveID or a store's), or the default one for "".
func (s *Set) Destination(ctx context.Context, id string) (Dest, error) {
	if id == "" {
		var err error
		if id, err = s.Default(ctx); err != nil {
			return nil, err
		}
	}
	if id == DriveID {
		if s.Drive == nil {
			return nil, ErrNoStore
		}
		return driveDest{s.Drive}, nil
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", ErrNoStore, id)
	}
	b, st, err := s.Backend(ctx, n)
	if err != nil {
		return nil, named(st, err)
	}
	w, ok := b.(Writer)
	if !ok || st.Mode != Managed {
		return nil, named(st, ErrReadOnly)
	}
	return storeDest{st, w}, nil
}

type driveDest struct{ d Drive }

func (driveDest) ID() string   { return DriveID }
func (driveDest) Name() string { return "Google Drive" }

func (d driveDest) Put(ctx context.Context, f PutFile) (string, error) {
	folder, err := d.d.Folder(ctx, f.Dir)
	if err != nil {
		return "", err
	}
	file, err := d.d.Upload(ctx, gdrive.Upload{Path: f.Path, Name: f.Name, ParentID: folder, MIME: f.MIME,
		Size: f.Size, SHA256: f.SHA256, Sessions: f.Sessions, Progress: f.Progress})
	if err != nil {
		return "", err
	}
	return file.ID, nil
}

type storeDest struct {
	st *Store
	w  Writer
}

func (d storeDest) ID() string   { return strconv.FormatInt(d.st.ID, 10) }
func (d storeDest) Name() string { return d.st.Name }

func (d storeDest) Put(ctx context.Context, f PutFile) (string, error) {
	key, err := d.w.Put(ctx, f)
	if err != nil {
		return "", named(d.st, err)
	}
	return Ref(d.st.Kind, d.st.ID, key), nil
}

// ---- letting go ----

// Trash moves a file the library let go of to its place's trash: Drive's, or a managed store's. A
// source's file is never touched: it is only left out of the source's scans from then on (Forget).
// A file already gone, or in a store that was removed, is done.
func (s *Set) Trash(ctx context.Context, ref string) error {
	if IsDrive(ref) {
		if s.Drive == nil {
			return ErrNoStore
		}
		if err := s.Drive.Trash(ctx, ref); err != nil && !gdrive.IsNotFound(err) {
			return err
		}
		return nil
	}
	b, st, key, err := s.resolve(ctx, ref)
	if errors.Is(err, ErrNoStore) {
		return nil
	}
	if err != nil {
		return named(st, err)
	}
	if st.Mode == Source {
		if s.Forget != nil {
			return s.Forget(ctx, st.ID, key)
		}
		return nil
	}
	w, ok := b.(Writer)
	if !ok {
		return nil
	}
	if err := w.Trash(ctx, key); err != nil && !errors.Is(err, ErrNotFound) {
		return named(st, err)
	}
	return nil
}

// ---- the stores' records ----

// NewStore is a store to add. Config is the kind's: {"root"} for a folder.
type NewStore struct {
	Kind   string          `json:"kind"`
	Name   string          `json:"name"`
	Mode   string          `json:"mode"`
	Config json.RawMessage `json:"config"`
	Secret json.RawMessage `json:"secret,omitempty"`
	// Category is where a source's albums are put (#92); 0: none.
	Category int64 `json:"category,omitempty"`
}

// ErrInvalid is a store that cannot be added as asked; the message says why.
type ErrInvalid struct{ Msg string }

func (e *ErrInvalid) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &ErrInvalid{fmt.Sprintf(format, a...)} }

// Add checks a new store and records it.
func (s *Set) Add(ctx context.Context, n NewStore) (*Store, error) {
	n.Name = strings.TrimSpace(n.Name)
	if n.Name == "" || len([]rune(n.Name)) > 60 {
		return nil, invalid("a name of 1 to 60 characters is needed")
	}
	if n.Mode != Managed && n.Mode != Source {
		return nil, invalid("mode must be managed or source")
	}
	var config, secret []byte
	switch n.Kind {
	case KindLocal:
		var in struct{ Root string }
		if err := json.Unmarshal(n.Config, &in); err != nil {
			return nil, invalid("config: %v", err)
		}
		root := filepath.Clean(strings.TrimSpace(in.Root))
		if err := s.checkFolder(ctx, root, 0); err != nil {
			return nil, err
		}
		// A folder of its own in the data directory is made here (that disk is always there); any
		// other must exist already, so a NAS that is not mounted is never written under.
		if n.Mode == Managed && s.DataDir != "" && within(root, filepath.Clean(s.DataDir)) {
			if err := os.MkdirAll(root, 0o700); err != nil {
				return nil, invalid("cannot make %s: %v", root, err)
			}
		}
		cfg, err := PrepareLocal(root, n.Mode)
		if err != nil {
			return nil, invalid("%v", err)
		}
		config, _ = json.Marshal(cfg)
	case KindS3:
		var err error
		if n.Config, err = r2Bucket(n.Config); err != nil {
			return nil, err
		}
		cfg, sec, err := s.checkBucket(ctx, n, 0)
		if err != nil {
			return nil, err
		}
		config, _ = json.Marshal(cfg)
		secret, _ = json.Marshal(sec)
	case KindOneDrive:
		var cfg OneDriveConfig
		var sec OneDriveSecret
		json.Unmarshal(n.Config, &cfg)
		json.Unmarshal(n.Secret, &sec)
		cfg = OneDriveConfig{ClientID: strings.TrimSpace(cfg.ClientID), Region: strings.TrimSpace(cfg.Region), Tenant: strings.TrimSpace(cfg.Tenant)}
		if cfg.Tenant == "" {
			cfg.Tenant = "common"
		}
		if cfg.Region == "global" {
			cfg.Region = ""
		}
		if _, ok := odClouds[cfg.Region]; !ok && cfg.Region != "" {
			return nil, invalid("unknown Microsoft cloud %q: global, cn or us", cfg.Region)
		}
		sec = OneDriveSecret{ClientSecret: strings.TrimSpace(sec.ClientSecret)}
		if cfg.ClientID == "" || sec.ClientSecret == "" {
			return nil, invalid("the app's client ID and a client secret are both needed")
		}
		config, _ = json.Marshal(cfg)
		secret, _ = json.Marshal(sec)
	default:
		return nil, invalid("unknown kind %q", n.Kind)
	}
	if err := s.checkCategory(ctx, n.Mode, n.Category); err != nil {
		return nil, err
	}
	r, err := s.DB.ExecContext(ctx, `INSERT INTO stores (kind, name, mode, config, secret, category, created_at) VALUES (?, ?, ?, ?, ?, nullif(?, 0), ?)`,
		n.Kind, n.Name, n.Mode, string(config), string(secret), n.Category, db.Now())
	if err != nil {
		return nil, err
	}
	id, _ := r.LastInsertId()
	return s.Check(ctx, id)
}

// checkFolder refuses a folder that overlaps the data directory's own folders or another folder
// store (except store except): cleaning staging or the cache must never touch the library, and no
// file may belong to two stores.
func (s *Set) checkFolder(ctx context.Context, root string, except int64) error {
	if !filepath.IsAbs(root) {
		return invalid("the folder must be an absolute path (starting with /)")
	}
	if s.DataDir != "" {
		data := filepath.Clean(s.DataDir)
		if root == data || within(data, root) {
			return invalid("%s holds Kanade's data folder (%s); choose a folder of its own", root, data)
		}
		if rel, err := filepath.Rel(data, root); err == nil && filepath.IsLocal(rel) {
			top := strings.Split(filepath.ToSlash(rel), "/")[0]
			for _, r := range reserved {
				if top == r {
					return invalid("%s is in Kanade's %s folder, which is emptied as it works; choose another folder", root, r)
				}
			}
		}
	}
	stores, err := s.Stores(ctx)
	if err != nil {
		return err
	}
	for _, st := range stores {
		if st.Kind != KindLocal || st.ID == except {
			continue
		}
		var cfg LocalConfig
		json.Unmarshal(st.Config, &cfg)
		if root == cfg.Root || within(root, cfg.Root) || within(cfg.Root, root) {
			return invalid("%s overlaps the folder of 「%s」 (%s)", root, st.Name, cfg.Root)
		}
	}
	return nil
}

// within says whether p is below dir.
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != "." && filepath.IsLocal(rel)
}

// Check looks at a store now and records what it found: whether it can be used, and its room.
func (s *Set) Check(ctx context.Context, id int64) (*Store, error) {
	b, st, err := s.Backend(ctx, id)
	if err != nil {
		if st == nil {
			return nil, err
		}
		s.record(ctx, id, nil, err)
		return s.Get(ctx, id)
	}
	u, err := b.Check(ctx)
	s.record(ctx, id, &u, named(st, err))
	return s.Get(ctx, id)
}

func (s *Set) record(ctx context.Context, id int64, u *Usage, err error) {
	state, msg := StateOK, ""
	if err != nil {
		state, msg = StateError, err.Error()
		if errors.Is(err, ErrOffline) {
			state = StateOffline
		}
	}
	usage := ""
	if u != nil && (u.Total > 0 || u.Free > 0 || u.Used > 0 || u.Files > 0) {
		b, _ := json.Marshal(u)
		usage = string(b)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE stores SET state = ?, error = ?, checked_at = ?, usage = CASE WHEN ? = '' THEN usage ELSE ? END
		WHERE id = ?`, state, msg, db.Now(), usage, usage, id); err != nil && s.Log != nil {
		s.Log.Warn("record store check", "store", id, "err", err)
	}
}

// Rename names a store anew.
func (s *Set) Rename(ctx context.Context, id int64, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 60 {
		return invalid("a name of 1 to 60 characters is needed")
	}
	r, err := s.DB.ExecContext(ctx, `UPDATE stores SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNoStore
	}
	return nil
}

// SetAutoScan says whether a source's new files are imported by the regular checks.
func (s *Set) SetAutoScan(ctx context.Context, id int64, on bool) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE stores SET auto_scan = ? WHERE id = ?`, on, id)
	return err
}

// SetCategory has the albums a source brings put in a category (#92); 0: in none. Albums it brought
// before stay where they are.
func (s *Set) SetCategory(ctx context.Context, id, category int64) error {
	st, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if st == nil {
		return ErrNoStore
	}
	if err := s.checkCategory(ctx, st.Mode, category); err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `UPDATE stores SET category = nullif(?, 0) WHERE id = ?`, category, id)
	return err
}

func (s *Set) checkCategory(ctx context.Context, mode string, category int64) error {
	if category == 0 {
		return nil
	}
	if mode != Source {
		return invalid("only a source's albums are put in a category")
	}
	var one int
	if err := s.DB.QueryRowContext(ctx, `SELECT 1 FROM categories WHERE id = ?`, category).Scan(&one); errors.Is(err, sql.ErrNoRows) {
		return invalid("there is no category %d", category)
	} else if err != nil {
		return err
	}
	return nil
}

// SetScan records the last scan (a source) or check (a managed store) of a store.
func (s *Set) SetScan(ctx context.Context, id int64, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `UPDATE stores SET scan = ? WHERE id = ?`, string(b), id)
	return err
}

// Remove deletes a store's record, and its default role (imports go to Google Drive again). The
// caller has dealt with the library's files there; the files themselves are left as they are.
func (s *Set) Remove(ctx context.Context, id int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.RemoveTx(ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveTx is Remove in tx, with what else goes with the store (a source's songs, review #222).
func (s *Set) RemoveTx(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key = ? AND value = ?`, settingDefault, strconv.FormatInt(id, 10)); err != nil {
		return err
	}
	r, err := tx.ExecContext(ctx, `DELETE FROM stores WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNoStore
	}
	s.rebuild(id) // made again from the database should the transaction not go through
	return nil
}

var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// r2Bucket takes the bucket out of an R2 endpoint as a bucket's settings show it, ending with its
// name (https://<account ID>.r2.cloudflarestorage.com/<bucket>), for a store being added: one made
// with it kept its files in that bucket, below a folder named as the bucket field said, its name
// shown for a bucket it was not. The bucket field empty or the same, the name is taken from the
// endpoint; another is refused, saying where each goes. A path on other services is theirs (one
// behind a proxy's path); stores made before keep theirs, as their files are where it says.
func r2Bucket(config json.RawMessage) (json.RawMessage, error) {
	var cfg S3Config
	if err := json.Unmarshal(config, &cfg); err != nil {
		return config, invalid("config: %v", err)
	}
	u, err := url.Parse(strings.TrimSpace(cfg.Endpoint))
	if err != nil || !strings.HasSuffix(strings.ToLower(u.Hostname()), ".r2.cloudflarestorage.com") {
		return config, nil
	}
	in := strings.Trim(u.Path, "/")
	if in == "" {
		return config, nil
	}
	u.Path, u.RawPath, u.RawQuery = "", "", ""
	name, rest, _ := strings.Cut(in, "/")
	bucket := strings.TrimSpace(cfg.Bucket)
	if rest != "" || (bucket != "" && bucket != name) {
		msg := fmt.Sprintf("the endpoint ends with a bucket's name, %q: the endpoint is %s, and %q goes in the bucket's name", "/"+in, u, name)
		if rest != "" {
			msg += fmt.Sprintf(" (%q in the folder's)", rest)
		}
		return config, invalid("%s", msg)
	}
	cfg.Endpoint, cfg.Bucket = u.String(), name
	return json.Marshal(cfg)
}

// checkBucket reads a bucket store's settings and keys, refuses a bucket folder that overlaps
// another store's (except store except), and tries them: a managed store's are tried by writing
// its mark, so keys that may only read are found out now.
func (s *Set) checkBucket(ctx context.Context, n NewStore, except int64) (S3Config, S3Secret, error) {
	var cfg S3Config
	var sec S3Secret
	if err := json.Unmarshal(n.Config, &cfg); err != nil {
		return cfg, sec, invalid("config: %v", err)
	}
	if len(n.Secret) > 0 {
		if err := json.Unmarshal(n.Secret, &sec); err != nil {
			return cfg, sec, invalid("secret: %v", err)
		}
	}
	cfg.Endpoint = strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	cfg.Bucket = strings.TrimSpace(cfg.Bucket)
	cfg.Region = strings.TrimSpace(cfg.Region)
	sec.AccessKey, sec.SecretKey = strings.TrimSpace(sec.AccessKey), strings.TrimSpace(sec.SecretKey)
	var err error
	if cfg.Prefix, err = cleanPrefix(cfg.Prefix); err != nil {
		return cfg, sec, invalid("%v", err)
	}
	if !bucketName.MatchString(cfg.Bucket) {
		return cfg, sec, invalid("the bucket's name is 3 to 63 lowercase letters, digits, dots and hyphens")
	}
	if sec.AccessKey == "" || sec.SecretKey == "" {
		return cfg, sec, invalid("the access key ID and the secret access key are both needed")
	}
	b, err := newS3(n.Name, n.Mode, cfg, sec, s.httpClient())
	if err != nil {
		return cfg, sec, invalid("%v", err)
	}
	cfg.Region = b.cfg.Region
	stores, err := s.Stores(ctx)
	if err != nil {
		return cfg, sec, err
	}
	for _, st := range stores {
		if st.Kind != KindS3 || st.ID == except {
			continue
		}
		var other S3Config
		json.Unmarshal(st.Config, &other)
		if !strings.EqualFold(strings.TrimRight(other.Endpoint, "/"), cfg.Endpoint) || other.Bucket != cfg.Bucket {
			continue
		}
		a, o := cfg.Prefix+"/", other.Prefix+"/"
		if cfg.Prefix == "" || other.Prefix == "" || strings.HasPrefix(a, o) || strings.HasPrefix(o, a) {
			return cfg, sec, invalid("%s/%s overlaps 「%s」 (%s/%s)", cfg.Bucket, cfg.Prefix, st.Name, other.Bucket, other.Prefix)
		}
	}
	try, cancel := context.WithTimeout(ctx, tryFor)
	defer cancel()
	if _, err := b.Check(try); err != nil {
		return cfg, sec, invalid("cannot use the bucket: %v", tried(err, cfg.Endpoint))
	}
	return cfg, sec, nil
}

// SetKeys gives a bucket store new keys (an R2 token rotated), tried before they are kept.
func (s *Set) SetKeys(ctx context.Context, id int64, secret json.RawMessage) (*Store, error) {
	st, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, ErrNoStore
	}
	if st.Kind == KindOneDrive {
		return s.setClientSecret(ctx, st, secret)
	}
	if st.Kind != KindS3 {
		return nil, invalid("only a bucket's keys or a OneDrive app's client secret are changed here")
	}
	_, sec, err := s.checkBucket(ctx, NewStore{Kind: st.Kind, Name: st.Name, Mode: st.Mode, Config: st.Config, Secret: secret}, id)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(sec)
	if _, err := s.DB.ExecContext(ctx, `UPDATE stores SET secret = ? WHERE id = ?`, string(raw), id); err != nil {
		return nil, err
	}
	return s.Check(ctx, id)
}
