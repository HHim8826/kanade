package library

import (
	"context"
	"database/sql"
	"errors"
	"strings"

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

// mergedFrom is the album and every album merged into it, directly or through others: their
// sidecars are the album's (review #27). Undoing a merge gives them back by itself.
const mergedFrom = `WITH RECURSIVE family(id) AS (SELECT ?1 UNION SELECT a.id FROM albums a JOIN family f ON a.merged_into = f.id)`

// FindSidecar returns the sidecar with this content on this album (or one merged into it), if it
// was kept before.
func (s *Store) FindSidecar(ctx context.Context, albumID int64, sha string, size int64) (*Sidecar, error) {
	if albumID == 0 {
		return scanSidecar(s.db.QueryRowContext(ctx, `SELECT id, coalesce(album_id, 0), name, kind, size, drive_file_id FROM sidecars
			WHERE sha256 = ? AND size = ? AND album_id IS NULL`, sha, size))
	}
	return scanSidecar(s.db.QueryRowContext(ctx, mergedFrom+` SELECT id, coalesce(album_id, 0), name, kind, size, drive_file_id FROM sidecars
		WHERE sha256 = ?2 AND size = ?3 AND album_id IN (SELECT id FROM family) ORDER BY id LIMIT 1`, albumID, sha, size))
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
	// With the albums merged into it; the same file kept twice is listed once.
	rows, err := s.db.QueryContext(ctx, mergedFrom+` SELECT min(id), coalesce(album_id, 0), name, kind, size, drive_file_id FROM sidecars
		WHERE album_id IN (SELECT id FROM family) GROUP BY sha256, size ORDER BY kind, name`, albumID)
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

// SourceComplete reports whether a source was imported in full and still is (decision D2 §2–3,
// review #19): every output it should give (pieces: the CUE track numbers of a disc image, or 0
// for a converted file) is a verified library file that a song uses. Then the source is skipped;
// a partial import, or an output since deleted or missing from Drive, makes it import again.
func (s *Store) SourceComplete(ctx context.Context, sha string, size int64, pieces []int) (bool, error) {
	if len(pieces) == 0 {
		return false, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT src.piece FROM import_sources src JOIN assets a ON a.id = src.asset_id
		WHERE src.sha256 = ? AND src.size = ? AND a.state = ? AND EXISTS (SELECT 1 FROM track_assets ta WHERE ta.asset_id = a.id)`,
		sha, size, AssetVerified)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	have := map[int]bool{}
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return false, err
		}
		have[p] = true
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, p := range pieces {
		if !have[p] {
			return false, nil
		}
	}
	return true, nil
}

// AddSource records that an asset was made from a source file: piece is the CUE track number, or 0
// for a conversion.
func (s *Store) AddSource(ctx context.Context, sha string, size, assetID int64, kind string, piece int) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO import_sources (sha256, size, asset_id, kind, piece, created_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (sha256, size, asset_id) DO UPDATE SET piece = excluded.piece`,
		sha, size, assetID, kind, piece, db.Now())
	return err
}

// DriveObservation is what Drive says about one library file.
type DriveObservation struct {
	ID      string
	Present bool   // exists and is not in the trash
	SHA256  string // hex; "" when Drive did not say
	Size    int64  // 0 when Drive did not say
}

// ObserveDriveFile records what Drive says about a library file. A file that is there with the
// asset's content keeps (or gets back) verified; one that is gone, or whose bytes were replaced
// under the same file ID, is missing: the library never trusts content it has not checked. It
// reports how many assets changed state and whether they are now missing.
func (s *Store) ObserveDriveFile(ctx context.Context, o DriveObservation) (changed int, missing bool, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, sha256, size, state FROM assets WHERE drive_file_id = ? AND state IN (?, ?)`,
		o.ID, AssetVerified, AssetMissing)
	if err != nil {
		return 0, false, err
	}
	type asset struct {
		id         int64
		sha, state string
		size       int64
	}
	var list []asset
	for rows.Next() {
		var a asset
		if err := rows.Scan(&a.id, &a.sha, &a.size, &a.state); err != nil {
			rows.Close()
			return 0, false, err
		}
		list = append(list, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	for _, a := range list {
		same := o.Present && (o.SHA256 == "" || strings.EqualFold(o.SHA256, a.sha)) && (o.Size <= 0 || o.Size == a.size)
		to := AssetVerified
		if !same {
			to = AssetMissing
		}
		missing = !same
		if a.state == to {
			continue
		}
		r, err := s.db.ExecContext(ctx, `UPDATE assets SET state = ? WHERE id = ? AND state = ?`, to, a.id, a.state)
		if err != nil {
			return changed, missing, err
		}
		if n, _ := r.RowsAffected(); n > 0 {
			changed++
		}
	}
	return changed, missing, nil
}

// DriveFiles lists the Drive file IDs of library files that should be there (verified or missing).
func (s *Store) DriveFiles(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT drive_file_id, state FROM assets WHERE drive_file_id IS NOT NULL AND state IN (?, ?)`,
		AssetVerified, AssetMissing)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, state string
		if err := rows.Scan(&id, &state); err != nil {
			return nil, err
		}
		out[id] = state
	}
	return out, rows.Err()
}

// MissingItem is a library song whose file is gone from Drive.
type MissingItem struct {
	TrackID int64  `json:"track_id"`
	Title   string `json:"title"`
	Artist  string `json:"artist"`
	Album   string `json:"album,omitempty"`
	AlbumID int64  `json:"album_id,omitempty"`
	Format  string `json:"format"`
	Size    int64  `json:"size"`
}

func (s *Store) Missing(ctx context.Context) ([]MissingItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id, t.title, t.artist, coalesce(al.title, ''), coalesce(al.id, 0), a.format, a.size
		FROM assets a JOIN track_assets ta ON ta.asset_id = a.id JOIN tracks t ON t.id = ta.track_id
		LEFT JOIN album_entries e ON e.id = (SELECT min(id) FROM album_entries WHERE asset_id = a.id)
		LEFT JOIN albums al ON al.id = e.album_id
		WHERE a.state = ? ORDER BY al.title, t.title`, AssetMissing)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MissingItem{}
	for rows.Next() {
		var m MissingItem
		if err := rows.Scan(&m.TrackID, &m.Title, &m.Artist, &m.Album, &m.AlbumID, &m.Format, &m.Size); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---- the Drive trash owed (review #26) ----

// TrashDue lists files owed to the Drive trash whose next try is due.
func (s *Store) TrashDue(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT file_id FROM drive_trash WHERE next_at <= ? ORDER BY created_at LIMIT ?`, db.Now(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// TrashDone records that Drive has the file in its trash, or no longer has it.
func (s *Store) TrashDone(ctx context.Context, fileID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM drive_trash WHERE file_id = ?`, fileID)
	return err
}

// TrashFailed records a failed try; the next waits longer (a minute, doubling, at most a day).
func (s *Store) TrashFailed(ctx context.Context, fileID string, cause error) error {
	_, err := s.db.ExecContext(ctx, `UPDATE drive_trash SET tries = tries + 1, last_error = ?,
		next_at = ? + min(60000 << min(tries, 11), 86400000) WHERE file_id = ?`, cause.Error(), db.Now(), fileID)
	return err
}

// TrashPending counts files still owed to the Drive trash.
func (s *Store) TrashPending(ctx context.Context) (int, string, error) {
	var n int
	var last string
	err := s.db.QueryRowContext(ctx, `SELECT count(*), coalesce((SELECT last_error FROM drive_trash WHERE last_error != ''
		ORDER BY next_at DESC LIMIT 1), '') FROM drive_trash`).Scan(&n, &last)
	return n, last, err
}
