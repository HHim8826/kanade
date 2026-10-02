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
	"strings"
	"sync"
	"sync/atomic"

	"github.com/HHim8826/kanade/server/internal/db"
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
	root    string // staging/uploads
	budget  int64
	// Other returns staging space held elsewhere (downloads), so both share one budget (plan §6).
	Other func(context.Context) int64

	mu    sync.Mutex
	locks map[int64]*sync.Mutex
}

func New(d *sql.DB, root string, budget int64) *Store {
	return &Store{db: d, root: root, budget: budget, locks: map[int64]*sync.Mutex{}}
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
	var pending int64
	s.db.QueryRowContext(ctx, `SELECT coalesce(sum(size - received), 0) FROM uploads WHERE state = ?`, StateReceiving).Scan(&pending)
	return dirSize(s.root) + pending
}

func (s *Store) partPath(u *Upload) string {
	return filepath.Join(s.GroupDir(u.Group), filepath.FromSlash(u.Path)) + ".part"
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
	used := s.Committed(ctx)
	if s.Other != nil {
		used += s.Other(ctx)
	}
	if used+size > s.budget {
		return nil, fmt.Errorf("%w: %d MB in use or reserved, %d MB more needed, budget %d MB", ErrOverBudget, used>>20, size>>20, s.budget>>20)
	}
	now := db.Now()
	r, err := s.db.ExecContext(ctx, `INSERT INTO uploads (grp, path, size, sha256, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, group, clean, size, strings.ToLower(sha), StateReceiving, now, now)
	if err != nil {
		return nil, err
	}
	id, _ := r.LastInsertId()
	u := &Upload{ID: id, Group: group, Path: clean, Size: size, State: StateReceiving, sha256: strings.ToLower(sha)}
	if err := os.MkdirAll(filepath.Dir(s.partPath(u)), 0o700); err != nil {
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
	f, err := os.OpenFile(s.partPath(u), os.O_WRONLY, 0o600)
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
	part := s.partPath(u)
	if u.sha256 != "" {
		f, err := os.Open(part)
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
			// Start over: the bytes are wrong somewhere.
			os.Truncate(part, 0)
			s.db.ExecContext(ctx, `UPDATE uploads SET received = 0, updated_at = ? WHERE id = ?`, db.Now(), id)
			return u, fmt.Errorf("sha256 mismatch (got %s); the upload was reset", got)
		}
	}
	if err := os.Rename(part, strings.TrimSuffix(part, ".part")); err != nil {
		return u, err
	}
	u.State = StateComplete
	_, err = s.db.ExecContext(ctx, `UPDATE uploads SET state = ?, updated_at = ? WHERE id = ?`, StateComplete, db.Now(), id)
	return u, err
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
	if err := os.RemoveAll(s.GroupDir(group)); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM uploads WHERE grp = ?`, group)
	return err
}
