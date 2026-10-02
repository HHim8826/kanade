package downloader

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/importer"
)

// Download states.
const (
	StateMetadata    = "metadata"    // fetching the torrent or magnet metadata
	StateSelecting   = "selecting"   // file list known; waiting for the user to choose
	StateQueued      = "queued"      // chosen; waits for the one download slot
	StateDownloading = "downloading" //
	StatePaused      = "paused"      //
	StateSeeding     = "seeding"     // data complete, imported, still seeding (D5)
	StateCompleted   = "completed"   // seeding finished
	StateFailed      = "failed"
	StateCanceled    = "canceled"
)

var (
	ErrNotReady   = errors.New("the downloader is not running")
	ErrOverBudget = errors.New("not enough staging space")
	ErrBadState   = errors.New("not possible in the current state")
)

type FileView struct {
	Index     int    `json:"index"` // aria2's 1-based file index
	Path      string `json:"path"`  // relative to the download folder
	Length    int64  `json:"length"`
	Selected  bool   `json:"selected"`
	Suggested bool   `json:"suggested"` // default choice by the D2 table
}

type View struct {
	ID            int64      `json:"id"`
	Source        string     `json:"source"`
	Name          string     `json:"name"`
	InfoHash      string     `json:"info_hash,omitempty"`
	State         string     `json:"state"`
	TotalBytes    int64      `json:"total_bytes"`
	DoneBytes     int64      `json:"done_bytes"`
	UploadedBytes int64      `json:"uploaded_bytes"`
	DownSpeed     int64      `json:"down_speed"`
	UpSpeed       int64      `json:"up_speed"`
	Peers         int64      `json:"peers"`
	Error         string     `json:"error,omitempty"`
	ImportBatchID int64      `json:"import_batch_id,omitempty"`
	FilesRemoved  bool       `json:"files_removed"`
	CreatedAt     int64      `json:"created_at"`
	CompletedAt   int64      `json:"completed_at,omitempty"`
	Files         []FileView `json:"files,omitempty"`
}

type Service struct {
	db      *sql.DB
	aria    *Aria2
	imp     *importer.Importer
	root    string // downloads directory
	budget  int64  // staging budget shared by downloads (plan §6: 2 GB)
	reserve int64  // free space to keep on the filesystem (plan §6: 4 GB)
	log     *slog.Logger
	mu      sync.Mutex // serializes state changes between API calls and the poll loop
	kick    chan struct{}
	// Other returns staging space held elsewhere (client uploads); both share one budget (plan §6).
	Other func(context.Context) int64
}

func NewService(d *sql.DB, aria *Aria2, imp *importer.Importer, root string, budget, reserve int64, log *slog.Logger) *Service {
	return &Service{db: d, aria: aria, imp: imp, root: root, budget: budget, reserve: reserve, log: log, kick: make(chan struct{}, 1)}
}

func (s *Service) poke() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

var btih = regexp.MustCompile(`(?i)^magnet:\?.*xt=urn:btih:`)

// Add starts a download from a magnet link, a .torrent URL or an uploaded .torrent.
// Torrents become paused aria2 tasks right away; magnets first fetch their metadata.
func (s *Service) Add(ctx context.Context, uri string, torrent []byte) (int64, error) {
	if !s.aria.Ready() {
		return 0, ErrNotReady
	}
	uri = strings.TrimSpace(uri)
	source := uri
	magnet := false
	switch {
	case len(torrent) > 0:
		source = "upload"
	case btih.MatchString(uri):
		magnet = true
	case strings.HasPrefix(uri, "https://"), strings.HasPrefix(uri, "http://"):
		var err error
		if torrent, err = fetchTorrent(ctx, uri); err != nil {
			return 0, err
		}
	default:
		return 0, errors.New("expected a magnet link or a .torrent URL")
	}
	if !magnet && (len(torrent) > maxTorrent || len(torrent) == 0 || torrent[0] != 'd') {
		return 0, errors.New("that is not a .torrent file")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := db.Now()
	r, err := s.db.ExecContext(ctx, `INSERT INTO downloads (source, state, dir, created_at, updated_at) VALUES (?, ?, '', ?, ?)`,
		source, StateMetadata, now, now)
	if err != nil {
		return 0, err
	}
	id, _ := r.LastInsertId()
	dir := filepath.Join(s.root, strconv.FormatInt(id, 10))
	// aria2 writes the torrent into dir so that its session can restore the task (same GID,
	// same file selection) after a restart. If dir does not exist yet, aria2 silently skips
	// that and the task is lost on restart; create it first.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}
	var gid, metaGID string
	if magnet {
		err = s.aria.RPC.Call(ctx, "addUri", &metaGID, []string{uri},
			map[string]string{"dir": dir, "bt-metadata-only": "true", "bt-save-metadata": "true"})
	} else {
		gid, err = s.addTorrent(ctx, torrent, dir)
	}
	if err != nil {
		s.db.ExecContext(ctx, `UPDATE downloads SET state = ?, error = ?, dir = ?, updated_at = ? WHERE id = ?`, StateFailed, err.Error(), dir, db.Now(), id)
		return id, err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE downloads SET dir = ?, gid = ?, meta_gid = ?, updated_at = ? WHERE id = ?`, dir, gid, metaGID, db.Now(), id)
	s.poke()
	return id, err
}

const maxTorrent = 10 << 20

func (s *Service) addTorrent(ctx context.Context, torrent []byte, dir string) (string, error) {
	var gid string
	err := s.aria.RPC.Call(ctx, "addTorrent", &gid, base64.StdEncoding.EncodeToString(torrent), []string{},
		map[string]string{"dir": dir, "pause": "true"})
	return gid, err
}

// fetchTorrent downloads a .torrent file (for example a Nyaa download link).
func fetchTorrent(ctx context.Context, uri string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching the torrent: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching the torrent: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxTorrent+1))
}

type row struct {
	View
	metaGID, gid, dir string
	files             []FileView
}

const rowCols = `id, source, name, info_hash, meta_gid, gid, state, dir, files, total_bytes, done_bytes, uploaded_bytes,
	down_speed, up_speed, peers, error, coalesce(import_batch_id, 0), files_removed, created_at, coalesce(completed_at, 0)`

func scanRow(sc interface{ Scan(...any) error }) (*row, error) {
	var r row
	var files string
	err := sc.Scan(&r.ID, &r.Source, &r.Name, &r.InfoHash, &r.metaGID, &r.gid, &r.State, &r.dir, &files, &r.TotalBytes,
		&r.DoneBytes, &r.UploadedBytes, &r.DownSpeed, &r.UpSpeed, &r.Peers, &r.Error, &r.ImportBatchID, &r.FilesRemoved,
		&r.CreatedAt, &r.CompletedAt)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(files), &r.files)
	return &r, nil
}

func (s *Service) load(ctx context.Context, id int64) (*row, error) {
	r, err := scanRow(s.db.QueryRowContext(ctx, `SELECT `+rowCols+` FROM downloads WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func (s *Service) Get(ctx context.Context, id int64) (*View, error) {
	r, err := s.load(ctx, id)
	if err != nil || r == nil {
		return nil, err
	}
	v := r.View
	v.Files = r.files
	if v.Files == nil {
		v.Files = []FileView{}
	}
	return &v, nil
}

func (s *Service) List(ctx context.Context, limit int) ([]View, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+rowCols+` FROM downloads ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []View{}
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r.View)
	}
	return out, rows.Err()
}

func (s *Service) setState(ctx context.Context, id int64, state, msg string) {
	s.db.ExecContext(ctx, `UPDATE downloads SET state = ?, error = ?, down_speed = 0, up_speed = 0, updated_at = ? WHERE id = ?`,
		state, msg, db.Now(), id)
}

// ---- selection and budget ----

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

// FreeSpace is the space left on dir's filesystem, or -1 when it cannot be read.
func FreeSpace(dir string) int64 { return freeSpace(dir) }

func freeSpace(dir string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * st.Bsize
}

// Select chooses files (aria2 indexes, 1-based) and queues the download. It enforces the
// staging budget, first stopping finished seeds whose import succeeded (D5), and keeps the
// filesystem reserve free.
func (s *Service) Select(ctx context.Context, id int64, indexes []int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.load(ctx, id)
	if err != nil {
		return err
	}
	if r == nil {
		return errors.New("no such download")
	}
	if r.State != StateSelecting {
		return ErrBadState
	}
	want := map[int]bool{}
	for _, i := range indexes {
		want[i] = true
	}
	var selected int64
	var list []string
	for i := range r.files {
		f := &r.files[i]
		f.Selected = want[f.Index]
		if f.Selected {
			selected += f.Length
			list = append(list, strconv.Itoa(f.Index))
			delete(want, f.Index)
		}
	}
	if len(list) == 0 || len(want) > 0 {
		return errors.New("choose at least one file, using indexes from the file list")
	}
	if err := s.makeRoom(ctx, selected); err != nil {
		return err
	}
	if free := freeSpace(s.root); free >= 0 && free-selected < s.reserve {
		return fmt.Errorf("%w: the disk would drop below the %d GB free-space reserve", ErrOverBudget, s.reserve>>30)
	}
	if err := s.aria.RPC.Call(ctx, "changeOption", nil, r.gid, map[string]string{"select-file": strings.Join(list, ",")}); err != nil {
		return err
	}
	files, _ := json.Marshal(r.files)
	_, err = s.db.ExecContext(ctx, `UPDATE downloads SET files = ?, total_bytes = ?, state = ?, updated_at = ? WHERE id = ?`,
		string(files), selected, StateQueued, db.Now(), id)
	s.poke()
	return err
}

// Committed is what the downloads hold or have promised: files on disk plus what queued
// and running downloads still have to fetch.
func (s *Service) Committed(ctx context.Context) int64 {
	var pending int64
	s.db.QueryRowContext(ctx, `SELECT coalesce(sum(total_bytes - done_bytes), 0) FROM downloads WHERE state IN (?, ?, ?)`,
		StateQueued, StateDownloading, StatePaused).Scan(&pending)
	return dirSize(s.root) + pending
}

func (s *Service) committed(ctx context.Context) int64 {
	n := s.Committed(ctx)
	if s.Other != nil {
		n += s.Other(ctx)
	}
	return n
}

func (s *Service) makeRoom(ctx context.Context, need int64) error {
	if need > s.budget {
		return fmt.Errorf("%w: %d MB selected, the staging budget is %d MB; choose fewer files", ErrOverBudget, need>>20, s.budget>>20)
	}
	for s.committed(ctx)+need > s.budget {
		// Oldest seed whose files are fully imported: stop it and free its space.
		var id int64
		err := s.db.QueryRowContext(ctx, `SELECT d.id FROM downloads d JOIN import_batches b ON b.id = d.import_batch_id
			WHERE d.state = ? AND d.files_removed = 0 AND b.state = 'done'
			AND NOT EXISTS (SELECT 1 FROM import_items i WHERE i.batch_id = b.id AND i.state = 'failed')
			ORDER BY d.completed_at LIMIT 1`, StateSeeding).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %d MB in use or reserved, %d MB selected, budget %d MB", ErrOverBudget,
				s.committed(ctx)>>20, need>>20, s.budget>>20)
		}
		if err != nil {
			return err
		}
		r, _ := s.load(ctx, id)
		s.aria.RPC.Call(ctx, "forceRemove", nil, r.gid)
		s.setState(ctx, id, StateCompleted, "seeding stopped early to make room")
		s.cleanup(ctx, r)
		s.log.Info("stopped seeding to free space", "download", id)
	}
	return nil
}

// ---- control ----

func (s *Service) Pause(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.load(ctx, id)
	if err != nil || r == nil {
		return errors.New("no such download")
	}
	if r.State != StateDownloading && r.State != StateQueued && r.State != StateSeeding {
		return ErrBadState
	}
	if r.State != StateQueued {
		if err := s.aria.RPC.Call(ctx, "forcePause", nil, r.gid); err != nil {
			return err
		}
	}
	s.setState(ctx, id, StatePaused, "")
	return nil
}

func (s *Service) Resume(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.load(ctx, id)
	if err != nil || r == nil {
		return errors.New("no such download")
	}
	if r.State != StatePaused {
		return ErrBadState
	}
	if r.TotalBytes > 0 && r.DoneBytes >= r.TotalBytes { // finished downloading: back to seeding
		if err := s.aria.RPC.Call(ctx, "unpause", nil, r.gid); err != nil {
			return err
		}
		s.setState(ctx, id, StateSeeding, "")
		return nil
	}
	s.setState(ctx, id, StateQueued, "") // the poll loop starts it when the slot is free
	s.poke()
	return nil
}

func (s *Service) Cancel(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.load(ctx, id)
	if err != nil || r == nil {
		return errors.New("no such download")
	}
	switch r.State {
	case StateCompleted, StateFailed, StateCanceled:
		return ErrBadState
	}
	for _, g := range []string{r.metaGID, r.gid} {
		if g != "" {
			s.aria.RPC.Call(ctx, "forceRemove", nil, g)
		}
	}
	s.setState(ctx, id, StateCanceled, "")
	s.cleanup(ctx, r)
	return nil
}

// cleanup removes a finished download's files once nothing needs them: no import, or an import
// that finished without failures (failed items keep their files so the import can be retried).
func (s *Service) cleanup(ctx context.Context, r *row) {
	if r.FilesRemoved || r.dir == "" {
		return
	}
	if r.ImportBatchID != 0 {
		var state string
		var failed, open int
		s.db.QueryRowContext(ctx, `SELECT b.state,
			(SELECT count(*) FROM import_items WHERE batch_id = b.id AND state = 'failed'),
			(SELECT count(*) FROM import_items WHERE batch_id = b.id AND state IN ('pending', 'uploading'))
			FROM import_batches b WHERE b.id = ?`, r.ImportBatchID).Scan(&state, &failed, &open)
		if state != "done" || failed > 0 || open > 0 {
			return
		}
	}
	for _, g := range []string{r.metaGID, r.gid} {
		if g != "" {
			s.aria.RPC.Call(ctx, "removeDownloadResult", nil, g)
		}
	}
	if err := os.RemoveAll(r.dir); err != nil {
		s.log.Warn("remove download files", "download", r.ID, "err", err)
		return
	}
	s.db.ExecContext(ctx, `UPDATE downloads SET files_removed = 1, updated_at = ? WHERE id = ?`, db.Now(), r.ID)
}
