package library

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/HHim8826/kanade/server/internal/db"
)

// Albums for standalone tracks imported from folders that name the album (the folder rule of
// the importer, applied afterwards to files imported before it).

// NewAlbum is an album to make of standalone tracks, or an album of the library they join.
type NewAlbum struct {
	Title, AlbumArtist string
	// Tagged is the identity the folder gives the album, as the importer's plan would: later imports
	// of the folder find the album by it, and each entry is the one a re-import of its file makes.
	Tagged Tagged
	// AlbumID, when set, is the album to join: where the folder's tagged files went. Its origin is
	// then the identity, and Title, AlbumArtist and Tagged are not used.
	AlbumID int64
	Entries []NewEntry
}

type NewEntry struct {
	TrackID, AssetID int64
	Disc, Track      int // the numbers the file's tags or name give
}

// MakeAlbums makes the albums (or fills the albums to join) as one action; an album the folder's
// later imports already made is filled instead. A track that is on an album by now is left alone; a
// track without an artist gets the album artist, as an import does. Undo takes it all back.
func (s *Store) MakeAlbums(ctx context.Context, albums []NewAlbum) (int64, error) {
	tracks := 0
	for i, a := range albums {
		albums[i].Title, albums[i].AlbumArtist = strings.TrimSpace(a.Title), strings.TrimSpace(a.AlbumArtist)
		if (a.AlbumID == 0 && (albums[i].Title == "" || a.Tagged.Album == "")) || len(a.Entries) == 0 {
			return 0, invalid("an album needs a title and songs")
		}
		tracks += len(a.Entries)
	}
	summary := fmt.Sprintf("依資料夾整理 %d 張專輯（%d 首）", len(albums), tracks)
	if len(albums) == 1 {
		title := albums[0].Title
		if albums[0].AlbumID != 0 {
			title, _ = s.name(ctx, "album", albums[0].AlbumID)
		}
		summary = fmt.Sprintf("依資料夾整理專輯「%s」（%d 首）", title, tracks)
	}
	return s.edit(ctx, SourceUser, summary, func(e *editor) error {
		for _, a := range albums {
			if err := e.makeAlbum(a); err != nil {
				return err
			}
		}
		return nil
	})
}

func (e *editor) makeAlbum(a NewAlbum) error {
	ctx, tx := e.ctx, e.tx
	key := albumOrigin(a.Tagged.Album, a.Tagged.AlbumArtist)
	var albumID int64
	var merged sql.NullInt64
	var err error
	if a.AlbumID != 0 {
		var title, artist string
		var origin sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT id, title, album_artist, origin, merged_into FROM albums WHERE id = ?`, a.AlbumID).
			Scan(&albumID, &title, &artist, &origin, &merged)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: album %d", ErrNotFound, a.AlbumID)
		}
		key = albumOrigin(title, artist)
		if origin.Valid {
			key = origin.String
		}
	} else {
		err = tx.QueryRowContext(ctx, `SELECT id, merged_into FROM albums WHERE origin = ? ORDER BY id LIMIT 1`, key).Scan(&albumID, &merged)
	}
	switch {
	case err == nil:
		for hops := 0; merged.Valid && hops < 10; hops++ {
			albumID = merged.Int64
			if err := tx.QueryRowContext(ctx, `SELECT merged_into FROM albums WHERE id = ?`, albumID).Scan(&merged); err != nil {
				return err
			}
		}
	case errors.Is(err, sql.ErrNoRows):
		now := db.Now()
		r, err := tx.ExecContext(ctx, `INSERT INTO albums (title, album_artist, date, origin, created_at, updated_at)
			VALUES (?, ?, '', ?, ?, ?)`, a.Title, a.AlbumArtist, key, now, now)
		if err != nil {
			return err
		}
		albumID, _ = r.LastInsertId()
		e.markIndex("album", albumID)
	default:
		return err
	}
	for _, en := range a.Entries {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM track_assets WHERE track_id = ? AND asset_id = ?
			AND NOT EXISTS (SELECT 1 FROM album_entries WHERE track_id = ?)`, en.TrackID, en.AssetID, en.TrackID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			continue // on an album by now, or not this track's file
		}
		if err != nil {
			return err
		}
		disc := max(en.Disc, 1)
		r, err := tx.ExecContext(ctx, `INSERT INTO album_entries (album_id, track_id, asset_id, disc_no, track_no, created_at, origin)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (album_id, disc_no, track_no, asset_id) DO NOTHING`,
			albumID, en.TrackID, en.AssetID, disc, en.Track, db.Now(), entryOrigin(key, disc, en.Track))
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			continue
		}
		id, _ := r.LastInsertId()
		row, _, err := e.current("entry", id, "row")
		if err != nil {
			return err
		}
		if err := e.record("entry", id, "row", nil, row); err != nil {
			return err
		}
		var artist, albumArtist string
		if err := tx.QueryRowContext(ctx, `SELECT t.artist, al.album_artist FROM tracks t, albums al WHERE t.id = ? AND al.id = ?`,
			en.TrackID, albumID).Scan(&artist, &albumArtist); err != nil {
			return err
		}
		if artist == "" && albumArtist != "" && !strings.EqualFold(albumArtist, "Various Artists") {
			if err := e.set(Change{"track", en.TrackID, "artist", Str(albumArtist)}); err != nil {
				return err
			}
		}
	}
	return nil
}
