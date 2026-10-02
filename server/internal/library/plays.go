package library

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
)

// Play counting (decision D9): a play counts once the listener has heard half the track or
// four minutes, whichever is shorter; tracks under 30 seconds never count.
const (
	minCountableMS = 30_000
	maxThresholdMS = 4 * 60_000
	resumeMarginMS = 30_000 // "continue" needs 30 s heard and 30 s left
	resumeWindow   = 30 * 24 * time.Hour
)

func playThreshold(durationMS int64) int64 {
	if durationMS > 0 && durationMS < minCountableMS {
		return -1 // never
	}
	need := int64(maxThresholdMS)
	if durationMS > 0 && durationMS/2 < need {
		need = durationMS / 2
	}
	return need
}

type PlayReport struct {
	Session    string `json:"session"`  // client-generated, one per playback
	AssetID    int64  `json:"asset_id"` //
	AlbumID    int64  `json:"album_id"` // album the track was played from, 0 if none
	PositionMS int64  `json:"position_ms"`
	ListenedMS int64  `json:"listened_ms"` // time actually heard so far in this session
	Finished   bool   `json:"finished"`
}

var ErrBadPlay = errors.New("invalid play report")

// RecordPlay creates or updates the session's row. Reports may repeat or arrive out of order:
// heard time only grows, and a counted or finished play stays so.
func (s *Store) RecordPlay(ctx context.Context, r PlayReport) error {
	if r.Session == "" || len(r.Session) > 64 || r.AssetID <= 0 || r.PositionMS < 0 || r.ListenedMS < 0 {
		return ErrBadPlay
	}
	var trackID, duration int64
	err := s.db.QueryRowContext(ctx, `SELECT ta.track_id, a.duration_ms FROM assets a
		JOIN track_assets ta ON ta.asset_id = a.id WHERE a.id = ? ORDER BY ta.track_id LIMIT 1`, r.AssetID).Scan(&trackID, &duration)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrBadPlay
	}
	if err != nil {
		return err
	}
	var album any
	if r.AlbumID > 0 {
		var ok int
		if s.db.QueryRowContext(ctx, `SELECT 1 FROM albums WHERE id = ?`, r.AlbumID).Scan(&ok) == nil {
			album = r.AlbumID
		}
	}
	if duration > 0 {
		r.PositionMS = min(r.PositionMS, duration)
	}
	need := playThreshold(duration)
	counted := 0
	if need >= 0 && r.ListenedMS >= need {
		counted = 1
	}
	finished := 0
	if r.Finished {
		finished = 1
	}
	now := db.Now()
	_, err = s.db.ExecContext(ctx, `INSERT INTO plays (session, asset_id, track_id, album_id, started_at, updated_at,
		position_ms, listened_ms, duration_ms, counted, finished) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (session) DO UPDATE SET
			updated_at = excluded.updated_at,
			position_ms = excluded.position_ms,
			listened_ms = max(plays.listened_ms, excluded.listened_ms),
			counted = max(plays.counted, excluded.counted,
				CASE WHEN ? >= 0 AND max(plays.listened_ms, excluded.listened_ms) >= ? THEN 1 ELSE 0 END),
			finished = max(plays.finished, excluded.finished)
		WHERE plays.asset_id = excluded.asset_id`,
		r.Session, r.AssetID, trackID, album, now, now, r.PositionMS, r.ListenedMS, duration, counted, finished, need, need)
	return err
}

// ResumePosition is where an unfinished recent playback of this file stopped, or 0.
func (s *Store) ResumePosition(ctx context.Context, assetID int64) (int64, error) {
	var pos, dur int64
	var finished int
	err := s.db.QueryRowContext(ctx, `SELECT position_ms, duration_ms, finished FROM plays
		WHERE asset_id = ? AND updated_at > ? ORDER BY updated_at DESC, id DESC LIMIT 1`,
		assetID, db.Now()-resumeWindow.Milliseconds()).Scan(&pos, &dur, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if finished == 1 || pos < resumeMarginMS || (dur > 0 && pos > dur-resumeMarginMS) {
		return 0, nil
	}
	return pos, nil
}

type ResumeItem struct {
	TrackItem
	PositionMS int64 `json:"position_ms"`
	// Finished: played to the end (or within the last 30 s), so "continue" means the next track.
	Finished bool `json:"finished"`
}

// unfinished lists the latest unfinished playback per file, newest first.
func (s *Store) unfinished(ctx context.Context, where string, limit int) ([]ResumeItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.track_id, p.position_ms, coalesce(p.album_id, 0) FROM plays p
		JOIN tracks t ON t.id = p.track_id
		WHERE p.id = (SELECT p2.id FROM plays p2 WHERE p2.asset_id = p.asset_id ORDER BY p2.updated_at DESC, p2.id DESC LIMIT 1)
		AND p.finished = 0 AND p.position_ms >= ? AND (p.duration_ms = 0 OR p.position_ms <= p.duration_ms - ?)
		AND p.updated_at > ? `+where+` ORDER BY p.updated_at DESC LIMIT ?`,
		resumeMarginMS, resumeMarginMS, db.Now()-resumeWindow.Milliseconds(), limit)
	if err != nil {
		return nil, err
	}
	type hit struct{ track, pos, album int64 }
	var hits []hit
	for rows.Next() {
		var h hit
		if err := rows.Scan(&h.track, &h.pos, &h.album); err != nil {
			rows.Close()
			return nil, err
		}
		hits = append(hits, h)
	}
	rows.Close()
	out := []ResumeItem{}
	for _, h := range hits {
		it, err := s.resumeItem(ctx, h.track, h.pos, h.album)
		if err != nil {
			return nil, err
		}
		if it != nil {
			out = append(out, *it)
		}
	}
	return out, nil
}

// resumeItem describes a track at a position, inside the album it was played from when known.
func (s *Store) resumeItem(ctx context.Context, trackID, pos, albumID int64) (*ResumeItem, error) {
	items, err := scanTracks(s.db.QueryContext(ctx, trackSQL+` WHERE t.id = ?`, trackID))
	if err != nil || len(items) == 0 {
		return nil, err
	}
	it := ResumeItem{TrackItem: items[0], PositionMS: pos}
	if albumID != 0 {
		var title string
		var cover int64
		if s.db.QueryRowContext(ctx, `SELECT title, coalesce(cover_id, 0) FROM albums WHERE id = ?`, albumID).Scan(&title, &cover) == nil {
			it.AlbumID, it.Album, it.CoverID = albumID, title, cover
		}
	}
	return &it, nil
}

// Continue is the latest playback, finished or not (decision D9, revised after use): the home page
// picks up where listening stopped, or with the next track when that one was played to the end.
func (s *Store) Continue(ctx context.Context) (*ResumeItem, error) {
	var track, pos, album, dur int64
	var finished int
	err := s.db.QueryRowContext(ctx, `SELECT track_id, position_ms, coalesce(album_id, 0), duration_ms, finished
		FROM plays WHERE updated_at > ? ORDER BY updated_at DESC, id DESC LIMIT 1`,
		db.Now()-resumeWindow.Milliseconds()).Scan(&track, &pos, &album, &dur, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	it, err := s.resumeItem(ctx, track, pos, album)
	if it != nil {
		it.Finished = finished == 1 || (dur > 0 && pos > dur-resumeMarginMS)
	}
	return it, err
}

// UnfinishedSpoken lists drama CDs and radio left partway.
func (s *Store) UnfinishedSpoken(ctx context.Context, limit int) ([]ResumeItem, error) {
	return s.unfinished(ctx, "AND t.kind = 'spoken'", limit)
}

// RecentlyPlayedAlbums orders albums by their latest playback.
func (s *Store) RecentlyPlayedAlbums(ctx context.Context, limit int) ([]AlbumSummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT album_id FROM plays WHERE album_id IS NOT NULL
		GROUP BY album_id ORDER BY max(updated_at) DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	var ids []any
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) == 0 {
		return []AlbumSummary{}, nil
	}
	list, err := scanAlbums(s.db.QueryContext(ctx, albumSummarySQL+` WHERE al.id IN (`+
		strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`) GROUP BY al.id`, ids...))
	if err != nil {
		return nil, err
	}
	byID := map[int64]AlbumSummary{}
	for _, a := range list {
		byID[a.ID] = a
	}
	out := make([]AlbumSummary, 0, len(ids))
	for _, id := range ids {
		if a, ok := byID[id.(int64)]; ok {
			out = append(out, a)
		}
	}
	return out, nil
}

// RandomAlbum picks an album that has playable music (drama CDs and radio are left out).
func (s *Store) RandomAlbum(ctx context.Context) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT al.id FROM albums al WHERE EXISTS (
		SELECT 1 FROM album_entries e JOIN tracks t ON t.id = e.track_id JOIN assets a ON a.id = e.asset_id
		WHERE e.album_id = al.id AND a.state = 'verified' AND t.kind = 'music') ORDER BY random() LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

type Attention struct {
	WithoutAlbum  int `json:"without_album"`  // standalone tracks: maybe an album to sort out
	UnknownArtist int `json:"unknown_artist"` // no artist tag
	FailedImports int `json:"failed_imports"` // import items waiting for a retry
}

func (s *Store) Attention(ctx context.Context) (Attention, error) {
	var a Attention
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM tracks t WHERE NOT EXISTS (SELECT 1 FROM album_entries e WHERE e.track_id = t.id)),
		(SELECT count(*) FROM tracks WHERE artist = ''),
		(SELECT count(*) FROM import_items WHERE state = 'failed')`).Scan(&a.WithoutAlbum, &a.UnknownArtist, &a.FailedImports)
	return a, err
}
