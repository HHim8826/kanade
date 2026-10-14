package library

import (
	"context"
	"database/sql"
	"errors"
	"path"
	"strings"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
)

// ---- moving files to another storage location ----

// FileToMove is a library file that can be moved: a song's, a cover's, a sidecar's, a gallery
// picture's (or one file that is several of these).
type FileToMove struct {
	Ref    string `json:"ref"`
	Size   int64  `json:"size"` // 0 for a cover: not recorded
	SHA256 string `json:"sha256"`
	MIME   string `json:"mime"`
	Kind   string `json:"kind"` // song | cover | sidecar | image
	Name   string `json:"name"` // the song's title, the file's name; the album's for a cover
}

// filesSQL lists the library's files whose reference meets cond (on file_ref, with the query's
// arguments), each once: songs first, then covers, sidecars and pictures.
func filesSQL(cond func(col string) string) string {
	return `SELECT file_ref, max(size), sha256, mime, kind, name FROM (
		SELECT a.file_ref, a.size, a.sha256, CASE a.format WHEN 'flac' THEN 'audio/flac' WHEN 'mp3' THEN 'audio/mpeg' WHEN 'm4a' THEN 'audio/mp4'
			WHEN 'ogg' THEN 'audio/ogg' WHEN 'opus' THEN 'audio/ogg' ELSE 'application/octet-stream' END AS mime, 'song' AS kind,
			coalesce((SELECT t.title FROM track_assets ta JOIN tracks t ON t.id = ta.track_id WHERE ta.asset_id = a.id ORDER BY t.id LIMIT 1), '') AS name,
			1 AS rank, a.id AS n FROM assets a WHERE a.state = '` + AssetVerified + `' AND ` + cond("a.file_ref") + `
		UNION ALL SELECT c.file_ref, 0, c.sha256, c.mime, 'cover', coalesce((SELECT al.title FROM albums al WHERE al.cover_id = c.id ORDER BY al.id LIMIT 1), ''),
			2, c.id FROM covers c WHERE ` + cond("c.file_ref") + `
		UNION ALL SELECT s.file_ref, s.size, s.sha256, 'text/plain', 'sidecar', s.name, 3, s.id FROM sidecars s WHERE ` + cond("s.file_ref") + `
		UNION ALL SELECT i.file_ref, i.size, i.sha256, i.mime, 'image', i.path, 4, i.id FROM album_images i WHERE i.hidden = 0 AND ` + cond("i.file_ref") + `
	) GROUP BY file_ref ORDER BY min(rank), min(n)`
}

func (s *Store) files(ctx context.Context, q string, args ...any) ([]FileToMove, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FileToMove{}
	for rows.Next() {
		var f FileToMove
		if err := rows.Scan(&f.Ref, &f.Size, &f.SHA256, &f.MIME, &f.Kind, &f.Name); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FilesAt lists the library's files in a store (kind and id), or in Google Drive (kind "drive").
func (s *Store) FilesAt(ctx context.Context, kind string, id int64) ([]FileToMove, error) {
	if kind == "drive" {
		return s.files(ctx, filesSQL(func(col string) string { return col + ` != '' AND instr(` + col + `, ':') = 0` }))
	}
	from, to := refRange(kind, id)
	return s.files(ctx, filesSQL(likeRefs), from, to)
}

// AlbumFiles lists the files of albums (as they are now: merged since, the album they went into),
// with the albums merged into them: their songs, cover, sidecars and gallery.
func (s *Store) AlbumFiles(ctx context.Context, ids []int64) ([]FileToMove, error) {
	seen := map[string]bool{}
	out := []FileToMove{}
	for _, id := range ids {
		id, _, err := s.AlbumNow(ctx, id)
		if err != nil {
			return nil, err
		}
		if id == 0 {
			continue
		}
		list, err := s.files(ctx, mergedFrom+` `+filesSQL(func(col string) string {
			return col + ` IN (SELECT a.file_ref FROM album_entries e JOIN assets a ON a.id = e.asset_id WHERE e.album_id IN (SELECT id FROM family)
				UNION SELECT c.file_ref FROM albums al JOIN covers c ON c.id = al.cover_id WHERE al.id IN (SELECT id FROM family)
				UNION SELECT file_ref FROM sidecars WHERE album_id IN (SELECT id FROM family)
				UNION SELECT file_ref FROM album_images WHERE hidden = 0 AND album_id IN (SELECT id FROM family))`
		}), id)
		if err != nil {
			return nil, err
		}
		for _, f := range list {
			if !seen[f.Ref] {
				seen[f.Ref] = true
				out = append(out, f)
			}
		}
	}
	return out, nil
}

// FileHome is the album a file goes with, for the folder a managed location keeps it in: the album
// artist and title of the album a song is on (the first), a sidecar or a picture belongs to; for a
// picture, also the folders below the album's it was in. A file of no album has none.
func (s *Store) FileHome(ctx context.Context, ref string) (artist, album, sub string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT artist, title, sub FROM (
		SELECT al.album_artist AS artist, al.title, '' AS sub, 1 AS rank, e.id AS n FROM assets a
			JOIN album_entries e ON e.asset_id = a.id JOIN albums al ON al.id = e.album_id
			WHERE a.file_ref = ?1 AND a.state = '`+AssetVerified+`'
		UNION ALL SELECT al.album_artist, al.title, '', 2, s.id FROM sidecars s JOIN albums al ON al.id = s.album_id WHERE s.file_ref = ?1
		UNION ALL SELECT al.album_artist, al.title, i.path, 3, i.id FROM album_images i JOIN albums al ON al.id = i.album_id WHERE i.file_ref = ?1
	) ORDER BY rank, n LIMIT 1`, ref).Scan(&artist, &album, &sub)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", nil
	}
	if sub != "" {
		sub = path.Dir(sub)
	}
	return artist, album, sub, err
}

// FileUsed says whether the library still reads a file at ref: it may have been deleted, or moved
// by another, since it was listed.
func (s *Store) FileUsed(ctx context.Context, ref string) (bool, error) {
	return refUsedTx(ctx, s.db, ref)
}

// Relocate has the library read what it had at from at to instead: a copy of the same bytes, kept
// and checked there. The file at from is owed to its location's trash after a while (streams
// reading it finish first); none of the library using from any more (deleted, or moved by another
// meanwhile), the copy at to is owed to the trash instead, and Relocate reports false. A copy at to
// that was owed to the trash (the same file moved away and back) is the library's again. A Drive
// file that went to the trash a moment ago is ErrInTrash: it has to be kept again (a new one gets
// another ID). A store's key that went to the trash is free again, and was just written anew.
func (s *Store) Relocate(ctx context.Context, from, to string, trashFromAfter time.Duration) (bool, error) {
	if err := lockTrash(ctx, to); err != nil {
		return false, err
	}
	defer trashMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var done int64
	switch err := tx.QueryRowContext(ctx, `SELECT done_at FROM file_trash WHERE file_id = ?`, to).Scan(&done); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, err
	case done > db.Now()-trashMemory.Milliseconds() && !strings.Contains(to, ":"):
		return false, ErrInTrash
	default:
		if _, err := tx.ExecContext(ctx, `DELETE FROM file_trash WHERE file_id = ?`, to); err != nil {
			return false, err
		}
	}
	var moved int64
	for _, q := range []string{
		`UPDATE assets SET file_ref = ?1 WHERE file_ref = ?2 AND state = '` + AssetVerified + `'`,
		`UPDATE covers SET file_ref = ?1 WHERE file_ref = ?2`,
		`UPDATE sidecars SET file_ref = ?1 WHERE file_ref = ?2`,
		`UPDATE album_images SET file_ref = ?1 WHERE file_ref = ?2`,
	} {
		r, err := tx.ExecContext(ctx, q, to, from)
		if err != nil {
			return false, err
		}
		n, _ := r.RowsAffected()
		moved += n
	}
	owe, at := from, db.Now()+trashFromAfter.Milliseconds()
	if moved == 0 {
		if used, err := refUsedTx(ctx, tx, to); err != nil || used {
			return false, err
		}
		owe, at = to, 0
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO file_trash (file_id, created_at, next_at) VALUES (?, ?, ?)
		ON CONFLICT (file_id) DO UPDATE SET created_at = excluded.created_at, next_at = excluded.next_at, done_at = 0, tries = 0, last_error = ''`,
		owe, db.Now(), at); err != nil {
		return false, err
	}
	return moved > 0, tx.Commit()
}
