package library

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"path"
	"slices"

	"github.com/HHim8826/kanade/server/internal/db"
)

// AlbumImage is a picture in an album's gallery (#134): a scan of its cover, back, booklet, disc or
// obi, kept as it came.
type AlbumImage struct {
	ID      int64  `json:"id"`
	AlbumID int64  `json:"album_id,omitempty"`
	Path    string `json:"path"` // where it was in the album's folder
	Name    string `json:"name"` // its file name
	Kind    string `json:"kind"` // see ImageKinds; "" not said
	Disc    int    `json:"disc"` // the disc it goes with; 0 the album as a whole
	SHA256  string `json:"-"`
	Size    int64  `json:"size"`
	MIME    string `json:"mime"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	Ref     string `json:"-"`

	position int
}

// ImageKinds are what a picture can be said to be.
var ImageKinds = []string{"front", "back", "booklet", "disc", "obi", "other"}

const imageCols = `id, coalesce(album_id, 0), path, kind, disc, position, sha256, size, mime, width, height, file_ref`

func scanImage(sc interface{ Scan(...any) error }) (AlbumImage, error) {
	var m AlbumImage
	err := sc.Scan(&m.ID, &m.AlbumID, &m.Path, &m.Kind, &m.Disc, &m.position, &m.SHA256, &m.Size, &m.MIME, &m.Width, &m.Height, &m.Ref)
	m.Name = path.Base(m.Path)
	return m, err
}

// FindImage returns the picture with this content on this album (or one merged into it), taken out
// of the gallery or not, if it was kept before.
func (s *Store) FindImage(ctx context.Context, albumID int64, sha string, size int64) (*AlbumImage, error) {
	m, err := scanImage(s.db.QueryRowContext(ctx, mergedFrom+` SELECT `+imageCols+` FROM album_images
		WHERE sha256 = ?2 AND size = ?3 AND album_id IN (SELECT id FROM family) ORDER BY id LIMIT 1`, albumID, sha, size))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &m, err
}

// AddImage keeps a picture with an album.
func (s *Store) AddImage(ctx context.Context, m AlbumImage) (int64, error) {
	r, err := s.db.ExecContext(ctx, `INSERT INTO album_images (album_id, path, kind, disc, sha256, size, mime, width, height, file_ref, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, nullID(m.AlbumID), m.Path, m.Kind, m.Disc, m.SHA256, m.Size, m.MIME, m.Width, m.Height,
		m.Ref, db.Now())
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// Image is one picture, and whether it was taken out of its gallery.
func (s *Store) Image(ctx context.Context, id int64) (*AlbumImage, bool, error) {
	var m AlbumImage
	var hidden bool
	err := s.db.QueryRowContext(ctx, `SELECT `+imageCols+`, hidden FROM album_images WHERE id = ?`, id).Scan(&m.ID, &m.AlbumID, &m.Path,
		&m.Kind, &m.Disc, &m.position, &m.SHA256, &m.Size, &m.MIME, &m.Width, &m.Height, &m.Ref, &hidden)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	m.Name = path.Base(m.Path)
	return &m, hidden, err
}

// AlbumImages is an album's gallery, with the albums merged into it (the same picture kept twice is
// there once), in order: the album's own pictures, then each disc's; within them as arranged, the
// rest by their paths in natural order.
func (s *Store) AlbumImages(ctx context.Context, albumID int64) ([]AlbumImage, error) {
	rows, err := s.db.QueryContext(ctx, mergedFrom+` SELECT `+imageCols+` FROM album_images
		WHERE id IN (SELECT min(id) FROM album_images WHERE album_id IN (SELECT id FROM family) AND hidden = 0 GROUP BY sha256, size)`, albumID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AlbumImage{}
	for rows.Next() {
		m, err := scanImage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortImages(out)
	return out, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func sortImages(list []AlbumImage) {
	slices.SortStableFunc(list, func(a, b AlbumImage) int {
		return cmp.Or(cmp.Compare(a.Disc, b.Disc), cmp.Compare(boolInt(a.position == 0), boolInt(b.position == 0)), cmp.Compare(a.position, b.position),
			NaturalCompare(a.Path, b.Path), cmp.Compare(a.ID, b.ID))
	})
}

// albumImageCount counts an album's gallery as AlbumImages lists it.
func (s *Store) albumImageCount(ctx context.Context, albumID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, mergedFrom+` SELECT count(DISTINCT sha256 || '/' || size) FROM album_images
		WHERE album_id IN (SELECT id FROM family) AND hidden = 0`, albumID).Scan(&n)
	return n, err
}

// ImagePlace is where a picture goes in its gallery: what it is and the disc it goes with.
type ImagePlace struct {
	ID   int64  `json:"id"`
	Kind string `json:"kind"`
	Disc int    `json:"disc"`
}

// ArrangeImages sets an album's gallery: every picture in it, in the order given, each with what it
// is and its disc. A picture missing or one of another album is refused, so that a gallery
// changed meanwhile (an import, another tab) is not arranged by an old list.
func (s *Store) ArrangeImages(ctx context.Context, albumID int64, list []ImagePlace) error {
	have, err := s.AlbumImages(ctx, albumID)
	if err != nil {
		return err
	}
	if len(list) != len(have) {
		return ErrInvalid
	}
	seen := map[int64]bool{}
	for _, p := range list {
		if seen[p.ID] || !slices.ContainsFunc(have, func(m AlbumImage) bool { return m.ID == p.ID }) ||
			(p.Kind != "" && !slices.Contains(ImageKinds, p.Kind)) || p.Disc < 0 || p.Disc > 99 {
			return ErrInvalid
		}
		seen[p.ID] = true
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, p := range list {
		if _, err := tx.ExecContext(ctx, `UPDATE album_images SET kind = ?, disc = ?, position = ? WHERE id = ?`, p.Kind, p.Disc, i+1, p.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// HideImage takes a picture out of its gallery, and the copies of it kept with the albums merged
// into it alike (the gallery lists them as one). A file in a managed store goes to its trash once
// nothing else uses it; one in a source stays where it is, and the picture stays taken out should
// a scan see it again. It reports the album whose gallery it was in (0: none).
func (s *Store) HideImage(ctx context.Context, id int64) (int64, error) {
	m, hidden, err := s.Image(ctx, id)
	if err != nil || m == nil || hidden {
		return 0, err
	}
	album := m.AlbumID
	if album != 0 {
		if album, _, err = s.AlbumNow(ctx, album); err != nil {
			return 0, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var refs []string
	if album != 0 {
		rows, err := tx.QueryContext(ctx, mergedFrom+` SELECT DISTINCT file_ref FROM album_images
			WHERE album_id IN (SELECT id FROM family) AND sha256 = ?2 AND size = ?3 AND hidden = 0`, album, m.SHA256, m.Size)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var ref string
			if err := rows.Scan(&ref); err != nil {
				rows.Close()
				return 0, err
			}
			refs = append(refs, ref)
		}
		rows.Close()
		if _, err := tx.ExecContext(ctx, mergedFrom+` UPDATE album_images SET hidden = 1
			WHERE album_id IN (SELECT id FROM family) AND sha256 = ?2 AND size = ?3`, album, m.SHA256, m.Size); err != nil {
			return 0, err
		}
	} else {
		refs = []string{m.Ref}
		if _, err := tx.ExecContext(ctx, `UPDATE album_images SET hidden = 1 WHERE id = ?`, id); err != nil {
			return 0, err
		}
	}
	for _, ref := range refs {
		if err := trashUnusedTx(ctx, tx, ref); err != nil {
			return 0, err
		}
	}
	return album, tx.Commit()
}

// trashUnusedTx owes a managed store's file to its trash, in tx, when nothing in the library uses
// it any more: a song, a cover, a sidecar, a picture in a gallery. A source's file is never touched.
func trashUnusedTx(ctx context.Context, tx *sql.Tx, ref string) error {
	if store, ok := refStore(ref); ok {
		var mode string
		switch err := tx.QueryRowContext(ctx, `SELECT mode FROM stores WHERE id = ?`, store).Scan(&mode); {
		case errors.Is(err, sql.ErrNoRows):
			return nil // removed with its store
		case err != nil:
			return err
		case mode != "managed":
			return nil
		}
	}
	if used, err := refUsedTx(ctx, tx, ref); err != nil || used {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO file_trash (file_id, created_at) VALUES (?, ?)
		ON CONFLICT (file_id) DO UPDATE SET created_at = excluded.created_at, done_at = 0, tries = 0, next_at = 0, last_error = ''`,
		ref, db.Now())
	return err
}

// refUsedTx says whether the library uses the file at ref: a song, a cover, a sidecar, a picture in
// a gallery.
func refUsedTx(ctx context.Context, q querier, ref string) (bool, error) {
	var used int
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM assets WHERE file_ref = ?1)
		OR EXISTS (SELECT 1 FROM covers WHERE file_ref = ?1) OR EXISTS (SELECT 1 FROM sidecars WHERE file_ref = ?1)
		OR EXISTS (SELECT 1 FROM album_images WHERE file_ref = ?1 AND hidden = 0)`, ref).Scan(&used)
	return used == 1, err
}

// ShowImage puts a picture taken out of its gallery back, its file at ref now (uploaded again).
func (s *Store) ShowImage(ctx context.Context, id int64, ref string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE album_images SET hidden = 0, file_ref = ? WHERE id = ?`, ref, id)
	return err
}

// ImageCover is the cover a picture of a gallery (a JPEG or PNG) makes: one with its content kept
// already, or the picture itself where it is.
func (s *Store) ImageCover(ctx context.Context, m *AlbumImage) (int64, error) {
	id, ref, err := s.CoverBySHA(ctx, m.SHA256)
	if err != nil || (id != 0 && ref != "") {
		return id, err
	}
	return s.AddCover(ctx, m.SHA256, m.MIME, m.Ref)
}
