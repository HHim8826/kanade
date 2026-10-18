// Package uploads receives files from clients in chunks (decision D4: 32 MB, resumable)
// and stages them, keeping their relative paths, until they are imported.
package uploads

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/staging"
)

// ChunkSize is the largest chunk accepted; it stays under Cloudflare's 100 MB request limit.
const ChunkSize = 32 << 20

const (
	StateReceiving = "receiving"
	StateComplete  = "complete"
	StateImported  = "imported"
)

var (
	ErrOverBudget = errors.New("not enough staging space")
	ErrLowDisk    = errors.New("the disk is nearly full; new uploads wait until space is freed")
	ErrOffset     = errors.New("chunk does not continue where the upload stopped")
	ErrNotFound   = errors.New("no such upload")
	groupRe       = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
)

type Upload struct {
	ID       int64  `json:"id"`
	Group    string `json:"group"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Received int64  `json:"received"`
	State    string `json:"state"`
	sha256   string
}

type Store struct {
	lowDisk atomic.Bool // set by the disk guard
	db      *sql.DB
	root    string          // staging/uploads
	budget  *staging.Budget // shared with downloads and the importer (plan §6)

	mu    sync.Mutex
	locks map[int64]*sync.Mutex

	extraMu sync.Mutex // the folder's bytes no upload accounts for, as last measured (Usage)
	extra   int64
	extraAt time.Time
}

// New makes a store with a budget of its own; ShareBudget puts it on the shared one.
func New(d *sql.DB, root string, budget int64) *Store {
	s := &Store{db: d, root: root, locks: map[int64]*sync.Mutex{}}
	s.ShareBudget(&staging.Budget{Limit: budget})
	return s
}

// ShareBudget counts this store's uploads against b, and checks new ones against it.
func (s *Store) ShareBudget(b *staging.Budget) {
	s.budget = b
	b.Use(s.Usage)
}

func (s *Store) GroupDir(group string) string { return filepath.Join(s.root, group) }

// CleanPath accepts a relative path made of ordinary names: no absolute paths, no "..",
// no empty or dot components, so a client cannot write outside its group folder.
func CleanPath(p string) (string, error) {
	p = strings.ReplaceAll(p, `\`, "/")
	if p == "" || strings.HasPrefix(p, "/") || len(p) > 1024 {
		return "", errors.New("path must be relative")
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." || len(part) > 255 || strings.ContainsRune(part, 0) {
			return "", fmt.Errorf("invalid path component %q", part)
		}
	}
	return path.Clean(p), nil
}

func (s *Store) lock(id int64) func() {
	s.mu.Lock()
	l, ok := s.locks[id]
	if !ok {
		l = &sync.Mutex{}
		s.locks[id] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func scan(row interface{ Scan(...any) error }) (*Upload, error) {
	var u Upload
	err := row.Scan(&u.ID, &u.Group, &u.Path, &u.Size, &u.Received, &u.State, &u.sha256)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &u, err
}

const cols = `id, grp, path, size, received, state, sha256`

func (s *Store) Get(ctx context.Context, id int64) (*Upload, error) {
	return scan(s.db.QueryRowContext(ctx, `SELECT `+cols+` FROM uploads WHERE id = ?`, id))
}

func dirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// Committed is staged upload data plus what receiving uploads still have to send.
func (s *Store) Committed(ctx context.Context) int64 {
	u := s.Usage(ctx)
	return u.Disk + u.Pending
}

// Usage is the staged upload data, and what receiving uploads still have to send. It is counted
// from the uploads' records, what a receiving one has and the whole of the others (review #258:
// walking the folder for each new upload made a selection of N files cost N walks of it). The
// folder is walked again at most once a minute, for what no record accounts for: a chunk cut off
// by a stop, a group whose removal stopped halfway.
func (s *Store) Usage(ctx context.Context) staging.Usage {
	var have, pending int64
	s.db.QueryRowContext(ctx, `SELECT coalesce(sum(CASE WHEN state = ?1 THEN min(received, size) ELSE size END), 0),
		coalesce(sum(CASE WHEN state = ?1 THEN max(size - received, 0) ELSE 0 END), 0) FROM uploads`, StateReceiving).Scan(&have, &pending)
	return staging.Usage{Disk: have + s.unrecorded(have), Pending: pending}
}

// extraEvery is how old the count of what no upload accounts for may get.
const extraEvery = time.Minute

var measure = dirSize // the folder's size; counted by tests

// unrecorded is what the folder holds beyond have, the uploads' bytes, as last measured. Bytes
// written while it is measured make it more for a while, never less; removed meanwhile, they could
// make it less: the last count stands then.
func (s *Store) unrecorded(have int64) int64 {
	s.extraMu.Lock()
	defer s.extraMu.Unlock()
	if s.extraAt.IsZero() || time.Since(s.extraAt) >= extraEvery {
		if n := measure(s.root) - have; n >= 0 || s.extraAt.IsZero() {
			s.extra = max(n, 0)
		}
		s.extraAt = time.Now()
	}
	return s.extra
}

// partsDir holds the bytes of the uploads being received, each in a file named by its ID: apart
// from the files' own paths, which a client chooses (review #251: "a.mp3.part" was where "a.mp3"
// was received). It is no group's folder: a group's name has no dot.
func (s *Store) partsDir() string { return filepath.Join(s.root, ".parts") }

func (s *Store) partPath(u *Upload) string {
	return filepath.Join(s.partsDir(), strconv.FormatInt(u.ID, 10))
}

// finalPath is where a received upload is, in its group's folder by its path.
func (s *Store) finalPath(u *Upload) string {
	return filepath.Join(s.GroupDir(u.Group), filepath.FromSlash(u.Path))
}

// SetLowDisk is set by the disk guard: while it is on, no new upload starts.
func (s *Store) SetLowDisk(on bool) { s.lowDisk.Store(on) }

// Create starts an upload, or returns the existing one with the same group and path so that a
// client can resume after losing its state.
func (s *Store) Create(ctx context.Context, group, p string, size int64, sha string) (*Upload, error) {
	if !groupRe.MatchString(group) {
		return nil, errors.New("group must be 8-64 letters, digits, - or _")
	}
	clean, err := CleanPath(p)
	if err != nil {
		return nil, err
	}
	if size <= 0 {
		return nil, errors.New("size must be positive")
	}
	if sha != "" {
		if b, err := hex.DecodeString(sha); err != nil || len(b) != sha256.Size {
			return nil, errors.New("sha256 must be 64 hex characters")
		}
	}
	if u, err := scan(s.db.QueryRowContext(ctx, `SELECT `+cols+` FROM uploads WHERE grp = ? AND path = ?`, group, clean)); err == nil {
		if u.Size != size {
			return nil, fmt.Errorf("%s was already started with a different size", clean)
		}
		return u, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if s.lowDisk.Load() { // started uploads may finish; new ones wait
		return nil, ErrLowDisk
	}
	// The check and the reservation (the row, counted by Committed) are one step on the shared
	// budget, which also keeps the disk's free-space reserve (review #4).
	var id int64
	err = s.budget.Take(ctx, staging.Request{Need: size, Record: func() error {
		now := db.Now()
		r, err := s.db.ExecContext(ctx, `INSERT INTO uploads (grp, path, size, sha256, state, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, group, clean, size, strings.ToLower(sha), StateReceiving, now, now)
		if err != nil {
			return err
		}
		id, err = r.LastInsertId()
		return err
	}})
	if errors.Is(err, staging.ErrOverBudget) || errors.Is(err, staging.ErrReserve) {
		return nil, fmt.Errorf("%w: %w", ErrOverBudget, err)
	}
	if err != nil {
		return nil, err
	}
	u := &Upload{ID: id, Group: group, Path: clean, Size: size, State: StateReceiving, sha256: strings.ToLower(sha)}
	if err := os.MkdirAll(s.partsDir(), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.partPath(u), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	return u, nil
}

// Append writes one chunk. offset must equal what the server already has; on mismatch the
// caller gets ErrOffset and the current count, and resumes from there.
func (s *Store) Append(ctx context.Context, id, offset int64, body io.Reader, n int64) (*Upload, error) {
	defer s.lock(id)()
	u, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if u.State != StateReceiving {
		return u, errors.New("upload is already complete")
	}
	if offset != u.Received {
		return u, ErrOffset
	}
	if n <= 0 || n > ChunkSize || offset+n > u.Size {
		return u, fmt.Errorf("chunk must be 1 byte to %d MB and end within the file", ChunkSize>>20)
	}
	// What the server had is gone (left behind by a stop, review #251): the upload starts over,
	// rather than the gap being filled with zeros.
	if st, err := os.Stat(s.partPath(u)); offset > 0 && (err != nil || st.Size() < offset) {
		s.reset(ctx, u, "")
		return u, ErrOffset
	}
	f, err := os.OpenFile(s.partPath(u), os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return u, err
	}
	defer f.Close()
	// Drop anything past the confirmed length (a chunk interrupted midway), then append.
	if err := f.Truncate(offset); err != nil {
		return u, err
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return u, err
	}
	got, err := io.Copy(f, io.LimitReader(body, n))
	if err != nil || got != n {
		f.Truncate(offset)
		if err == nil {
			err = fmt.Errorf("chunk ended after %d of %d bytes", got, n)
		}
		return u, err
	}
	u.Received = offset + n
	_, err = s.db.ExecContext(ctx, `UPDATE uploads SET received = ?, updated_at = ? WHERE id = ?`, u.Received, db.Now(), id)
	return u, err
}

// Complete checks the length (and SHA-256 when the client gave one) and moves the file into place.
// It can be sent again: if the server stopped between moving the file and recording it (review
// #5), the file already in place is checked the same way and the upload completes.
func (s *Store) Complete(ctx context.Context, id int64) (*Upload, error) {
	defer s.lock(id)()
	u, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if u.State != StateReceiving {
		return u, nil
	}
	if u.Received != u.Size {
		return u, fmt.Errorf("received %d of %d bytes", u.Received, u.Size)
	}
	part, final := s.partPath(u), s.finalPath(u)
	src := part
	if _, err := os.Stat(part); errors.Is(err, fs.ErrNotExist) {
		if st, err := os.Stat(final); err == nil && st.Mode().IsRegular() && st.Size() == u.Size {
			src = final // moved into place before a restart
		} else {
			return u, s.reset(ctx, u, "the uploaded data is gone")
		}
	}
	if st, err := os.Stat(src); err != nil || st.Size() != u.Size {
		return u, s.reset(ctx, u, "the uploaded file has the wrong length")
	}
	if u.sha256 != "" {
		f, err := os.Open(src)
		if err != nil {
			return u, err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return u, err
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != u.sha256 {
			return u, s.reset(ctx, u, fmt.Sprintf("sha256 mismatch (got %s)", got)) // the bytes are wrong somewhere
		}
	}
	if src == part {
		if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
			return u, err
		}
		if err := os.Rename(part, final); err != nil {
			return u, err
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE uploads SET state = ?, updated_at = ? WHERE id = ?`, StateComplete, db.Now(), id); err != nil {
		return u, err // the file is in place: sending Complete again finishes
	}
	u.State = StateComplete
	return u, nil
}

// reset starts an upload over from the first byte.
func (s *Store) reset(ctx context.Context, u *Upload, why string) error {
	os.Remove(s.finalPath(u))
	os.MkdirAll(s.partsDir(), 0o700)
	if f, err := os.OpenFile(s.partPath(u), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err == nil {
		f.Close()
	}
	s.db.ExecContext(ctx, `UPDATE uploads SET received = 0, updated_at = ? WHERE id = ?`, db.Now(), u.ID)
	u.Received = 0
	return fmt.Errorf("%s; the upload was reset", why)
}

// Recover finishes, at start-up, uploads whose file was moved into place just before the server
// stopped, so their group can be imported without the client sending Complete again. Before, it
// takes up what an older Kanade was receiving beside the files (review #251), and removes the
// received bytes no upload has any more.
func (s *Store) Recover(ctx context.Context) (int, error) {
	receiving, err := s.receiving(ctx)
	if err != nil {
		return 0, err
	}
	mine := map[string]bool{}
	for _, u := range receiving {
		mine[strconv.FormatInt(u.ID, 10)] = true
		if err := s.adoptPart(ctx, u); err != nil {
			return 0, err
		}
	}
	if list, err := os.ReadDir(s.partsDir()); err == nil {
		for _, e := range list {
			if !mine[e.Name()] {
				os.Remove(filepath.Join(s.partsDir(), e.Name()))
			}
		}
	}
	n := 0
	for _, u := range receiving {
		if u.Received != u.Size {
			continue
		}
		if _, err := os.Stat(s.partPath(u)); !errors.Is(err, fs.ErrNotExist) {
			continue // still waiting for the client's Complete
		}
		id := u.ID
		if u, err := s.Complete(ctx, id); err == nil && u.State == StateComplete {
			n++
		}
	}
	return n, nil
}

func (s *Store) receiving(ctx context.Context) ([]*Upload, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+cols+` FROM uploads WHERE state = ?`, StateReceiving)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Upload
	for rows.Next() {
		u, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// adoptPart moves what an older Kanade received for u, at its path with ".part" added, to where u
// is received now. Unless that is a file another upload of the group put there: then it is not u's
// to take (it was overwritten), and u starts over.
func (s *Store) adoptPart(ctx context.Context, u *Upload) error {
	old := s.finalPath(u) + ".part"
	st, err := os.Lstat(old)
	if err != nil || !st.Mode().IsRegular() {
		return nil
	}
	if _, err := os.Stat(s.partPath(u)); err == nil {
		return nil
	}
	var theirs int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM uploads WHERE grp = ? AND path = ? AND state != ?`,
		u.Group, u.Path+".part", StateReceiving).Scan(&theirs); err != nil || theirs > 0 {
		return err
	}
	if err := os.MkdirAll(s.partsDir(), 0o700); err != nil {
		return err
	}
	return os.Rename(old, s.partPath(u))
}

// ReadyForImport reports whether every upload of a group is complete.
func (s *Store) ReadyForImport(ctx context.Context, group string) (complete int, err error) {
	var receiving int
	err = s.db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE state = ?), count(*) FILTER (WHERE state = ?)
		FROM uploads WHERE grp = ?`, StateComplete, StateReceiving, group).Scan(&complete, &receiving)
	if err != nil {
		return 0, err
	}
	if receiving > 0 {
		return complete, fmt.Errorf("%d file(s) in this group are still uploading", receiving)
	}
	if complete == 0 {
		return 0, errors.New("nothing uploaded in this group")
	}
	return complete, nil
}

func (s *Store) MarkImported(ctx context.Context, group string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE uploads SET state = ?, updated_at = ? WHERE grp = ? AND state = ?`,
		StateImported, db.Now(), group, StateComplete)
	return err
}

// RemoveGroup deletes a group's staging folder (including images that were sent only for
// cover selection) and its upload records, after its import finished without failures.
func (s *Store) RemoveGroup(ctx context.Context, group string) error {
	if !groupRe.MatchString(group) {
		return errors.New("bad group")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM uploads WHERE grp = ?`, group)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			os.Remove(s.partPath(&Upload{ID: id}))
		}
	}
	rows.Close()
	if err := os.RemoveAll(s.GroupDir(group)); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM uploads WHERE grp = ?`, group)
	return err
}

// ---- unfinished uploads (review #6) ----

// ErrBusy: an import of the group is under way.
var ErrBusy = errors.New("this upload is being imported; cancel the import first")

// Group is a client selection whose files are not imported yet.
type Group struct {
	Group     string `json:"group"`
	Files     int    `json:"files"`
	Size      int64  `json:"size"`
	Received  int64  `json:"received"`
	Complete  int    `json:"complete"`
	UpdatedAt int64  `json:"updated_at"`
	Importing bool   `json:"importing"` // an import batch of it is analyzing, in review or running
}

// importing is the SQL condition for a group (column grp) with an import still at work on it.
const importing = `EXISTS (SELECT 1 FROM import_batches b WHERE b.kind = 'upload' AND b.source = grp
	AND b.state NOT IN ('done', 'canceled'))`

// Groups lists selections still being sent or waiting to be imported, newest first.
func (s *Store) Groups(ctx context.Context) ([]Group, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT grp, count(*), sum(size), sum(received), count(*) FILTER (WHERE state = ?),
		max(updated_at), `+importing+` FROM uploads WHERE state IN (?, ?) GROUP BY grp ORDER BY max(updated_at) DESC`,
		StateComplete, StateReceiving, StateComplete)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Group{}
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.Group, &g.Files, &g.Size, &g.Received, &g.Complete, &g.UpdatedAt, &g.Importing); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// CancelGroup drops a selection that is not imported: its staged files and records go, and the
// space it held or reserved is free again. A group an import is working on is left alone.
func (s *Store) CancelGroup(ctx context.Context, group string) error {
	var busy, n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*), coalesce(max(`+importing+`), 0) FROM uploads WHERE grp = ? AND state IN (?, ?)`,
		group, StateReceiving, StateComplete).Scan(&n, &busy); err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	if busy != 0 {
		return ErrBusy
	}
	return s.RemoveGroup(ctx, group)
}

// Expire drops selections nobody has sent to or imported for maxAge, so interrupted uploads do not
// hold staging space for ever. Groups with an import at work, or already imported, are kept.
func (s *Store) Expire(ctx context.Context, maxAge time.Duration) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT grp FROM uploads GROUP BY grp
		HAVING max(updated_at) < ? AND count(*) FILTER (WHERE state = ?) = 0 AND NOT max(`+importing+`)`,
		db.Now()-maxAge.Milliseconds(), StateImported)
	if err != nil {
		return 0, err
	}
	var groups []string
	for rows.Next() {
		var g string
		if rows.Scan(&g) == nil {
			groups = append(groups, g)
		}
	}
	rows.Close()
	for _, g := range groups {
		if err := s.RemoveGroup(ctx, g); err != nil {
			return 0, err
		}
	}
	return len(groups), nil
}
