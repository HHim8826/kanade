// Package library stores and queries assets, tracks, albums and artists (plan §4).
package library

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/HHim8826/kanade/server/internal/db"
)

type Store struct{ db *sql.DB }

func New(d *sql.DB) *Store { return &Store{db: d} }

const (
	AssetUploading = "uploading"
	AssetVerified  = "verified"
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

func (s *Store) MarkVerified(ctx context.Context, assetID int64, driveFileID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE assets SET state = ?, drive_file_id = ?, verified_at = ? WHERE id = ?`,
		AssetVerified, driveFileID, db.Now(), assetID)
	return err
}

// Normalize prepares text for the search index and for queries: NFKC folds full-width
// letters and half-width katakana; lowercasing makes Latin matching case-insensitive.
func Normalize(s string) string {
	return strings.ToLower(norm.NFKC.String(strings.TrimSpace(s)))
}

func index(ctx context.Context, tx *sql.Tx, kind string, id int64, parts ...string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM search_index WHERE kind = ? AND ref_id = ?`, kind, id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO search_index (kind, ref_id, text) VALUES (?, ?, ?)`,
		kind, id, Normalize(strings.Join(parts, " ")))
	return err
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
		albumID, err := upsertAlbum(ctx, tx, in, now)
		if err != nil {
			return res, err
		}
		disc := max(in.DiscNo, 1)
		r, err := tx.ExecContext(ctx, `INSERT INTO album_entries (album_id, track_id, asset_id, disc_no, track_no, created_at)
			VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (album_id, disc_no, track_no, asset_id) DO NOTHING`,
			albumID, res.TrackID, assetID, disc, in.TrackNo, now)
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

// upsertAlbum matches an existing album by exact title and album artist. P1 accepts that two
// different releases with identical names would merge; the P2 import preview lets users split them.
func upsertAlbum(ctx context.Context, tx *sql.Tx, in EntryInput, now int64) (int64, error) {
	var id, cover int64
	err := tx.QueryRowContext(ctx, `SELECT id, coalesce(cover_id, 0) FROM albums WHERE title = ? AND album_artist = ?`,
		in.Album, in.AlbumArtist).Scan(&id, &cover)
	switch {
	case err == nil:
		if cover == 0 && in.CoverID != 0 {
			_, err = tx.ExecContext(ctx, `UPDATE albums SET cover_id = ?, updated_at = ? WHERE id = ?`, in.CoverID, now, id)
		}
		return id, err
	case !errors.Is(err, sql.ErrNoRows):
		return 0, err
	}
	var coverArg any
	if in.CoverID != 0 {
		coverArg = in.CoverID
	}
	r, err := tx.ExecContext(ctx, `INSERT INTO albums (title, album_artist, date, cover_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`, in.Album, in.AlbumArtist, in.Date, coverArg, now, now)
	if err != nil {
		return 0, err
	}
	id, _ = r.LastInsertId()
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
