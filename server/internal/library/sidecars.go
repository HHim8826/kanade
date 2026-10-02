package library

import (
	"context"
	"database/sql"
	"errors"

	"github.com/HHim8826/kanade/server/internal/db"
)

// Sidecar is a file kept with an album in Drive, such as a CUE sheet or a rip log (decision D2 §5).
type Sidecar struct {
	ID          int64  `json:"id"`
	AlbumID     int64  `json:"album_id,omitempty"`
	Name        string `json:"name"`
	Kind        string `json:"kind"` // cue | log
	Size        int64  `json:"size"`
	DriveFileID string `json:"-"`
}

// FindSidecar returns the sidecar with this content on this album, if it was kept before.
func (s *Store) FindSidecar(ctx context.Context, albumID int64, sha string, size int64) (*Sidecar, error) {
	return scanSidecar(s.db.QueryRowContext(ctx, `SELECT id, coalesce(album_id, 0), name, kind, size, drive_file_id FROM sidecars
		WHERE sha256 = ? AND size = ? AND album_id IS ?`, sha, size, nullID(albumID)))
}

func (s *Store) AddSidecar(ctx context.Context, albumID int64, name, kind, sha string, size int64, driveFileID string) (int64, error) {
	r, err := s.db.ExecContext(ctx, `INSERT INTO sidecars (album_id, name, kind, sha256, size, drive_file_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, nullID(albumID), name, kind, sha, size, driveFileID, db.Now())
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

func (s *Store) Sidecar(ctx context.Context, id int64) (*Sidecar, error) {
	return scanSidecar(s.db.QueryRowContext(ctx, `SELECT id, coalesce(album_id, 0), name, kind, size, drive_file_id FROM sidecars
		WHERE id = ?`, id))
}

func (s *Store) albumSidecars(ctx context.Context, albumID int64) ([]Sidecar, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, coalesce(album_id, 0), name, kind, size, drive_file_id FROM sidecars
		WHERE album_id = ? ORDER BY kind, name`, albumID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Sidecar{}
	for rows.Next() {
		var c Sidecar
		if err := rows.Scan(&c.ID, &c.AlbumID, &c.Name, &c.Kind, &c.Size, &c.DriveFileID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanSidecar(row *sql.Row) (*Sidecar, error) {
	var c Sidecar
	err := row.Scan(&c.ID, &c.AlbumID, &c.Name, &c.Kind, &c.Size, &c.DriveFileID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &c, err
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// SourceImported reports whether files were already made from this source (decision D2 §2–3: an
// APE file or a disc image imported once is skipped the next time).
func (s *Store) SourceImported(ctx context.Context, sha string, size int64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM import_sources WHERE sha256 = ? AND size = ? LIMIT 1`, sha, size).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// AddSource records that an asset was made from a source file.
func (s *Store) AddSource(ctx context.Context, sha string, size, assetID int64, kind string) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO import_sources (sha256, size, asset_id, kind, created_at)
		VALUES (?, ?, ?, ?, ?)`, sha, size, assetID, kind, db.Now())
	return err
}
