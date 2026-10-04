package library

import (
	"context"
	"database/sql"
	"errors"

	"github.com/HHim8826/kanade/server/internal/db"
)

// Listening time as it happened (review #93). Each report of a play says how much of it has been
// heard so far; what that adds to the play is spread over the time just before the report (the
// listener heard it then), cut into 15-minute spans, so days add up what was heard during them in
// any time zone. Heard time only grows, so a report sent again or late adds nothing.

const spanMS = 15 * 60_000

// span is a 15-minute span and the heard time in it.
type span struct{ bucket, ms int64 }

// spread cuts heard time ms ending at end into the spans it falls in, the latest last.
func spread(end, ms int64) []span {
	var out []span
	for start := end - ms; start < end; {
		b := start - start%spanMS
		stop := min(b+spanMS, end)
		out = append(out, span{b, stop - start})
		start = stop
	}
	return out
}

// addListening records what a play added at the moment at: ms more heard, and whether it reached the
// play threshold then.
func addListening(ctx context.Context, q querier, playID, at, ms int64, counted bool, estimated bool) error {
	var trackID int64
	var album sql.NullInt64
	var kind string
	err := q.QueryRowContext(ctx, `SELECT p.track_id, p.album_id, coalesce(t.kind, 'music') FROM plays p LEFT JOIN tracks t ON t.id = p.track_id
		WHERE p.id = ?`, playID).Scan(&trackID, &album, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	spans := spread(at, ms)
	if counted && len(spans) == 0 {
		spans = []span{{at - at%spanMS, 0}}
	}
	for i, sp := range spans {
		c := 0
		if counted && i == len(spans)-1 {
			c = 1
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO listening (play_id, bucket, track_id, album_id, kind, ms, counted, estimated)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (play_id, bucket) DO UPDATE SET ms = ms + excluded.ms, counted = max(counted, excluded.counted)`,
			playID, sp.bucket, trackID, album, kind, sp.ms, c, estimated); err != nil {
			return err
		}
	}
	return nil
}

// BackfillListening spreads the plays recorded before listening spans were kept back from their
// last report, marked estimated: their heard time is known, not when within the session it was
// heard. Plays that have spans are left alone, so this runs once in effect.
func (s *Store) BackfillListening(ctx context.Context) error {
	if v, _ := db.GetSetting(ctx, s.db, "listening_backfill"); v == "done" {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, updated_at, listened_ms, counted FROM plays p
		WHERE (listened_ms > 0 OR counted = 1) AND NOT EXISTS (SELECT 1 FROM listening l WHERE l.play_id = p.id)`)
	if err != nil {
		return err
	}
	type old struct {
		id, at, ms int64
		counted    bool
	}
	var plays []old
	for rows.Next() {
		var p old
		if err := rows.Scan(&p.id, &p.at, &p.ms, &p.counted); err != nil {
			rows.Close()
			return err
		}
		plays = append(plays, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range plays {
		if err := addListening(ctx, tx, p.id, p.at, p.ms, p.counted, true); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('listening_backfill', 'done')
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`); err != nil {
		return err
	}
	return tx.Commit()
}
