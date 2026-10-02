package importer

import (
	"context"
	"database/sql"
	"errors"
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
	Items      []ItemView     `json:"items,omitempty"`
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

// Batches lists recent batches with per-state counts (the task center's import view).
func (im *Importer) Batches(ctx context.Context, limit int) ([]BatchView, error) {
	rows, err := im.db.QueryContext(ctx, `SELECT id, kind, source, state, created_at, coalesce(finished_at, 0)
		FROM import_batches ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	var out []BatchView
	for rows.Next() {
		var b BatchView
		if err := rows.Scan(&b.ID, &b.Kind, &b.Source, &b.State, &b.CreatedAt, &b.FinishedAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, b)
	}
	rows.Close()
	for i := range out {
		var err error
		if out[i].Counts, err = im.counts(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	if out == nil {
		out = []BatchView{}
	}
	return out, nil
}
