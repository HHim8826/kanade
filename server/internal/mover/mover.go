// Package mover moves the library's files to another storage location, Google Drive or a managed
// store: every file of a location (to empty it before it is removed, or to change where the library
// lives), or those of chosen albums. One file at a time, it is read where it is (a folder's file in
// place, any other copied to staging under the staging budget) and checked against its SHA-256,
// kept at the destination and checked there; then the library reads it from there, and the old copy
// goes to its location's trash a while later, once streams reading it are over. A move that cannot
// go on (the destination offline or full, no room in staging) waits and is tried again; a file that
// is no longer there, or holds other bytes than were imported, is left where it is and listed.
package mover

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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/importer"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/staging"
	"github.com/HHim8826/kanade/server/internal/storage"
)

// A move's states.
const (
	Running  = "running"
	Waiting  = "waiting" // tried again at next_at
	Done     = "done"
	Canceled = "canceled"
)

// Why a move waits or stopped, and why a file was not moved.
const (
	ReasonDestOffline   = "dest_offline"   // the destination cannot be reached
	ReasonDestAuth      = "dest_auth"      // it refuses its keys or sign-in; Drive is not connected
	ReasonDestFull      = "dest_full"      // it has no room for the file
	ReasonDestGone      = "dest_gone"      // it was removed, or made a source: the move stopped
	ReasonOriginOffline = "origin_offline" // where a file is cannot be reached
	ReasonOriginAuth    = "origin_auth"
	ReasonStaging       = "staging" // no room in staging to copy a file through
	ReasonError         = "error"   // anything else: tried again, a file three times
	ReasonGone          = "gone"    // a file no longer where it was
	ReasonChanged       = "changed" // other bytes there than were imported
)

// tries is how many times a file is tried when it fails for no known reason.
const tries = 3

type Mover struct {
	DB     *sql.DB
	Lib    *library.Store
	Files  *storage.Set
	Budget *staging.Budget // nil: not limited
	Temp   string          // where a file is copied through
	Log    *slog.Logger
	// OnMoved is told of a file read from another place now (the stream cache keeps its copy).
	OnMoved func(from, to string)
	// TrashAfter is how long an old copy stays before it goes to the trash: streams reading it end.
	TrashAfter time.Duration
	// RetryEvery is how long a move that cannot go on waits before it is tried again.
	RetryEvery time.Duration

	wake     chan struct{}
	trashing chan struct{} // the old copies' loop: a file was moved
	pacer    library.TrashPacer
	mu       sync.Mutex
	cur      Current
	curMove  int64
	stop     context.CancelFunc // ends the file being moved: its move was canceled
	sessions map[int64]string   // resumable uploads, by item
}

func New(d *sql.DB, lib *library.Store, files *storage.Set, temp string, log *slog.Logger) *Mover {
	return &Mover{DB: d, Lib: lib, Files: files, Temp: temp, Log: log, TrashAfter: 15 * time.Minute, RetryEvery: 5 * time.Minute,
		wake: make(chan struct{}, 1), trashing: make(chan struct{}, 1), sessions: map[int64]string{}}
}

func (m *Mover) kick() { poke(m.wake) }

func poke(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// ---- what is asked ----

// Request is a move to ask for: every file in Origin ("drive" or a managed store's ID), or those of
// Albums, to Dest.
type Request struct {
	Dest   string  `json:"dest"`
	Origin string  `json:"origin,omitempty"`
	Albums []int64 `json:"albums,omitempty"`
}

// Plan is what a move would do.
type Plan struct {
	Dest     string `json:"dest"`
	DestName string `json:"dest_name"`
	Title    string `json:"title"`
	Files    int    `json:"files"` // to move
	Songs    int    `json:"songs"`
	Bytes    int64  `json:"bytes"` // their size, covers left out (not recorded)
	Here     int    `json:"here"`  // at the destination already
	Fixed    int    `json:"fixed"` // in a source, or a store since removed: they stay where they are
	// Missing are the songs of an origin store whose files were not found there: they cannot be
	// moved, and keep it from being removed until they are deleted or found.
	Missing int `json:"missing,omitempty"`
	// Default is an origin where imports go: new ones are still kept there.
	Default bool `json:"default,omitempty"`

	files []library.FileToMove
}

// ErrInvalid is a move that cannot be asked for; the message says why.
type ErrInvalid struct{ Msg string }

func (e *ErrInvalid) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &ErrInvalid{fmt.Sprintf(format, a...)} }

// ErrNotFound is no such move; ErrBusy, one that is still going on (to clear).
var (
	ErrNotFound = errors.New("no such move")
	ErrBusy     = errors.New("the move is still going on")
)

// ErrDestination is a store files are being moved to: it is not removed, nor its folder changed,
// before the move is over or canceled (review #236).
var ErrDestination = errors.New("files are being moved to this storage location: wait for the move to end, or cancel it")

// Heading counts the moves not over whose destination is dest (a store's ID); in the transaction
// that removes it.
func Heading(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, dest string) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM moves WHERE dest = ? AND state IN (?, ?)`, dest, Running, Waiting).Scan(&n)
	return n, err
}

// Plan says what moving r would do.
func (m *Mover) Plan(ctx context.Context, r Request) (*Plan, error) {
	if r.Dest == "" {
		return nil, invalid("say where the files go (dest)")
	}
	dest, err := m.Files.Destination(ctx, r.Dest)
	switch {
	case errors.Is(err, storage.ErrNoStore):
		return nil, invalid("no such storage location: %s", r.Dest)
	case errors.Is(err, storage.ErrReadOnly):
		return nil, invalid("%s is a source: Kanade does not write to it", r.Dest)
	case err != nil:
		return nil, err
	}
	p := &Plan{Dest: dest.ID(), DestName: dest.Name()}
	var files []library.FileToMove
	switch {
	case r.Origin != "" && len(r.Albums) > 0:
		return nil, invalid("a location or albums, not both")
	case r.Origin == p.Dest:
		return nil, invalid("the files are there already")
	case r.Origin == storage.DriveID:
		p.Title = "Google Drive"
		files, err = m.Lib.FilesAt(ctx, "drive", 0)
	case r.Origin != "":
		id, _ := strconv.ParseInt(r.Origin, 10, 64)
		st, err := m.Files.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if st == nil {
			return nil, invalid("no such storage location: %s", r.Origin)
		}
		if st.Mode != storage.Managed {
			return nil, invalid("%s is a source: its files are not Kanade's to move", st.Name)
		}
		p.Title = st.Name
		if files, err = m.Lib.FilesAt(ctx, st.Kind, st.ID); err != nil {
			return nil, err
		}
		use, err := m.Lib.StoreUse(ctx, st.Kind, st.ID)
		if err != nil {
			return nil, err
		}
		p.Missing = use.Missing
	case len(r.Albums) > 0:
		if len(r.Albums) > 5000 {
			return nil, invalid("at most 5000 albums at a time")
		}
		if err := m.DB.QueryRowContext(ctx, `SELECT title FROM albums WHERE id = ?`, r.Albums[0]).Scan(&p.Title); errors.Is(err, sql.ErrNoRows) {
			return nil, invalid("no such album: %d", r.Albums[0])
		} else if err != nil {
			return nil, err
		}
		files, err = m.Lib.AlbumFiles(ctx, r.Albums)
	default:
		return nil, invalid("say what to move: a location (origin) or albums")
	}
	if err != nil {
		return nil, err
	}
	if r.Origin != "" {
		def, err := m.Files.Default(ctx)
		if err != nil {
			return nil, err
		}
		p.Default = def == r.Origin
	}
	places := map[string]*storage.Place{}
	for _, f := range files {
		where := placeKey(f.Ref)
		pl, ok := places[where]
		if !ok {
			pl = m.Files.Place(ctx, f.Ref)
			places[where] = pl
		}
		switch {
		case pl != nil && pl.ID == p.Dest:
			p.Here++
		case pl == nil || pl.Mode != storage.Managed:
			p.Fixed++
		default:
			p.Files++
			p.Bytes += f.Size
			if f.Kind == "song" {
				p.Songs++
			}
			p.files = append(p.files, f)
		}
	}
	return p, nil
}

// placeKey names the place a reference is in: "drive", or the store's kind and ID.
func placeKey(ref string) string {
	if kind, id, _, ok := storage.Parse(ref); ok {
		return kind + ":" + strconv.FormatInt(id, 10)
	}
	return storage.DriveID
}

// Start asks for a move: its files are moved one after another from now on.
func (m *Mover) Start(ctx context.Context, r Request) (*Move, error) {
	p, err := m.Plan(ctx, r)
	if err != nil {
		return nil, err
	}
	if p.Files == 0 {
		return nil, invalid("nothing to move")
	}
	albums := ""
	if len(r.Albums) > 0 {
		b, _ := json.Marshal(r.Albums)
		albums = string(b)
	}
	tx, err := m.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := db.Now()
	res, err := tx.ExecContext(ctx, `INSERT INTO moves (dest, origin, albums, title, state, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		p.Dest, r.Origin, albums, p.Title, Running, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	add, err := tx.PrepareContext(ctx, `INSERT INTO move_items (move_id, ref, size, sha256, mime, kind, name, state, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?)`)
	if err != nil {
		return nil, err
	}
	defer add.Close()
	for _, f := range p.files {
		if _, err := add.ExecContext(ctx, id, f.Ref, f.Size, f.SHA256, f.MIME, f.Kind, f.Name, now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	m.kick()
	return m.Get(ctx, id)
}

// ---- what is going on ----

// Move is a move asked for, and how far it got.
type Move struct {
	ID         int64   `json:"id"`
	Dest       string  `json:"dest"`
	DestName   string  `json:"dest_name"` // "" once removed
	Origin     string  `json:"origin,omitempty"`
	Albums     []int64 `json:"albums,omitempty"`
	Title      string  `json:"title"`
	State      string  `json:"state"`
	Reason     string  `json:"reason,omitempty"`
	Error      string  `json:"error,omitempty"`
	NextAt     int64   `json:"next_at,omitempty"`
	CreatedAt  int64   `json:"created_at"`
	FinishedAt int64   `json:"finished_at,omitempty"`
	Files      int     `json:"files"`
	Moved      int     `json:"moved"`
	Skipped    int     `json:"skipped"` // no longer the library's where they were
	Failed     int     `json:"failed"`
	Bytes      int64   `json:"bytes"`
	MovedBytes int64   `json:"moved_bytes"`
	// Current is the file being moved now.
	Current *Current `json:"current,omitempty"`
	// Failures are the files not moved, and why (Get only, at most 500).
	Failures []Failure `json:"failures,omitempty"`
}

type Current struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Sent  int64  `json:"sent"`
	Total int64  `json:"total"`
}

type Failure struct {
	Ref    string `json:"ref"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
	Error  string `json:"error"`
}

const moveCols = `m.id, m.dest, m.origin, m.albums, m.title, m.state, m.reason, m.error, m.next_at, m.created_at, m.finished_at,
	count(i.id), coalesce(sum(i.state = 'done'), 0), coalesce(sum(i.state = 'skipped'), 0), coalesce(sum(i.state = 'failed'), 0),
	coalesce(sum(i.size), 0), coalesce(sum(CASE WHEN i.state = 'done' THEN i.size END), 0)`

func (m *Mover) moves(ctx context.Context, where string, args ...any) ([]Move, error) {
	rows, err := m.DB.QueryContext(ctx, `SELECT `+moveCols+` FROM moves m LEFT JOIN move_items i ON i.move_id = m.id
		WHERE `+where+` GROUP BY m.id ORDER BY m.id DESC LIMIT 50`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Move{}
	for rows.Next() {
		var v Move
		var albums string
		if err := rows.Scan(&v.ID, &v.Dest, &v.Origin, &albums, &v.Title, &v.State, &v.Reason, &v.Error, &v.NextAt, &v.CreatedAt,
			&v.FinishedAt, &v.Files, &v.Moved, &v.Skipped, &v.Failed, &v.Bytes, &v.MovedBytes); err != nil {
			return nil, err
		}
		if albums != "" {
			json.Unmarshal([]byte(albums), &v.Albums)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	names := map[string]string{}
	m.mu.Lock()
	cur, curMove := m.cur, m.curMove
	m.mu.Unlock()
	for i := range out {
		v := &out[i]
		name, ok := names[v.Dest]
		if !ok {
			name = m.placeName(ctx, v.Dest)
			names[v.Dest] = name
		}
		v.DestName = name
		if v.ID == curMove && cur.Name != "" && v.State == Running {
			c := cur
			v.Current = &c
		}
	}
	return out, nil
}

// placeName is the name of a place ("drive" or a store's ID); "" for a store since removed.
func (m *Mover) placeName(ctx context.Context, id string) string {
	if id == storage.DriveID {
		return "Google Drive"
	}
	n, _ := strconv.ParseInt(id, 10, 64)
	if st, err := m.Files.Get(ctx, n); err == nil && st != nil {
		return st.Name
	}
	return ""
}

// List is the moves asked for, the latest 50, newest first.
func (m *Mover) List(ctx context.Context) ([]Move, error) {
	return m.moves(ctx, `1`)
}

// Get is a move, with the files it did not move.
func (m *Mover) Get(ctx context.Context, id int64) (*Move, error) {
	list, err := m.moves(ctx, `m.id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	v := &list[0]
	rows, err := m.DB.QueryContext(ctx, `SELECT ref, name, kind, reason, error FROM move_items WHERE move_id = ? AND state = 'failed' ORDER BY id LIMIT 500`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f Failure
		if err := rows.Scan(&f.Ref, &f.Name, &f.Kind, &f.Reason, &f.Error); err != nil {
			return nil, err
		}
		v.Failures = append(v.Failures, f)
	}
	return v, rows.Err()
}

// Cancel stops a move: the file being moved stays where it was, those moved stay moved.
func (m *Mover) Cancel(ctx context.Context, id int64) (*Move, error) {
	res, err := m.DB.ExecContext(ctx, `UPDATE moves SET state = ?, finished_at = ? WHERE id = ? AND state IN (?, ?)`,
		Canceled, db.Now(), id, Running, Waiting)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		m.mu.Lock()
		if m.curMove == id && m.stop != nil {
			m.stop()
		}
		m.mu.Unlock()
	}
	return m.Get(ctx, id)
}

// Retry tries the files that failed again, and goes on with those not moved yet: a waiting move
// now, a canceled one again.
func (m *Mover) Retry(ctx context.Context, id int64) (*Move, error) {
	tx, err := m.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE moves SET state = ?, reason = '', error = '', next_at = 0, finished_at = 0 WHERE id = ?`, Running, id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE move_items SET state = 'pending', reason = '', error = '', tries = 0, updated_at = ?
		WHERE move_id = ? AND state = 'failed'`, db.Now(), id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	m.kick()
	return m.Get(ctx, id)
}

// Clear forgets a move that is over (id 0: every one), and says how many; what it moved stays.
func (m *Mover) Clear(ctx context.Context, id int64) (int, error) {
	res, err := m.DB.ExecContext(ctx, `DELETE FROM moves WHERE (id = ?1 OR ?1 = 0) AND state IN (?2, ?3)`, id, Done, Canceled)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n > 0 || id == 0 {
		return int(n), nil
	}
	var state string
	if err := m.DB.QueryRowContext(ctx, `SELECT state FROM moves WHERE id = ?`, id).Scan(&state); errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	} else if err != nil {
		return 0, err
	}
	return 0, ErrBusy
}

// ---- moving ----

// Run moves the files of the moves asked for until ctx ends, and beside them the old copies of the
// files moved to their trash. Copies left in staging by a stop are removed first: a file is copied
// again when its turn comes.
func (m *Mover) Run(ctx context.Context) {
	if err := os.RemoveAll(m.Temp); err != nil {
		m.Log.Warn("removing what moving files left in staging", "err", err)
	}
	go m.trashLoop(ctx)
	for {
		wait := m.work(ctx)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-m.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

// work goes on with the moves that can, and says how long until a waiting one is tried again.
func (m *Mover) work(ctx context.Context) time.Duration {
	for ctx.Err() == nil {
		var id int64
		err := m.DB.QueryRowContext(ctx, `SELECT id FROM moves WHERE state = ? OR (state = ? AND next_at <= ?) ORDER BY state = ?, id LIMIT 1`,
			Running, Waiting, db.Now(), Waiting).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			if ctx.Err() == nil {
				m.Log.Warn("moves", "err", err)
			}
			return m.RetryEvery
		}
		if err := m.run(ctx, id); err != nil && ctx.Err() == nil {
			m.Log.Warn("moving files", "move", id, "err", err)
			return m.RetryEvery
		}
	}
	var next sql.NullInt64
	m.DB.QueryRowContext(ctx, `SELECT min(next_at) FROM moves WHERE state = ?`, Waiting).Scan(&next)
	if !next.Valid {
		return time.Hour
	}
	return max(time.Duration(next.Int64-db.Now())*time.Millisecond, time.Second)
}

// trashLoop moves the old copies of the files moved to their trash as their wait ends, beside the
// moves: a location that does not answer, or is offline, holds up none of them (review #245).
func (m *Mover) trashLoop(ctx context.Context) {
	for {
		t := time.NewTimer(m.trashDue(ctx))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-m.trashing:
			t.Stop()
		case <-t.C:
		}
	}
}

// trashDue moves the old copies of the files moved to their trash once their wait is over: all of
// them soon after a move, rather than a hundred at a time with the Drive check (which takes those
// whose move was cleared). One that cannot go now is tried again later, a little longer each time,
// and its location's others with it (library.TrashPacer). It says how long until the next is due.
func (m *Mover) trashDue(ctx context.Context) time.Duration {
	const most = 1000
	rows, err := m.DB.QueryContext(ctx, `SELECT t.file_id FROM file_trash t WHERE t.done_at = 0 AND t.next_at <= ?
		AND t.file_id IN (SELECT ref FROM move_items WHERE state = 'done') ORDER BY t.next_at LIMIT ?`, db.Now(), most)
	if err != nil {
		return m.RetryEvery
	}
	var refs []string
	for rows.Next() {
		var ref string
		if rows.Scan(&ref) == nil {
			refs = append(refs, ref)
		}
	}
	rows.Close()
	done, held := m.pacer.Trash(ctx, m.Lib, refs, m.Files.Trash, m.Files.Offline)
	if done > 0 {
		m.Log.Info("moved files' old copies moved to the trash", "count", done)
	}
	if len(refs) == most && done > 0 {
		return time.Second // more of them
	}
	var due sql.NullInt64
	m.DB.QueryRowContext(ctx, `SELECT min(t.next_at) FROM file_trash t WHERE t.done_at = 0 AND t.next_at > ?
		AND t.file_id IN (SELECT ref FROM move_items WHERE state = 'done')`, db.Now()).Scan(&due)
	wait := time.Hour
	if due.Valid {
		wait = time.Duration(due.Int64-db.Now()) * time.Millisecond
	}
	if !held.IsZero() {
		wait = min(wait, time.Until(held))
	}
	return max(wait, time.Second)
}

type item struct {
	id             int64
	ref, sha, mime string
	kind, name     string
	size           int64
	tries          int
}

// run moves a move's files until it is done, has to wait, or was canceled.
func (m *Mover) run(ctx context.Context, id int64) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Cancel stops what it finds here: a cancel after the state is read below ends ctx, one before
	// is read there (review #237).
	m.mu.Lock()
	m.curMove, m.stop, m.cur = id, cancel, Current{}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.curMove, m.stop, m.cur = 0, nil, Current{}
		m.mu.Unlock()
	}()
	var dest, state string
	if err := m.DB.QueryRowContext(ctx, `SELECT dest, state FROM moves WHERE id = ?`, id).Scan(&dest, &state); err != nil {
		return err
	}
	switch state {
	case Running:
	case Waiting:
		res, err := m.DB.ExecContext(ctx, `UPDATE moves SET state = ?, reason = '', error = '' WHERE id = ? AND state = ?`, Running, id, Waiting)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
	default: // canceled since it was chosen
		return nil
	}
	for {
		var it item
		err := m.DB.QueryRowContext(ctx, `SELECT id, ref, sha256, mime, kind, name, size, tries FROM move_items
			WHERE move_id = ? AND state = 'pending' ORDER BY id LIMIT 1`, id).Scan(&it.id, &it.ref, &it.sha, &it.mime, &it.kind, &it.name, &it.size, &it.tries)
		if errors.Is(err, sql.ErrNoRows) {
			_, err := m.DB.ExecContext(ctx, `UPDATE moves SET state = ?, reason = '', error = '', finished_at = ? WHERE id = ? AND state = ?`,
				Done, db.Now(), id, Running)
			return err
		}
		if err != nil {
			return err
		}
		m.mu.Lock()
		m.cur = Current{Name: it.name, Kind: it.kind, Total: it.size}
		m.mu.Unlock()
		r := m.moveItem(ctx, dest, it)
		if ctx.Err() != nil && r.state != "done" { // canceled, or shutting down: the file is moved later, if at all
			return nil
		}
		if r.retry && it.tries+1 >= tries {
			r.state = "failed"
		}
		if r.state != "" {
			if r.state != "failed" {
				r.reason, r.err = "", ""
			} else {
				m.Log.Warn("a file was not moved", "move", id, "ref", it.ref, "reason", r.reason, "err", r.err)
			}
			if _, err := m.DB.ExecContext(context.WithoutCancel(ctx), `UPDATE move_items SET state = ?, new_ref = ?, reason = ?, error = ?, updated_at = ? WHERE id = ?`,
				r.state, r.newRef, r.reason, r.err, db.Now(), it.id); err != nil {
				return err
			}
			if r.state == "done" {
				poke(m.trashing) // its old copy is owed now
			}
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		if r.retry {
			if _, err := m.DB.ExecContext(ctx, `UPDATE move_items SET tries = tries + 1, reason = ?, error = ?, updated_at = ? WHERE id = ?`,
				r.reason, r.err, db.Now(), it.id); err != nil {
				return err
			}
		}
		// The move cannot go on now: it waits, or stops (its destination gone).
		state, next, finished := Waiting, db.Now()+m.RetryEvery.Milliseconds(), int64(0)
		if r.stop {
			state, next, finished = Canceled, 0, db.Now()
		}
		m.Log.Info("moving files waits", "move", id, "state", state, "reason", r.reason, "err", r.err)
		_, err = m.DB.ExecContext(ctx, `UPDATE moves SET state = ?, reason = ?, error = ?, next_at = ?, finished_at = ? WHERE id = ? AND state = ?`,
			state, r.reason, r.err, next, finished, id, Running)
		return err
	}
}

// result is what became of a file: its state, or "" when not over (the move waits; with retry, a
// try counted), or stop (the move cannot go on).
type result struct {
	state       string // done | skipped | failed
	newRef      string
	retry, stop bool
	reason, err string
}

func failed(reason string, err error) result {
	return result{state: "failed", reason: reason, err: err.Error()}
}
func wait(reason string, err error) result { return result{reason: reason, err: err.Error()} }
func retry(err error) result               { return result{retry: true, reason: ReasonError, err: err.Error()} }

// moveItem moves one file to dest.
func (m *Mover) moveItem(ctx context.Context, destID string, it item) result {
	used, err := m.Lib.FileUsed(ctx, it.ref)
	if err != nil {
		return retry(err)
	}
	if !used { // let go of, or moved by another move, since it was listed
		return result{state: "skipped"}
	}
	dest, err := m.Files.Destination(ctx, destID)
	if err != nil {
		return destFailed(err)
	}
	name, err := m.Files.FileName(ctx, it.ref)
	if err != nil {
		return originFailed(err)
	}
	if name == "" || name == "." || name == "/" {
		name = it.sha[:min(16, len(it.sha))]
	}
	artist, album, sub, err := m.Lib.FileHome(ctx, it.ref)
	if err != nil {
		return retry(err)
	}
	p, size, release, r := m.fetch(ctx, it)
	if release == nil {
		return r
	}
	defer release()
	var since int64
	put := func() (string, error) {
		since = db.Now()
		return dest.Put(ctx, storage.PutFile{Path: p, Dir: importer.HomeDir(it.kind, artist, album, sub), Name: name, MIME: it.mime,
			Size: size, SHA256: it.sha, Sessions: &sessions{m, it.id}, Progress: m.progress})
	}
	ref, err := put()
	if err != nil {
		return destFailed(err)
	}
	// Kept there: the library reads it from there now, even if the move is canceled at this moment
	// (the copy would be left there, of nothing).
	moved, err := m.Lib.Relocate(context.WithoutCancel(ctx), it.ref, ref, since, m.TrashAfter)
	if errors.Is(err, library.ErrInTrash) { // the copy found there went to the trash meanwhile: kept again (review #243)
		library.PastTrash()
		if ref, err = put(); err != nil {
			return destFailed(err)
		}
		moved, err = m.Lib.Relocate(context.WithoutCancel(ctx), it.ref, ref, since, m.TrashAfter)
	}
	if errors.Is(err, library.ErrStoreGone) { // removed while the file was kept there (review #236)
		return destFailed(storage.ErrNoStore)
	}
	if err != nil {
		return retry(err)
	}
	if !moved {
		return result{state: "skipped"}
	}
	if m.OnMoved != nil {
		m.OnMoved(it.ref, ref)
	}
	return result{state: "done", newRef: ref}
}

// coverMost is the most a cover (whose size is not recorded) is read to.
const coverMost = 64 << 20

// fetch has a file's bytes on this server, checked against what was imported: a folder store's
// file where it is, any other copied to staging. release is nil when it could not be (r says why).
func (m *Mover) fetch(ctx context.Context, it item) (p string, size int64, release func(), r result) {
	check := func(h []byte, n int64) bool {
		return strings.EqualFold(hex.EncodeToString(h), it.sha) && (it.size <= 0 || n == it.size)
	}
	changed := func(n int64) result {
		return failed(ReasonChanged, fmt.Errorf("%d bytes there whose SHA-256 is not the one imported (%d bytes)", n, it.size))
	}
	if lp, ok, err := m.Files.LocalPath(ctx, it.ref); ok || err != nil {
		if err != nil {
			return "", 0, nil, originFailed(err)
		}
		f, err := os.Open(lp)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", 0, nil, failed(ReasonGone, err)
			}
			return "", 0, nil, retry(err)
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil {
			return "", 0, nil, retry(err)
		}
		if !check(h.Sum(nil), n) {
			return "", 0, nil, changed(n)
		}
		return lp, n, func() {}, result{}
	}
	need := it.size
	if need <= 0 {
		need = coverMost
	}
	free := func() {}
	if m.Budget != nil {
		rel, err := m.Budget.HoldRequest(ctx, staging.Request{Need: need, Alone: true})
		if err != nil {
			return "", 0, nil, wait(ReasonStaging, err)
		}
		free = rel
	}
	fail := func(r result) (string, int64, func(), result) {
		free()
		return "", 0, nil, r
	}
	if err := os.MkdirAll(m.Temp, 0o700); err != nil {
		return fail(retry(err))
	}
	resp, err := m.Files.OpenRange(ctx, it.ref, 0, -1)
	if err != nil {
		return fail(originFailed(err))
	}
	defer resp.Body.Close()
	f, err := os.CreateTemp(m.Temp, "move-*")
	if err != nil {
		return fail(retry(err))
	}
	gone := func() { f.Close(); os.Remove(f.Name()) }
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, need+1))
	if err == nil {
		err = f.Close()
	}
	switch {
	case err != nil:
		gone()
		return fail(originFailed(err))
	case !check(h.Sum(nil), n):
		gone()
		return fail(changed(n))
	}
	return f.Name(), n, func() { os.Remove(f.Name()); free() }, result{}
}

// originFailed says what reading a file where it is failing means.
func originFailed(err error) result {
	switch reason, _ := storage.Explain(err); reason {
	case "gone":
		return failed(ReasonGone, err)
	case "offline":
		return wait(ReasonOriginOffline, err)
	case "store_auth", "not_connected", "auth_expired":
		return wait(ReasonOriginAuth, err)
	}
	return retry(err)
}

// destFailed says what keeping a file at the destination failing means.
func destFailed(err error) result {
	var ae *gdrive.APIError
	switch {
	case errors.Is(err, storage.ErrNoStore), errors.Is(err, storage.ErrReadOnly):
		r := wait(ReasonDestGone, err)
		r.stop = true
		return r
	case errors.Is(err, storage.ErrNoRoom), errors.As(err, &ae) && ae.Reason == "storageQuotaExceeded":
		return wait(ReasonDestFull, err)
	}
	switch reason, _ := storage.Explain(err); reason {
	case "offline":
		return wait(ReasonDestOffline, err)
	case "store_auth", "not_connected", "auth_expired":
		return wait(ReasonDestAuth, err)
	}
	return retry(err)
}

func (m *Mover) progress(sent, total int64) {
	m.mu.Lock()
	m.cur.Sent, m.cur.Total = sent, total
	m.mu.Unlock()
}

// sessions keeps a file's resumable upload while the server runs: tried again, it goes on.
type sessions struct {
	m  *Mover
	id int64
}

func (s *sessions) LoadSession(context.Context) (string, error) {
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	return s.m.sessions[s.id], nil
}

func (s *sessions) SaveSession(_ context.Context, v string) error {
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	s.m.sessions[s.id] = v
	return nil
}

func (s *sessions) DeleteSession(context.Context) error {
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	delete(s.m.sessions, s.id)
	return nil
}
