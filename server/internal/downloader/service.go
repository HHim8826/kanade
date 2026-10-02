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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/importer"
	"github.com/HHim8826/kanade/server/internal/staging"
)

// Download states.
const (
	StateMetadata    = "metadata"    // fetching the torrent or magnet metadata
	StateSelecting   = "selecting"   // file list known; waiting for the user to choose
	StateQueued      = "queued"      // chosen; waits for the one download slot
	StateDownloading = "downloading" //
	StatePaused      = "paused"      //
	StateImporting   = "importing"   // a round is fetched and being imported; the next one waits (review #28)
	StateSeeding     = "seeding"     // data complete, imported, still seeding (D5)
	StateCompleted   = "completed"   // seeding finished
	StateFailed      = "failed"
	StateCanceled    = "canceled"
)

var (
	ErrNotReady   = errors.New("the downloader is not running")
	ErrOverBudget = errors.New("not enough staging space")
	ErrBadState   = errors.New("not possible in the current state")
	ErrLowDisk    = errors.New("the disk is nearly full; new downloads wait until space is freed")
)

type FileView struct {
	Index     int    `json:"index"` // aria2's 1-based file index
	Path      string `json:"path"`  // relative to the download folder
	Length    int64  `json:"length"`
	Selected  bool   `json:"selected"`
	Suggested bool   `json:"suggested"`       // default choice by the D2 table
	Round     int    `json:"round,omitempty"` // the round that fetches it (review #28); 0: not yet
	Batch     int64  `json:"batch,omitempty"` // the import batch its round made
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
	AutoSelect    bool       `json:"auto_select,omitempty"` // started by an RSS rule: takes the suggested files itself
	Round         int        `json:"round,omitempty"`       // the round running or last finished (review #28)
	Rounds        int        `json:"rounds,omitempty"`      // rounds so far, plus one when files are left
	Note          string     `json:"note,omitempty"`        // what it waits for
	Left          int        `json:"left,omitempty"`        // selected files no round has fetched yet
	WaitingSpace  bool       `json:"waiting_space,omitempty"`
	Budget        int64      `json:"budget,omitempty"` // the staging budget rounds fit (with the file list)
	Files         []FileView `json:"files,omitempty"`
}

type Service struct {
	lowDisk atomic.Bool // set by the disk guard
	db      *sql.DB
	aria    *Aria2
	imp     *importer.Importer
	root    string          // downloads directory
	budget  *staging.Budget // staging budget shared with uploads and the importer (plan §6: 2 GB, 4 GB reserve)
	log     *slog.Logger
	mu      sync.Mutex // serializes state changes between API calls and the poll loop
	kick    chan struct{}
}

// NewService makes the service with a budget of its own; ShareBudget puts it on the shared one.
func NewService(d *sql.DB, aria *Aria2, imp *importer.Importer, root string, budget, reserve int64, log *slog.Logger) *Service {
	s := &Service{db: d, aria: aria, imp: imp, root: root, log: log, kick: make(chan struct{}, 1)}
	s.ShareBudget(&staging.Budget{Limit: budget, Reserve: reserve, Dir: root, Free: freeSpace})
	return s
}

// ShareBudget counts the downloads against b, and starts rounds only when they fit it.
func (s *Service) ShareBudget(b *staging.Budget) {
	s.budget = b
	b.Use(s.Committed)
}

func (s *Service) poke() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

var btih = regexp.MustCompile(`(?i)^magnet:\?.*xt=urn:btih:`)

// SetLowDisk is set by the disk guard: while it is on, nothing new starts downloading.
func (s *Service) SetLowDisk(on bool) { s.lowDisk.Store(on) }

// PauseForDisk pauses every transfer that writes to disk (seeding goes on), marking them so that
// ResumeAfterDisk picks them up again. The mark is written before aria2 is told, and taken back if
// aria2 refuses, so the database never claims a pause that did not happen; an error leaves the rest
// for the guard's next check.
func (s *Service) PauseForDisk(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT id, gid, state FROM downloads WHERE state IN (?, ?)`, StateDownloading, StateQueued)
	if err != nil {
		return 0, err
	}
	type dl struct {
		id         int64
		gid, state string
	}
	var list []dl
	for rows.Next() {
		var d dl
		if err := rows.Scan(&d.id, &d.gid, &d.state); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	paused := 0
	for _, d := range list {
		r, err := s.db.ExecContext(ctx, `UPDATE downloads SET state = ?, paused_by = 'disk', error = ?, updated_at = ? WHERE id = ? AND state = ?`,
			StatePaused, "paused: the disk is nearly full; it continues by itself once space is freed", db.Now(), d.id, d.state)
		if err != nil {
			return paused, err
		}
		if n, err := r.RowsAffected(); err != nil || n == 0 {
			continue // changed meanwhile
		}
		if d.state == StateDownloading && d.gid != "" {
			if err := s.aria.RPC.Call(ctx, "forcePause", nil, d.gid); err != nil && !IsNotFound(err) {
				s.db.ExecContext(ctx, `UPDATE downloads SET state = ?, paused_by = '', error = '', updated_at = ? WHERE id = ?`,
					d.state, db.Now(), d.id)
				return paused, err
			}
		}
		paused++
	}
	return paused, nil
}

// DiskPaused counts the downloads the disk guard has paused, so a restarted guard knows space was low.
func (s *Service) DiskPaused(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM downloads WHERE state = ? AND paused_by = 'disk'`, StatePaused).Scan(&n)
	return n, err
}

// ResumeAfterDisk queues again what the disk guard paused.
func (s *Service) ResumeAfterDisk(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.db.ExecContext(ctx, `UPDATE downloads SET state = ?, paused_by = '', error = '', updated_at = ?
		WHERE state = ? AND paused_by = 'disk'`, StateQueued, db.Now(), StatePaused)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	if n > 0 {
		s.poke()
	}
	return int(n), nil
}

// Add starts a download from a magnet link, a .torrent URL or .torrent bytes (uploaded, or fetched
// by the caller; uri then names where they came from). Torrents become paused aria2 tasks right
// away; magnets first fetch their metadata. With auto the suggested files are chosen as soon as the
// file list is known (RSS auto-download); otherwise the task waits for the user.
func (s *Service) Add(ctx context.Context, uri string, torrent []byte, auto bool) (int64, error) {
	if !s.aria.Ready() {
		return 0, ErrNotReady
	}
	if s.lowDisk.Load() {
		return 0, ErrLowDisk
	}
	uri = strings.TrimSpace(uri)
	source := uri
	magnet := false
	switch {
	case len(torrent) > 0:
		if source == "" {
			source = "upload"
		}
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
	r, err := s.db.ExecContext(ctx, `INSERT INTO downloads (source, state, dir, auto_select, created_at, updated_at) VALUES (?, ?, '', ?, ?, ?)`,
		source, StateMetadata, auto, now, now)
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
	} else if err = saveTorrent(dir, torrent); err == nil {
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

// taskTorrent is the torrent kept in a download's folder, from which later rounds add the task
// again (review #28).
const taskTorrent = "task.torrent"

func saveTorrent(dir string, torrent []byte) error {
	return os.WriteFile(filepath.Join(dir, taskTorrent), torrent, 0o600)
}

// addTorrent adds a paused task. With check, aria2 first checks what is already on disk against
// the piece hashes: a later round finds files and fragments left by earlier ones, and only pieces
// that verify count as fetched.
func (s *Service) addTorrent(ctx context.Context, torrent []byte, dir string, check ...bool) (string, error) {
	opts := map[string]string{"dir": dir, "pause": "true"}
	if len(check) > 0 && check[0] {
		opts["check-integrity"] = "true"
	}
	var gid string
	err := s.aria.RPC.Call(ctx, "addTorrent", &gid, base64.StdEncoding.EncodeToString(torrent), []string{}, opts)
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
	metaGID, gid, dir      string
	files                  []FileView
	roundBytes, doneBefore int64
}

const rowCols = `id, source, name, info_hash, meta_gid, gid, state, dir, files, total_bytes, done_bytes, uploaded_bytes,
	down_speed, up_speed, peers, error, coalesce(import_batch_id, 0), files_removed, created_at, coalesce(completed_at, 0), auto_select,
	round, round_bytes, done_before, note`

func scanRow(sc interface{ Scan(...any) error }) (*row, error) {
	var r row
	var files string
	err := sc.Scan(&r.ID, &r.Source, &r.Name, &r.InfoHash, &r.metaGID, &r.gid, &r.State, &r.dir, &files, &r.TotalBytes,
		&r.DoneBytes, &r.UploadedBytes, &r.DownSpeed, &r.UpSpeed, &r.Peers, &r.Error, &r.ImportBatchID, &r.FilesRemoved,
		&r.CreatedAt, &r.CompletedAt, &r.AutoSelect, &r.Round, &r.roundBytes, &r.doneBefore, &r.Note)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(files), &r.files)
	r.Rounds = r.Round
	switch r.State { // rounds left only matter while it is under way (downloads from before rounds have none)
	case StateQueued, StateDownloading, StatePaused, StateImporting:
		if r.Left = len(remaining(r.files)); r.Left > 0 {
			r.Rounds++
		}
	}
	r.WaitingSpace = r.State == StateQueued && strings.HasPrefix(r.Note, notePrefixSpace)
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
	v.Budget = s.budget.Limit
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

// dirSize is the disk space the files under dir take: aria2 leaves sparse fragments of unselected
// files (pieces shared with selected neighbours), which take far less than their length.
func dirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				if st, ok := info.Sys().(*syscall.Stat_t); ok {
					n += min(int64(st.Blocks)*512, info.Size()+4096)
				} else {
					n += info.Size()
				}
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

// Select chooses files (aria2 indexes, 1-based) and queues the download. Nothing is reserved yet:
// the scheduler takes staging space round by round, so a selection larger than the budget is
// accepted and downloads in rounds (review #28).
func (s *Service) Select(ctx context.Context, id int64, indexes []int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.selectLocked(ctx, id, indexes)
}

func (s *Service) selectLocked(ctx context.Context, id int64, indexes []int) error {
	if s.lowDisk.Load() {
		return ErrLowDisk
	}
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
	n := 0
	for i := range r.files {
		f := &r.files[i]
		f.Selected = want[f.Index]
		f.Round, f.Batch = 0, 0
		if f.Selected {
			selected += f.Length
			n++
			delete(want, f.Index)
		}
	}
	if n == 0 || len(want) > 0 {
		return errors.New("choose at least one file, using indexes from the file list")
	}
	note := ""
	if selected > s.budget.Limit {
		note = fmt.Sprintf("%d MB selected, more than the %d MB staging budget: it downloads in rounds, each imported and cleared before the next",
			selected>>20, s.budget.Limit>>20)
	}
	files, _ := json.Marshal(r.files)
	_, err = s.db.ExecContext(ctx, `UPDATE downloads SET files = ?, total_bytes = ?, done_bytes = 0, done_before = 0, round = 0,
		round_bytes = 0, note = ?, state = ?, updated_at = ? WHERE id = ?`, string(files), selected, note, StateQueued, db.Now(), id)
	s.poke()
	return err
}

// Committed is what the downloads hold or have promised: files on disk plus what running rounds
// still have to fetch. Rounds not started promise nothing.
func (s *Service) Committed(ctx context.Context) int64 {
	var pending int64
	s.db.QueryRowContext(ctx, `SELECT coalesce(sum(max(round_bytes - (done_bytes - done_before), 0)), 0) FROM downloads
		WHERE state IN (?, ?, ?)`, StateQueued, StateDownloading, StatePaused).Scan(&pending)
	return dirSize(s.root) + pending
}

// stopOldestSeed frees staging space for a round: it stops the oldest seed whose files are all in
// the library, and removes them (D5). It reports whether it stopped one.
func (s *Service) stopOldestSeed(ctx context.Context) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM downloads WHERE state = ? AND files_removed = 0 ORDER BY completed_at`, StateSeeding)
	if err != nil {
		return false, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		r, err := s.load(ctx, id)
		if err != nil || r == nil || !s.importsSaved(ctx, r) {
			continue
		}
		s.aria.RPC.Call(ctx, "forceRemove", nil, r.gid)
		s.setState(ctx, id, StateCompleted, "seeding stopped early to make room")
		s.cleanup(ctx, r)
		s.log.Info("stopped seeding to free space", "download", id)
		return true, nil
	}
	return false, nil
}

// batches are the import batches a download's rounds made.
func (r *row) batches() []int64 {
	seen := map[int64]bool{}
	var out []int64
	add := func(id int64) {
		if id != 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, f := range r.files {
		add(f.Batch)
	}
	add(r.ImportBatchID)
	return out
}

// importsSaved reports whether every import of a download is done and its files are in the library,
// excluded, or discarded: nothing needs its files any more (review #1).
func (s *Service) importsSaved(ctx context.Context, r *row) bool {
	for _, b := range r.batches() {
		var state string
		var keep int
		if err := s.db.QueryRowContext(ctx, `SELECT b.state,
			(SELECT count(*) FROM import_items i WHERE i.batch_id = b.id AND `+importer.KeepsSource("i")+`)
			FROM import_batches b WHERE b.id = ?`, b).Scan(&state, &keep); err != nil {
			return false
		}
		if state != "done" || keep > 0 {
			return false
		}
	}
	return true
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
	if r.State != StateQueued && r.gid != "" {
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
	if s.lowDisk.Load() && !(r.TotalBytes > 0 && r.DoneBytes >= r.TotalBytes) {
		return ErrLowDisk
	}
	s.db.ExecContext(ctx, `UPDATE downloads SET paused_by = '' WHERE id = ?`, id)
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

// cleanup removes a finished download's files once nothing needs them: no import, or imports in
// which every file is in the library, was excluded, or was discarded by the user. Failed and
// skipped audio keep the files, so the import can be retried (review #1).
func (s *Service) cleanup(ctx context.Context, r *row) {
	if r.FilesRemoved || r.dir == "" {
		return
	}
	if !s.importsSaved(ctx, r) {
		return // files not in the library yet stay, for a retry or until the user discards them
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
