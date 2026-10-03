package importer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/HHim8826/kanade/server/internal/db"
)

type ItemView struct {
	ID        int64  `json:"id"`
	RelPath   string `json:"path"`
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
	SentBytes int64  `json:"sent_bytes,omitempty"`
	Total     int64  `json:"total_bytes,omitempty"`
	AssetID   int64  `json:"asset_id,omitempty"`
	TrackID   int64  `json:"track_id,omitempty"`
	EntryID   int64  `json:"entry_id,omitempty"`
}

type BatchView struct {
	ID         int64          `json:"id"`
	Kind       string         `json:"kind"`
	Source     string         `json:"source"`
	State      string         `json:"state"`
	CreatedAt  int64          `json:"created_at"`
	FinishedAt int64          `json:"finished_at,omitempty"`
	Counts     map[string]int `json:"counts"`
	// Unsaved counts files of a finished batch that are not in the library (failed, or audio that
	// was skipped): their sources are kept until they are retried or discarded.
	Unsaved int        `json:"unsaved"`
	Items   []ItemView `json:"items,omitempty"`
	// Clearable: finished with nothing left to save, so its record can leave the task center.
	Clearable bool `json:"clearable,omitempty"`
}

func (im *Importer) counts(ctx context.Context, batchID int64) (map[string]int, error) {
	rows, err := im.db.QueryContext(ctx, `SELECT state, count(*) FROM import_items WHERE batch_id = ? GROUP BY state`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}

// Batch returns a batch with its items, including live upload progress.
func (im *Importer) Batch(ctx context.Context, id int64) (*BatchView, error) {
	var b BatchView
	var finished sql.NullInt64
	err := im.db.QueryRowContext(ctx, `SELECT id, kind, source, state, created_at, finished_at FROM import_batches WHERE id = ?`, id).
		Scan(&b.ID, &b.Kind, &b.Source, &b.State, &b.CreatedAt, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b.FinishedAt = finished.Int64
	if b.Counts, err = im.counts(ctx, id); err != nil {
		return nil, err
	}
	if b.State == BatchDone {
		if b.Unsaved, err = im.Unsaved(ctx, id); err != nil {
			return nil, err
		}
	}
	rows, err := im.db.QueryContext(ctx, `SELECT id, rel_path, state, error, coalesce(asset_id, 0), coalesce(track_id, 0),
		coalesce(entry_id, 0) FROM import_items WHERE batch_id = ? ORDER BY rel_path`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	b.Items = []ItemView{}
	for rows.Next() {
		var v ItemView
		if err := rows.Scan(&v.ID, &v.RelPath, &v.State, &v.Error, &v.AssetID, &v.TrackID, &v.EntryID); err != nil {
			return nil, err
		}
		im.mu.Lock()
		if p, ok := im.progress[v.ID]; ok {
			v.SentBytes, v.Total = p[0], p[1]
		}
		im.mu.Unlock()
		b.Items = append(b.Items, v)
	}
	return &b, rows.Err()
}

// unfinished is a batch the user still has to do something with (or wait for): under way, in review,
// or finished with files that are not in the library.
const unfinished = `(b.state NOT IN ('done', 'canceled') OR (b.state = 'done' AND EXISTS (SELECT 1 FROM import_items i
	WHERE i.batch_id = b.id AND %s)))`

// Batches lists the batches of the task center with per-state counts: every unfinished one, and the
// latest history finished ones; more says there are older finished ones. Batches whose record was
// cleared are left out.
func (im *Importer) Batches(ctx context.Context, history int) ([]BatchView, bool, error) {
	open := fmt.Sprintf(unfinished, KeepsSource("i"))
	rows, err := im.db.QueryContext(ctx, `SELECT id, kind, source, state, created_at, finished_at FROM (
		SELECT b.id, b.kind, b.source, b.state, b.created_at, coalesce(b.finished_at, 0) finished_at, 0 done
			FROM import_batches b WHERE b.cleared_at IS NULL AND `+open+`
		UNION ALL
		SELECT * FROM (SELECT b.id, b.kind, b.source, b.state, b.created_at, coalesce(b.finished_at, 0), 1
			FROM import_batches b WHERE b.cleared_at IS NULL AND NOT `+open+` ORDER BY b.id DESC LIMIT ?))
		ORDER BY id DESC`, history+1)
	if err != nil {
		return nil, false, err
	}
	var out []BatchView
	for rows.Next() {
		var b BatchView
		if err := rows.Scan(&b.ID, &b.Kind, &b.Source, &b.State, &b.CreatedAt, &b.FinishedAt); err != nil {
			rows.Close()
			return nil, false, err
		}
		out = append(out, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	finished := 0
	for i := range out {
		var err error
		if out[i].Counts, err = im.counts(ctx, out[i].ID); err != nil {
			return nil, false, err
		}
		if out[i].State == BatchDone {
			if out[i].Unsaved, err = im.Unsaved(ctx, out[i].ID); err != nil {
				return nil, false, err
			}
		}
		if out[i].Clearable = (out[i].State == BatchDone || out[i].State == BatchCanceled) && out[i].Unsaved == 0; out[i].Clearable {
			finished++
		}
	}
	more := finished > history
	if more { // the one past the page: the oldest finished batch listed
		for i := len(out) - 1; i >= 0; i-- {
			if out[i].Clearable {
				out = slices.Delete(out, i, i+1)
				break
			}
		}
	}
	if out == nil {
		out = []BatchView{}
	}
	return out, more, nil
}

// ErrNotClearable is a batch that is not finished, or has files not in the library yet.
var ErrNotClearable = errors.New("only a finished import with every file saved, set aside or discarded can be removed")

// Clear removes a finished batch's record from the task center (nothing else changes); with id 0,
// every finished batch's. It reports how many were cleared.
func (im *Importer) Clear(ctx context.Context, id int64) (int, error) {
	open := fmt.Sprintf(unfinished, KeepsSource("i"))
	r, err := im.db.ExecContext(ctx, `UPDATE import_batches AS b SET cleared_at = ? WHERE b.cleared_at IS NULL AND (? = 0 OR b.id = ?)
		AND NOT `+open, db.Now(), id, id)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	if id != 0 && n == 0 {
		return 0, ErrNotClearable
	}
	return int(n), nil
}
