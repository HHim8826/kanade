package library

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/HHim8826/kanade/server/internal/db"
)

// Library files in storage locations besides Drive (#99): references "<kind>:<store>:<key>".

// refRange is the range the references of store id are in: from "<kind>:<id>:" up to the same
// ending in the character after the colon.
func refRange(kind string, id int64) (from, to string) {
	from = fmt.Sprintf("%s:%d:", kind, id)
	return from, from[:len(from)-1] + ";"
}

// likeRefs is the condition for column col being a reference in the store whose range is ?1, ?2
// (a range, so the index on the column serves it).
func likeRefs(col string) string { return col + ` >= ?1 AND ` + col + ` < ?2` }

// PlaceAsset makes an asset read from where a source has it (imported in place): the file at ref
// was read and hashed as the asset. Another asset that was there has other bytes now: it is
// missing, and no longer there. An asset the library already has elsewhere stays there (the file
// is a copy of it).
func (s *Store) PlaceAsset(ctx context.Context, assetID int64, ref string) error {
	if err := lockTrash(ctx, ref); err != nil {
		return err
	}
	defer trashMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The store is there still, in the same transaction as its removal would be (review #222).
	if store, ok := refStore(ref); ok {
		var there int
		switch err := tx.QueryRowContext(ctx, `SELECT 1 FROM stores WHERE id = ?`, store).Scan(&there); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrStoreGone
		case err != nil:
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE assets SET state = ?, file_ref = NULL WHERE file_ref = ? AND id != ?`,
		AssetMissing, ref, assetID); err != nil {
		return err
	}
	// A source's file the library let go of before is taken up again: nothing is owed for it.
	if _, err := tx.ExecContext(ctx, `DELETE FROM file_trash WHERE file_id = ?`, ref); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE assets SET state = ?, file_ref = ?, verified_at = ? WHERE id = ? AND state != ?`,
		AssetVerified, ref, db.Now(), assetID, AssetVerified); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkRefsMissing marks the assets at refs missing (their files are gone, or other bytes are
// there); they keep the references, so they come back if the files do (RestoreRefs). It returns
// how many changed.
func (s *Store) MarkRefsMissing(ctx context.Context, refs []string) (int, error) {
	return s.setRefsState(ctx, refs, AssetVerified, AssetMissing)
}

// RestoreRefs marks missing assets at refs verified again: their files are back as they were.
func (s *Store) RestoreRefs(ctx context.Context, refs []string) (int, error) {
	return s.setRefsState(ctx, refs, AssetMissing, AssetVerified)
}

func (s *Store) setRefsState(ctx context.Context, refs []string, from, to string) (int, error) {
	n := 0
	for _, ref := range refs {
		r, err := s.db.ExecContext(ctx, `UPDATE assets SET state = ? WHERE file_ref = ? AND state = ?`, to, ref, from)
		if err != nil {
			return n, err
		}
		k, _ := r.RowsAffected()
		n += int(k)
	}
	return n, nil
}

// MoveRef follows a file that moved within a source, found by its content: what the library had
// at from is at to now (and verified, as it was just read).
func (s *Store) MoveRef(ctx context.Context, from, to string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`UPDATE assets SET file_ref = ?1, state = '` + AssetVerified + `' WHERE file_ref = ?2`,
		`UPDATE covers SET file_ref = ?1 WHERE file_ref = ?2`,
		`UPDATE sidecars SET file_ref = ?1 WHERE file_ref = ?2`,
		`UPDATE album_images SET file_ref = ?1 WHERE file_ref = ?2`,
	} {
		if _, err := tx.ExecContext(ctx, q, to, from); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RefUse is a library file kept in a store, for checking the store holds it.
type RefUse struct {
	Ref   string
	Size  int64
	State string // an asset's: verified | missing; "" for covers, sidecars and pictures
}

// StoreRefs lists the library's files in a store: songs, covers, sidecars and the galleries' pictures.
func (s *Store) StoreRefs(ctx context.Context, kind string, id int64) ([]RefUse, error) {
	from, to := refRange(kind, id)
	rows, err := s.db.QueryContext(ctx, `SELECT file_ref, size, state FROM assets WHERE `+likeRefs("file_ref")+`
		UNION ALL SELECT file_ref, 0, '' FROM covers WHERE `+likeRefs("file_ref")+`
		UNION ALL SELECT file_ref, size, '' FROM sidecars WHERE `+likeRefs("file_ref")+`
		UNION ALL SELECT file_ref, size, '' FROM album_images WHERE hidden = 0 AND `+likeRefs("file_ref"), from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RefUse
	for rows.Next() {
		var u RefUse
		if err := rows.Scan(&u.Ref, &u.Size, &u.State); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// StoreUse counts the songs a store holds and their size, and those of them missing.
type StoreUse struct {
	Songs   int   `json:"songs"`
	Bytes   int64 `json:"bytes"`
	Missing int   `json:"missing"`
	Others  int   `json:"others"` // covers, sidecars and the galleries' pictures
}

func (s *Store) StoreUse(ctx context.Context, kind string, id int64) (StoreUse, error) {
	return storeUse(ctx, s.db, kind, id)
}

// StoreUseTx is StoreUse in tx: with the store's removal (review #236).
func (s *Store) StoreUseTx(ctx context.Context, tx *sql.Tx, kind string, id int64) (StoreUse, error) {
	return storeUse(ctx, tx, kind, id)
}

func storeUse(ctx context.Context, q querier, kind string, id int64) (StoreUse, error) {
	var u StoreUse
	from, to := refRange(kind, id)
	err := q.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(size), 0), coalesce(sum(state = ?3), 0) FROM assets
		WHERE `+likeRefs("file_ref"), from, to, AssetMissing).Scan(&u.Songs, &u.Bytes, &u.Missing)
	if err != nil {
		return u, err
	}
	err = q.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM covers WHERE `+likeRefs("file_ref")+`)
		+ (SELECT count(*) FROM sidecars WHERE `+likeRefs("file_ref")+`)
		+ (SELECT count(*) FROM album_images WHERE hidden = 0 AND missing = 0 AND `+likeRefs("file_ref")+`)`, from, to).Scan(&u.Others)
	return u, err
}

// ErrStoreInUse is a managed store that still holds library files: they would have nowhere to be.
var ErrStoreInUse = errors.New("the library still keeps files in this storage location")

// ForgetStore lets go of what the library has from a source being removed, in tx (with the store's
// removal, review #222): songs whose every file is there leave the library (as deleting them does:
// plays, favorites and playlist items with them); a file of a song that has others there too is
// missing; covers there are dropped from their albums, and sidecars and pictures from theirs. The files themselves
// are not touched. The edit log says it once, by the store's name (review #229). It returns how many
// songs left.
func (s *Store) ForgetStore(ctx context.Context, tx *sql.Tx, kind string, id int64, name string) (int, error) {
	from, to := refRange(kind, id)
	tracks, err := idsTx(ctx, tx, `SELECT DISTINCT ta.track_id FROM track_assets ta JOIN assets a ON a.id = ta.asset_id
		WHERE `+likeRefs("a.file_ref")+` AND NOT EXISTS (SELECT 1 FROM track_assets o JOIN assets b ON b.id = o.asset_id
		WHERE o.track_id = ta.track_id AND NOT (`+likeRefs("coalesce(b.file_ref, '')")+`))`, from, to)
	if err != nil {
		return 0, err
	}
	if _, err := deleteTracksTx(ctx, tx, tracks); err != nil { // their files' debts to the trash go below
		return 0, err
	}
	for _, q := range []string{
		`UPDATE assets SET state = '` + AssetMissing + `', file_ref = NULL WHERE ` + likeRefs("file_ref"),
		`UPDATE albums SET cover_id = NULL WHERE cover_id IN (SELECT id FROM covers WHERE ` + likeRefs("file_ref") + `)`,
		`DELETE FROM covers WHERE ` + likeRefs("file_ref") + ` AND id NOT IN (SELECT cover_id FROM albums WHERE cover_id IS NOT NULL)`,
		`DELETE FROM sidecars WHERE ` + likeRefs("file_ref"),
		`DELETE FROM album_images WHERE ` + likeRefs("file_ref"),
		`DELETE FROM file_trash WHERE ` + likeRefs("file_id"),
	} {
		if _, err := tx.ExecContext(ctx, q, from, to); err != nil {
			return 0, fmt.Errorf("%s: %w", strings.Fields(q)[0], err)
		}
	}
	if len(tracks) > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO edit_groups (source, summary, created_at) VALUES (?, ?, ?)`,
			SourceUser, fmt.Sprintf("移除儲存位置「%s」（%d 首）", name, len(tracks)), db.Now()); err != nil {
			return 0, err
		}
	}
	return len(tracks), nil
}

// ErrStoreGone is a source's file that cannot be placed: its store was removed (review #222).
var ErrStoreGone = errors.New("the storage location was removed")

// refStore is the store of a reference "<kind>:<store>:<key>"; false for a Drive file ID.
func refStore(ref string) (int64, bool) {
	_, rest, ok := strings.Cut(ref, ":")
	if !ok {
		return 0, false
	}
	n, _, ok := strings.Cut(rest, ":")
	id, err := strconv.ParseInt(n, 10, 64)
	return id, ok && err == nil
}

// A source's pictures are the library's covers where they are (#99); its scans follow them as they
// do its songs (review #221). A cover's content never changes: other bytes at its place are another
// cover, so what was made of the old one (its sizes, kept by its checksum; a browser's copy, kept by
// its ID) is never taken for the new one.

// SourceCover is a cover whose picture is in a source.
type SourceCover struct {
	ID     int64
	SHA256 string
	Ref    string
}

// CoversIn lists the covers whose pictures are in store id.
func (s *Store) CoversIn(ctx context.Context, kind string, id int64) ([]SourceCover, error) {
	from, to := refRange(kind, id)
	rows, err := s.db.QueryContext(ctx, `SELECT id, sha256, file_ref FROM covers WHERE `+likeRefs("file_ref"), from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SourceCover
	for rows.Next() {
		var c SourceCover
		if err := rows.Scan(&c.ID, &c.SHA256, &c.Ref); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MoveCover has cover id read from to: its picture moved there, or another copy of it is there.
func (s *Store) MoveCover(ctx context.Context, id int64, to string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE covers SET file_ref = ? WHERE id = ?`, to, id)
	return err
}

// LoseCover lets go of cover id, its picture gone or other bytes at its place, with no other copy:
// its albums have none (as when its source is removed), and it goes.
func (s *Store) LoseCover(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE albums SET cover_id = NULL, updated_at = ? WHERE cover_id = ?`, db.Now(), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM covers WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// FolderCover gives the albums of songs below dir in a source ("" for all of it; the paths its scans
// saw) whose cover is from the cover to: the folder's picture has other bytes now. It returns how many
// changed.
func (s *Store) FolderCover(ctx context.Context, kind string, store int64, dir string, from, to int64) (int, error) {
	prefix, _ := refRange(kind, store)
	q := `UPDATE albums SET cover_id = ?1, updated_at = ?2 WHERE cover_id = ?3 AND id IN (SELECT e.album_id FROM album_entries e
		JOIN assets a ON a.id = e.asset_id JOIN source_files f ON f.store_id = ?4 AND a.file_ref = ?5 || f.key`
	args := []any{to, db.Now(), from, store, prefix}
	if dir != "" {
		q += ` WHERE f.path >= ?6 AND f.path < ?7`
		args = append(args, dir+"/", dir+"0") // '0' follows '/'
	}
	r, err := s.db.ExecContext(ctx, q+`)`, args...)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	return int(n), nil
}
