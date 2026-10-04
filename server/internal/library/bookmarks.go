package library

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/HHim8826/kanade/server/internal/db"
)

// Bookmarks are named places in songs (review #98), apart from where a playback last stopped. A
// bookmark is made on a file of the song; it plays at its place only on that file, so a song whose
// file changed since does not jump to the same second of other audio: the bookmark says so.

type Bookmark struct {
	ID         int64  `json:"id"`
	TrackID    int64  `json:"track_id"`
	PositionMS int64  `json:"position_ms"`
	Name       string `json:"name"`
	Note       string `json:"note"`
	CreatedAt  int64  `json:"created_at"`
	// Track is the song as it plays now; Asset is the file the bookmark was made on, set while that
	// file is still the song's and in the library (Moved otherwise).
	Track *TrackItem  `json:"track,omitempty"`
	Asset *AssetBrief `json:"asset,omitempty"`
	Moved bool        `json:"moved,omitempty"`
}

func cleanBookmark(name, note string) (string, string, error) {
	name, note = strings.TrimSpace(name), strings.TrimSpace(note)
	if name == "" {
		return "", "", invalid("a bookmark needs a name")
	}
	if utf8.RuneCountInString(name) > 100 || utf8.RuneCountInString(note) > 500 {
		return "", "", invalid("a bookmark's name is at most 100 characters, its note 500")
	}
	return name, note, nil
}

// AddBookmark marks a place in the song of a file.
func (s *Store) AddBookmark(ctx context.Context, assetID, positionMS int64, name, note string) (*Bookmark, error) {
	name, note, err := cleanBookmark(name, note)
	if err != nil {
		return nil, err
	}
	var trackID, duration int64
	err = s.db.QueryRowContext(ctx, `SELECT ta.track_id, a.duration_ms FROM assets a JOIN track_assets ta ON ta.asset_id = a.id
		WHERE a.id = ? ORDER BY ta.track_id LIMIT 1`, assetID).Scan(&trackID, &duration)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: file %d", ErrNotFound, assetID)
	}
	if err != nil {
		return nil, err
	}
	if positionMS < 0 || (duration > 0 && positionMS > duration) {
		return nil, invalid("the place is outside the song")
	}
	now := db.Now()
	r, err := s.db.ExecContext(ctx, `INSERT INTO bookmarks (track_id, asset_id, duration_ms, position_ms, name, note, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, trackID, assetID, duration, positionMS, name, note, now, now)
	if err != nil {
		return nil, err
	}
	id, _ := r.LastInsertId()
	list, err := s.bookmarks(ctx, `WHERE b.id = ?`, id)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

// Bookmarks lists a song's bookmarks in order of place (trackID > 0), or all of them, the latest
// made first.
func (s *Store) Bookmarks(ctx context.Context, trackID int64) ([]Bookmark, error) {
	if trackID > 0 {
		return s.bookmarks(ctx, `WHERE b.track_id = ? ORDER BY b.position_ms, b.id`, trackID)
	}
	return s.bookmarks(ctx, `ORDER BY b.created_at DESC, b.id DESC LIMIT 500`)
}

func (s *Store) bookmarks(ctx context.Context, where string, args ...any) ([]Bookmark, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT b.id, b.track_id, b.asset_id, b.duration_ms, b.position_ms, b.name, b.note, b.created_at,
		EXISTS (SELECT 1 FROM track_assets ta JOIN assets a ON a.id = ta.asset_id WHERE ta.track_id = b.track_id AND a.id = b.asset_id
			AND a.state = 'verified' AND (b.duration_ms = 0 OR abs(a.duration_ms - b.duration_ms) < 1000))
		FROM bookmarks b `+where, args...)
	if err != nil {
		return nil, err
	}
	type row struct {
		b      Bookmark
		asset  int64
		usable bool
	}
	var found []row
	for rows.Next() {
		var r row
		var duration int64
		if err := rows.Scan(&r.b.ID, &r.b.TrackID, &r.asset, &duration, &r.b.PositionMS, &r.b.Name, &r.b.Note, &r.b.CreatedAt, &r.usable); err != nil {
			rows.Close()
			return nil, err
		}
		found = append(found, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Bookmark, 0, len(found))
	for _, r := range found {
		b := r.b
		if t, err := scanTracks(s.db.QueryContext(ctx, trackSQL+` WHERE t.id = ?`, b.TrackID)); err != nil {
			return nil, err
		} else if len(t) > 0 {
			b.Track = &t[0]
		}
		if r.usable {
			var a AssetBrief
			if err := s.db.QueryRowContext(ctx, `SELECT `+briefCols+` FROM assets a WHERE a.id = ?`, r.asset).Scan(a.dest()...); err != nil {
				return nil, err
			}
			b.Asset = &a
		} else {
			b.Moved = true
		}
		out = append(out, b)
	}
	return out, nil
}

// UpdateBookmark renames a bookmark or changes its note.
func (s *Store) UpdateBookmark(ctx context.Context, id int64, name, note string) error {
	name, note, err := cleanBookmark(name, note)
	if err != nil {
		return err
	}
	r, err := s.db.ExecContext(ctx, `UPDATE bookmarks SET name = ?, note = ?, updated_at = ? WHERE id = ?`, name, note, db.Now(), id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: bookmark %d", ErrNotFound, id)
	}
	return nil
}

func (s *Store) DeleteBookmark(ctx context.Context, id int64) error {
	r, err := s.db.ExecContext(ctx, `DELETE FROM bookmarks WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: bookmark %d", ErrNotFound, id)
	}
	return nil
}
