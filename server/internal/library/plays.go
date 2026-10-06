package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"slices"
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
	// Seq counts the session's reports up from 1; a report older than one already recorded does not
	// change the position or the time (review #8). 0: an older client, taken as it comes.
	Seq int64 `json:"seq"`
	// At is the client's clock when the report was made (ms); 0 when unknown.
	At int64 `json:"at"`
	// Client tells apart the devices (logins) reports come from, for their clock offsets; set by
	// the server, never by the client.
	Client string `json:"-"`
}

// offsetWindow is how far back a device's reports measure its clock offset.
const offsetWindow = 12 * time.Hour

var ErrBadPlay = errors.New("invalid play report")

// Heard time is real time (review #102): no playback hears more than a day, and a playback hears no
// more than the time since it started, give or take the reports' clocks (heardSlackMS). A first
// report hears no more than the file, as playback reports from its start.
const (
	maxHeardMS   = 24 * 60 * 60_000
	heardSlackMS = 2 * 60_000
)

// RecordPlay creates or updates the session's row. Reports may repeat or arrive out of order:
// heard time only grows, and a counted or finished play stays so.
func (s *Store) RecordPlay(ctx context.Context, r PlayReport) error {
	if r.Session == "" || len(r.Session) > 64 || r.AssetID <= 0 || r.PositionMS < 0 || r.ListenedMS < 0 || r.ListenedMS > maxHeardMS {
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
	finished := 0
	if r.Finished {
		finished = 1
	}
	// When the report was made, on the server's clock (review #8). Receiving it took the device's
	// clock offset plus the time it spent in the network, which is never negative: so the offset is
	// the smallest gap of the device's recent reports, this one included, and a report that sat in
	// the network, even a playback's first, keeps the time it was made. Never later than now.
	now := db.Now()
	at, sample := now, int64(0)
	if r.At > 0 {
		sample = now - r.At
		offset := sample
		var least sql.NullInt64
		s.db.QueryRowContext(ctx, `SELECT min(skew) FROM (SELECT skew FROM plays WHERE client = ? AND seq > 0 AND updated_at > ?
			ORDER BY updated_at DESC LIMIT 50)`, r.Client, now-offsetWindow.Milliseconds()).Scan(&least)
		if least.Valid {
			offset = min(offset, least.Int64)
		}
		at = min(now, r.At+offset)
	}
	// The play and what it adds to the listening spans change together (review #93): heard time
	// before and after the report tell what this one adds.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var before struct {
		heard, started int64
		counted        bool
	}
	err = tx.QueryRowContext(ctx, `SELECT listened_ms, started_at, counted FROM plays WHERE session = ?`, r.Session).
		Scan(&before.heard, &before.started, &before.counted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		r.ListenedMS = min(r.ListenedMS, max(duration, 0)+heardSlackMS)
	case err != nil:
		return err
	default:
		r.ListenedMS = min(r.ListenedMS, max(before.heard, at-min(before.started, at)+heardSlackMS))
	}
	counted := 0
	if need >= 0 && r.ListenedMS >= need {
		counted = 1
	}
	// A newer report (higher seq) sets the position and the time, never earlier than the last; an
	// older one arriving late changes neither, but can only move the start earlier. Heard time,
	// counting and finishing only ever grow, whatever the order. skew keeps the session's smallest
	// gap, for the offsets of later reports.
	_, err = tx.ExecContext(ctx, `INSERT INTO plays (session, asset_id, track_id, album_id, started_at, updated_at,
		position_ms, listened_ms, duration_ms, counted, finished, seq, skew, client) VALUES (?1, ?2, ?3, ?4, ?5, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?15)
		ON CONFLICT (session) DO UPDATE SET
			updated_at = CASE WHEN excluded.seq = 0 THEN ?13
				WHEN excluded.seq > plays.seq THEN max(plays.updated_at, ?5)
				ELSE plays.updated_at END,
			started_at = CASE WHEN excluded.seq > 0 THEN min(plays.started_at, ?5) ELSE plays.started_at END,
			position_ms = CASE WHEN excluded.seq = 0 OR excluded.seq > plays.seq THEN excluded.position_ms ELSE plays.position_ms END,
			seq = max(plays.seq, excluded.seq),
			skew = CASE WHEN excluded.seq > 0 THEN min(plays.skew, excluded.skew) ELSE plays.skew END,
			listened_ms = max(plays.listened_ms, excluded.listened_ms),
			counted = max(plays.counted, excluded.counted,
				CASE WHEN ?14 >= 0 AND max(plays.listened_ms, excluded.listened_ms) >= ?14 THEN 1 ELSE 0 END),
			finished = max(plays.finished, excluded.finished)
		WHERE plays.asset_id = excluded.asset_id`,
		r.Session, r.AssetID, trackID, album, at, r.PositionMS, r.ListenedMS, duration, counted, finished, r.Seq, sample, now, need, r.Client)
	if err != nil {
		return err
	}
	var id, heard int64
	var nowCounted bool
	if err := tx.QueryRowContext(ctx, `SELECT id, listened_ms, counted FROM plays WHERE session = ?`, r.Session).Scan(&id, &heard, &nowCounted); err != nil {
		return err
	}
	if added, reached := heard-before.heard, nowCounted && !before.counted; added > 0 || reached {
		if err := addListening(ctx, tx, id, at, max(added, 0), reached, false); err != nil {
			return err
		}
	}
	return tx.Commit()
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
		// The album as it is now, merged or not (review #152), as long as the song is in it.
		var title string
		var cover int64
		if now, _, err := s.AlbumNow(ctx, albumID); err == nil && now != 0 &&
			s.db.QueryRowContext(ctx, `SELECT title, coalesce(cover_id, 0) FROM albums al WHERE id = ?
				AND EXISTS (SELECT 1 FROM album_entries e WHERE e.album_id = al.id AND e.track_id = ?)`, now, trackID).Scan(&title, &cover) == nil {
			it.AlbumID, it.Album, it.CoverID = now, title, cover
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

// RecentlyPlayedAlbums orders albums by their latest playback: played from an album that was merged
// since, the album it went into (review #152); one emptied (removed, its row kept for undo) takes no
// place, so earlier ones show instead (review #194). The playbacks are read from the latest back, a
// few at a time, until there are limit albums: not the whole history (review #186). Each few go on
// from where the last stopped, and leave out the albums met already, so a history of a few albums
// is gone through once, by SQLite, not read again and again (review #191).
func (s *Store) RecentlyPlayedAlbums(ctx context.Context, limit int) ([]AlbumSummary, error) {
	var ids []any
	seen := map[int64]bool{} // albums listed
	now := map[int64]int64{} // album played from -> the album it shows as (0: none)
	at := playAt{math.MaxInt64, math.MaxInt64}
	batch := max(limit*8, 64)
	for len(ids) < limit {
		played, next, err := s.recentAlbums(ctx, at, now, batch)
		if err != nil {
			return nil, err
		}
		for _, a := range played {
			if len(ids) == limit {
				break
			}
			to, ok := now[a]
			if !ok {
				if to, err = s.shownAlbum(ctx, a); err != nil && ctx.Err() != nil {
					return nil, err
				}
				now[a] = to
			}
			if to != 0 && !seen[to] {
				seen[to] = true
				ids = append(ids, to)
			}
		}
		if len(played) < batch {
			break
		}
		at = next
	}
	if len(ids) == 0 {
		return []AlbumSummary{}, nil
	}
	list, err := scanAlbums(s.db.QueryContext(ctx, albumSummarySQL+` WHERE al.id IN (`+
		strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`) GROUP BY al.id `+listed, ids...))
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

// shownAlbum is the album a playback was from as lists show it: the one it was merged into, if it
// was; 0 when it is no more, has no songs left, or was merged in a loop.
func (s *Store) shownAlbum(ctx context.Context, id int64) (int64, error) {
	to, _, err := s.AlbumNow(ctx, id)
	if err != nil || to == 0 {
		return 0, err
	}
	var songs bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM album_entries WHERE album_id = ?)`, to).Scan(&songs); err != nil || !songs {
		return 0, err
	}
	return to, nil
}

// playAt is where a playback is in the history, the latest first: by when it was last played, then
// by its ID (ties of the same millisecond).
type playAt struct{ updated, id int64 }

// recentAlbums are the albums of the playbacks before at, the latest first (through plays_recent),
// but for those met already, and where the last of them is.
func (s *Store) recentAlbums(ctx context.Context, before playAt, met map[int64]int64, limit int) ([]int64, playAt, error) {
	known, err := json.Marshal(slices.AppendSeq([]int64{}, maps.Keys(met))) // [], never null: NOT IN (NULL) leaves all out
	if err != nil {
		return nil, before, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, updated_at, album_id FROM plays INDEXED BY plays_recent
		WHERE album_id IS NOT NULL AND (updated_at < ?1 OR (updated_at = ?1 AND id < ?2))
			AND album_id NOT IN (SELECT value FROM json_each(?3))
		ORDER BY updated_at DESC, id DESC LIMIT ?4`, before.updated, before.id, string(known), limit)
	if err != nil {
		return nil, before, err
	}
	defer rows.Close()
	var out []int64
	last := before
	for rows.Next() {
		var id int64
		if err := rows.Scan(&last.id, &last.updated, &id); err != nil {
			return nil, before, err
		}
		out = append(out, id)
	}
	return out, last, rows.Err()
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
	Missing       int `json:"missing"`        // files deleted or trashed in Drive (P2-6)
}

func (s *Store) Attention(ctx context.Context) (Attention, error) {
	var a Attention
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM tracks t WHERE NOT EXISTS (SELECT 1 FROM album_entries e WHERE e.track_id = t.id)),
		(SELECT count(*) FROM tracks WHERE artist = ''),
		(SELECT count(*) FROM import_items WHERE state = 'failed'),
		(SELECT count(*) FROM assets WHERE state = ?)`, AssetMissing).Scan(&a.WithoutAlbum, &a.UnknownArtist, &a.FailedImports, &a.Missing)
	return a, err
}
