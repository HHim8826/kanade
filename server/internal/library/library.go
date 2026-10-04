// Package library stores and queries assets, tracks, albums and artists (plan §4).
package library

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/HHim8826/kanade/server/internal/db"
)

type Store struct{ db *sql.DB }

func New(d *sql.DB) *Store { return &Store{db: d} }

const (
	AssetUploading = "uploading"
	AssetVerified  = "verified"
	AssetMissing   = "missing" // deleted or trashed in Drive; kept, and verified again if it comes back
)

type Asset struct {
	ID          int64  `json:"id"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	Format      string `json:"format"`
	Codec       string `json:"codec"`
	SampleRate  int    `json:"sample_rate"`
	BitDepth    int    `json:"bit_depth"`
	Channels    int    `json:"channels"`
	DurationMS  int64  `json:"duration_ms"`
	Bitrate     int    `json:"bitrate"`
	AudioMD5    string `json:"-"`
	DriveFileID string `json:"-"`
	State       string `json:"state"`
}

const assetCols = `id, sha256, size, format, codec, sample_rate, bit_depth, channels, duration_ms, bitrate, audio_md5, coalesce(drive_file_id, ''), state`

func scanAsset(row interface{ Scan(...any) error }) (*Asset, error) {
	var a Asset
	err := row.Scan(&a.ID, &a.SHA256, &a.Size, &a.Format, &a.Codec, &a.SampleRate, &a.BitDepth, &a.Channels,
		&a.DurationMS, &a.Bitrate, &a.AudioMD5, &a.DriveFileID, &a.State)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &a, err
}

func (s *Store) Asset(ctx context.Context, id int64) (*Asset, error) {
	return scanAsset(s.db.QueryRowContext(ctx, `SELECT `+assetCols+` FROM assets WHERE id = ?`, id))
}

func (s *Store) AssetByHash(ctx context.Context, sha string, size int64) (*Asset, error) {
	return scanAsset(s.db.QueryRowContext(ctx, `SELECT `+assetCols+` FROM assets WHERE sha256 = ? AND size = ?`, sha, size))
}

// CreateAsset inserts a new asset in the uploading state, or returns the existing one with the
// same content (the sha256+size unique constraint is the exact-duplicate rule).
func (s *Store) CreateAsset(ctx context.Context, a Asset) (*Asset, error) {
	_, err := s.db.ExecContext(ctx, `INSERT INTO assets (sha256, size, format, codec, sample_rate, bit_depth, channels,
		duration_ms, bitrate, audio_md5, state, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (sha256, size) DO NOTHING`,
		a.SHA256, a.Size, a.Format, a.Codec, a.SampleRate, a.BitDepth, a.Channels, a.DurationMS, a.Bitrate, a.AudioMD5,
		AssetUploading, db.Now())
	if err != nil {
		return nil, err
	}
	return s.AssetByHash(ctx, a.SHA256, a.Size)
}

// A Drive file that left the library and is still owed to the trash is taken up again here, which
// drops the debt; one that already went to the trash is refused with ErrInTrash (review #44).
func (s *Store) MarkVerified(ctx context.Context, assetID int64, driveFileID string) error {
	trashMu.Lock()
	defer trashMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var done int64
	switch err := tx.QueryRowContext(ctx, `SELECT done_at FROM drive_trash WHERE file_id = ?`, driveFileID).Scan(&done); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	case done > db.Now()-trashMemory.Milliseconds():
		return ErrInTrash
	default: // owed (or long since trashed and restored): nothing owed any more
		if _, err := tx.ExecContext(ctx, `DELETE FROM drive_trash WHERE file_id = ?`, driveFileID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE assets SET state = ?, drive_file_id = ?, verified_at = ? WHERE id = ?`,
		AssetVerified, driveFileID, db.Now(), assetID); err != nil {
		return err
	}
	return tx.Commit()
}

// Normalize prepares text for the search index and for queries (P2-2): NFKC folds full-width
// letters and half-width katakana, lowercasing makes Latin case-insensitive, katakana is folded
// to hiragana, and spaces, punctuation and symbols are dropped, so 「ハレ晴レ ユカイ」 finds
// 「ハレ晴レユカイ」 and 「かなで」 finds 「カナデ」. The long vowel mark ー stays.
func Normalize(s string) string { return fold(s, true) }

// fold is Normalize; with strip false it keeps spaces and symbols, for queries made only of them.
func fold(s string, strip bool) string {
	s = strings.ToLower(norm.NFKC.String(strings.TrimSpace(s)))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'ァ' && r <= 'ヶ', r == 'ヽ', r == 'ヾ':
			r -= 0x60
		case strip && (unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r)):
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// indexVersion changes whenever Normalize does; EnsureSearchIndex then rebuilds the index.
const indexVersion = "3"

// index stores the search text of one object. Parts are normalized one by one and kept on separate
// lines, so a query never matches across the end of a title and the start of a name.
func index(ctx context.Context, tx *sql.Tx, kind string, id int64, parts ...string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM search_index WHERE kind = ? AND ref_id = ?`, kind, id); err != nil {
		return err
	}
	var lines []string
	for _, p := range parts {
		if n := Normalize(p); n != "" {
			lines = append(lines, n)
		}
		if l := fold(p, false); l != Normalize(p) && strings.TrimSpace(l) != "" {
			lines = append(lines, l) // keeps symbols: see Search
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO search_index (kind, ref_id, text) VALUES (?, ?, ?)`,
		kind, id, strings.Join(lines, "\n"))
	return err
}

// EnsureSearchIndex rebuilds the search index when it was built by an older Normalize.
func (s *Store) EnsureSearchIndex(ctx context.Context) error {
	var v string
	s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'search_index'`).Scan(&v)
	if v == indexVersion {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM search_index`); err != nil {
		return err
	}
	for kind, table := range map[string]string{"track": "tracks", "album": "albums", "artist": "artists"} {
		ids, err := idsTx(ctx, tx, `SELECT id FROM `+table)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := reindex(ctx, tx, kind, id); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('search_index', ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, indexVersion); err != nil {
		return err
	}
	return tx.Commit()
}

// EntryInput is what one imported file contributes to the library.
type EntryInput struct {
	Title       string
	Artist      string
	Album       string // empty: a standalone track, no album is invented
	AlbumArtist string
	Date        string
	DiscNo      int
	TrackNo     int
	CoverID     int64  // 0 when none
	Kind        string // music | spoken; empty means music

	// Set by a planned import (P2-3).
	Tagged    *Tagged // what the file's tags said: the entry's identity, whatever the preview changed
	AlbumTags *Tagged // the album identity of the file's group (Album and AlbumArtist)
	AlbumID   int64   // join exactly this album: where an earlier file of the same group went
	NewAlbum  bool    // make a new album even if one was made from the same tags
	Chosen    bool    // the user chose that new album: even a file imported before goes into it (review #21)
}

// Tagged is the album identity a file's own tags give it. It is what later imports of the same
// file are matched by, whatever the preview or later edits changed.
type Tagged struct {
	Album, AlbumArtist string
	Disc, Track        int
	// Derived: the file names no album artist; AlbumArtist was worked out from the songs imported
	// with it, so a later import of it may work out another (review #81).
	Derived bool `json:",omitempty"`
}

type PublishResult struct {
	TrackID int64
	EntryID int64 // 0 for a standalone track
	Created bool  // false when the library already had exactly this
}

// Publish links a verified asset into the library in one transaction.
// The same asset always maps to the same track; tracks are never merged by title.
func (s *Store) Publish(ctx context.Context, assetID int64, in EntryInput) (PublishResult, error) {
	var res PublishResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	now := db.Now()

	err = tx.QueryRowContext(ctx, `SELECT track_id FROM track_assets WHERE asset_id = ? ORDER BY track_id LIMIT 1`, assetID).Scan(&res.TrackID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		kind := in.Kind
		if kind == "" {
			kind = "music"
		}
		r, err := tx.ExecContext(ctx, `INSERT INTO tracks (title, artist, kind, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
			in.Title, in.Artist, kind, now, now)
		if err != nil {
			return res, err
		}
		res.TrackID, _ = r.LastInsertId()
		res.Created = true
		if _, err := tx.ExecContext(ctx, `INSERT INTO track_assets (track_id, asset_id) VALUES (?, ?)`, res.TrackID, assetID); err != nil {
			return res, err
		}
		if in.Artist != "" {
			artistID, err := upsertArtist(ctx, tx, in.Artist)
			if err != nil {
				return res, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO track_artists (track_id, artist_id) VALUES (?, ?)`, res.TrackID, artistID); err != nil {
				return res, err
			}
		}
		if err := index(ctx, tx, "track", res.TrackID, in.Title, in.Artist); err != nil {
			return res, err
		}
	case err != nil:
		return res, err
	}

	if in.Album != "" {
		disc := max(in.DiscNo, 1)
		key, tagDisc, tagTrack := albumOrigin(in.Album, in.AlbumArtist), disc, in.TrackNo
		if t := in.Tagged; t != nil && t.Album != "" {
			key, tagDisc, tagTrack = albumOrigin(t.Album, t.AlbumArtist), max(t.Disc, 1), t.Track
		}
		origin := entryOrigin(key, tagDisc, tagTrack)
		if a := in.AlbumTags; a != nil && a.Album != "" {
			key = albumOrigin(a.Album, a.AlbumArtist) // the album is found or made by the group's identity
		}
		// The same file imported with the same tags is the entry made last time, wherever it has
		// been moved or renumbered since (P2-2) — unless the user chose a new album for it, which
		// gets an entry of its own sharing the file.
		if !(in.NewAlbum && in.Chosen) {
			err := tx.QueryRowContext(ctx, `SELECT id FROM album_entries WHERE asset_id = ? AND origin = ? ORDER BY id LIMIT 1`,
				assetID, origin).Scan(&res.EntryID)
			if errors.Is(err, sql.ErrNoRows) && in.Tagged != nil && in.Tagged.Derived {
				res.EntryID, err = sameEntry(ctx, tx, assetID, origin)
			}
			if err == nil {
				return res, tx.Commit()
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return res, err
			}
		}
		var albumID int64
		switch {
		case in.AlbumID != 0:
			albumID, err = joinAlbum(ctx, tx, in, now)
		case in.NewAlbum:
			albumID, err = createAlbum(ctx, tx, in, key, now)
		default:
			albumID, err = upsertAlbum(ctx, tx, in, key, now)
		}
		if err != nil {
			return res, err
		}
		r, err := tx.ExecContext(ctx, `INSERT INTO album_entries (album_id, track_id, asset_id, disc_no, track_no, created_at, origin)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (album_id, disc_no, track_no, asset_id) DO NOTHING`,
			albumID, res.TrackID, assetID, disc, in.TrackNo, now, origin)
		if err != nil {
			return res, err
		}
		if n, _ := r.RowsAffected(); n > 0 {
			res.Created = true
		}
		if err := tx.QueryRowContext(ctx, `SELECT id FROM album_entries WHERE album_id = ? AND disc_no = ? AND track_no = ? AND asset_id = ?`,
			albumID, disc, in.TrackNo, assetID).Scan(&res.EntryID); err != nil {
			return res, err
		}
	}
	return res, tx.Commit()
}

// sameEntry finds the entry an earlier import made of a file that names no album artist: the same
// album tag, disc and track, whatever album artist was worked out then from the songs imported with
// it (review #81: importing one song of an album again is not another album).
func sameEntry(ctx context.Context, tx *sql.Tx, assetID int64, origin string) (int64, error) {
	album, disc, track, ok := splitEntryOrigin(origin)
	title, _, _ := strings.Cut(album, sep)
	if !ok {
		return 0, sql.ErrNoRows
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, origin FROM album_entries WHERE asset_id = ? AND origin IS NOT NULL ORDER BY id`, assetID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var o string
		if err := rows.Scan(&id, &o); err != nil {
			return 0, err
		}
		a, d, t, ok := splitEntryOrigin(o)
		if t2, _, _ := strings.Cut(a, sep); ok && t2 == title && d == disc && t == track {
			return id, nil
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return 0, sql.ErrNoRows
}

func upsertArtist(ctx context.Context, tx *sql.Tx, name string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM artists WHERE name = ?`, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	r, err := tx.ExecContext(ctx, `INSERT INTO artists (name) VALUES (?)`, name)
	if err != nil {
		return 0, err
	}
	id, _ = r.LastInsertId()
	return id, index(ctx, tx, "artist", id, name)
}

// upsertAlbum finds the album first made from this title and album artist, even if it was renamed
// since, and follows it when it was merged into another. Two different releases with identical tags
// still meet here; the P2 import preview lets users keep them apart.
func upsertAlbum(ctx context.Context, tx *sql.Tx, in EntryInput, key string, now int64) (int64, error) {
	var id, cover int64
	var merged sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT id, coalesce(cover_id, 0), merged_into FROM albums WHERE origin = ? ORDER BY id LIMIT 1`,
		key).Scan(&id, &cover, &merged)
	switch {
	case err == nil:
		for hops := 0; merged.Valid && hops < 10; hops++ {
			id = merged.Int64
			if err := tx.QueryRowContext(ctx, `SELECT coalesce(cover_id, 0), merged_into FROM albums WHERE id = ?`, id).Scan(&cover, &merged); err != nil {
				return 0, err
			}
		}
		if cover == 0 && in.CoverID != 0 {
			_, err = tx.ExecContext(ctx, `UPDATE albums SET cover_id = ?, updated_at = ? WHERE id = ?`, in.CoverID, now, id)
		}
		return id, err
	case !errors.Is(err, sql.ErrNoRows):
		return 0, err
	}
	return createAlbum(ctx, tx, in, key, now)
}

// joinAlbum adds to the album given in AlbumID, filling in its cover when it has none.
func joinAlbum(ctx context.Context, tx *sql.Tx, in EntryInput, now int64) (int64, error) {
	var cover int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(cover_id, 0) FROM albums WHERE id = ?`, in.AlbumID).Scan(&cover); err != nil {
		return 0, err
	}
	if cover == 0 && in.CoverID != 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE albums SET cover_id = ?, updated_at = ? WHERE id = ?`, in.CoverID, now, in.AlbumID); err != nil {
			return 0, err
		}
	}
	return in.AlbumID, nil
}

// createAlbum makes an album that later imports with the same tags (key) will find.
func createAlbum(ctx context.Context, tx *sql.Tx, in EntryInput, key string, now int64) (int64, error) {
	var coverArg any
	if in.CoverID != 0 {
		coverArg = in.CoverID
	}
	r, err := tx.ExecContext(ctx, `INSERT INTO albums (title, album_artist, date, cover_id, origin, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, in.Album, in.AlbumArtist, in.Date, coverArg, key, now, now)
	if err != nil {
		return 0, err
	}
	id, _ := r.LastInsertId()
	return id, index(ctx, tx, "album", id, in.Album, in.AlbumArtist)
}

// CoverBySHA returns the cover with this content, or 0.
func (s *Store) CoverBySHA(ctx context.Context, sha string) (int64, string, error) {
	var id int64
	var driveID sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id, drive_file_id FROM covers WHERE sha256 = ?`, sha).Scan(&id, &driveID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	return id, driveID.String, err
}

func (s *Store) AddCover(ctx context.Context, sha, mime, driveFileID string) (int64, error) {
	_, err := s.db.ExecContext(ctx, `INSERT INTO covers (sha256, mime, drive_file_id, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (sha256) DO UPDATE SET drive_file_id = excluded.drive_file_id`, sha, mime, driveFileID, db.Now())
	if err != nil {
		return 0, err
	}
	id, _, err := s.CoverBySHA(ctx, sha)
	return id, err
}

type Cover struct {
	ID          int64
	SHA256      string
	MIME        string
	DriveFileID string
}

func (s *Store) Cover(ctx context.Context, id int64) (*Cover, error) {
	var c Cover
	err := s.db.QueryRowContext(ctx, `SELECT id, sha256, mime, coalesce(drive_file_id, '') FROM covers WHERE id = ?`, id).
		Scan(&c.ID, &c.SHA256, &c.MIME, &c.DriveFileID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &c, err
}

// AlbumNow is the album id is now, following merges, with its album artist; 0 when it is gone.
func (s *Store) AlbumNow(ctx context.Context, id int64) (int64, string, error) {
	for hops := 0; hops < 10; hops++ {
		var artist string
		var merged sql.NullInt64
		err := s.db.QueryRowContext(ctx, `SELECT album_artist, merged_into FROM albums WHERE id = ?`, id).Scan(&artist, &merged)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, "", nil
		}
		if err != nil {
			return 0, "", err
		}
		if !merged.Valid {
			return id, artist, nil
		}
		id = merged.Int64
	}
	return 0, "", errors.New("albums merged in a loop")
}

// ReplaceAlbumArtist changes an album's artist from one an import worked out to another, unless it
// was changed since (review #81).
func (s *Store) ReplaceAlbumArtist(ctx context.Context, id int64, from, to string) error {
	return s.replaceAlbum(ctx, id, "album_artist", from, to)
}

// ReplaceAlbumTitle changes an album's title from one an import gave it to another, unless it was
// changed since (review #86).
func (s *Store) ReplaceAlbumTitle(ctx context.Context, id int64, from, to string) error {
	return s.replaceAlbum(ctx, id, "title", from, to)
}

func (s *Store) replaceAlbum(ctx context.Context, id int64, column, from, to string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `UPDATE albums SET `+column+` = ?, updated_at = ? WHERE id = ? AND `+column+` = ?`, to, db.Now(), id, from)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return nil
	}
	if err := reindex(ctx, tx, "album", id); err != nil {
		return err
	}
	return tx.Commit()
}
