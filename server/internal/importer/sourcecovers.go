package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"path"

	"github.com/HHim8826/kanade/server/internal/storage"
)

// A source's pictures taken as covers where they are (review #221) are followed by its scans as its
// songs are (source_covers): one moved is found by its content and its cover read from there; other
// bytes at its place are a cover of their own for the albums of its folder (a cover's content never
// changes, so nothing made of the old one, its sizes or a browser's copy, passes for the new); one
// gone has its cover read from another copy, or its albums have none.

// coverRow is a source's picture taken as a cover as the last scan saw it; size -1 for one no scan
// recorded (taken before the scans followed them): its cover is the library's.
type coverRow struct {
	key, path, version, sha string
	size                    int64
}

// picture is what a scan read of a picture.
type picture struct {
	sha, mime string
}

func readPicture(ctx context.Context, b storage.Backend, o storage.Object) (picture, error) {
	rc, err := b.Open(ctx, o.Key, 0, -1)
	if err != nil {
		return picture{}, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxCoverFile+1))
	if err != nil {
		return picture{}, err
	}
	if int64(len(data)) != o.Size {
		return picture{}, io.ErrUnexpectedEOF
	}
	sum := sha256.Sum256(data)
	return picture{hex.EncodeToString(sum[:]), http.DetectContentType(data)}, nil
}

// coverDir is the folder whose songs a picture at p is the cover of ("" for the whole source): its
// own, or the album folder above a scans folder.
func coverDir(p string) string {
	dir := path.Dir(p)
	if dir != "." && scansDir.MatchString(path.Base(dir)) {
		dir = path.Dir(dir)
	}
	if dir == "." {
		return ""
	}
	return dir
}

// followCovers follows source st's pictures taken as covers, with pictures, those the scan found
// there (by key). held: the source looks not all there, so none is taken for gone. It returns how
// many it followed: moved, other bytes, or gone.
func (im *Importer) followCovers(ctx context.Context, b storage.Backend, st *storage.Store, pictures map[string]storage.Object,
	partial *storage.PartialError, held bool) (int, error) {
	rows := map[string]*coverRow{}
	q, err := im.db.QueryContext(ctx, `SELECT key, path, size, version, sha256 FROM source_covers WHERE store_id = ?`, st.ID)
	if err != nil {
		return 0, err
	}
	for q.Next() {
		var r coverRow
		if err := q.Scan(&r.key, &r.path, &r.size, &r.version, &r.sha); err != nil {
			q.Close()
			return 0, err
		}
		rows[r.key] = &r
	}
	q.Close()
	if err := q.Err(); err != nil {
		return 0, err
	}
	covers, err := im.lib.CoversIn(ctx, st.Kind, st.ID)
	if err != nil {
		return 0, err
	}
	coverAt := map[string]int64{} // key -> the cover read from there
	for _, c := range covers {
		_, _, key, ok := storage.Parse(c.Ref)
		if !ok {
			continue
		}
		coverAt[key] = c.ID
		if rows[key] == nil {
			rows[key] = &coverRow{key: key, path: key, sha: c.SHA256, size: -1}
			if o, ok := pictures[key]; ok {
				rows[key].path = o.Path
			}
		}
	}
	if len(rows) == 0 {
		return 0, nil
	}
	ref := func(key string) string { return storage.Ref(st.Kind, st.ID, key) }
	read := map[string]picture{}
	readOnce := func(o storage.Object) (picture, bool) {
		if p, ok := read[o.Key]; ok {
			return p, p.sha != ""
		}
		p, err := readPicture(ctx, b, o)
		if err != nil {
			im.log.Warn("source scan: read a picture", "file", o.Path, "err", err)
		}
		read[o.Key] = p
		return p, err == nil
	}
	keep := func(o storage.Object, sha string) error {
		_, err := im.db.ExecContext(ctx, `INSERT INTO source_covers (store_id, key, path, size, version, sha256) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (store_id, key) DO UPDATE SET path = excluded.path, size = excluded.size, version = excluded.version,
			sha256 = excluded.sha256`, st.ID, o.Key, o.Path, o.Size, o.Version, sha)
		rows[o.Key] = &coverRow{key: o.Key, path: o.Path, size: o.Size, version: o.Version, sha: sha}
		return err
	}
	drop := func(key string) error {
		delete(rows, key)
		_, err := im.db.ExecContext(ctx, `DELETE FROM source_covers WHERE store_id = ? AND key = ?`, st.ID, key)
		return err
	}
	there := map[string]bool{} // keys whose pictures are there as recorded now
	// copyOf is another picture there with the bytes sha.
	copyOf := func(sha, but string) string {
		for key, r := range rows {
			if key != but && there[key] && r.sha == sha {
				return key
			}
		}
		return ""
	}

	type change struct {
		r   *coverRow
		o   storage.Object
		now picture
	}
	var unseen []*coverRow
	var changed []change
	for key, r := range rows {
		o, ok := pictures[key]
		switch {
		case !ok:
			if partial == nil || !partial.Covers(r.path) {
				unseen = append(unseen, r)
			}
		case r.size == o.Size && r.version == o.Version:
			there[key] = true
			if r.path != o.Path { // renamed or moved in a OneDrive, whose keys stay
				if err := keep(o, r.sha); err != nil {
					return 0, err
				}
			}
		default:
			p, ok := readOnce(o)
			if !ok {
				continue // as it was until a scan reads it
			}
			if p.sha == r.sha { // touched, the same picture
				there[key] = true
				if err := keep(o, r.sha); err != nil {
					return 0, err
				}
				continue
			}
			changed = append(changed, change{r, o, p})
		}
	}

	// Unseen: moved, to a picture not recorded that has its bytes (of its size; by its name for one
	// whose size was never recorded), or gone.
	bySize, byName := map[int64][]string{}, map[string][]string{}
	for key, o := range pictures {
		if rows[key] == nil {
			bySize[o.Size] = append(bySize[o.Size], key)
			byName[path.Base(o.Path)] = append(byName[path.Base(o.Path)], key)
		}
	}
	n := 0
	taken := map[string]bool{}
	var gone []*coverRow
	for _, r := range unseen {
		list := bySize[r.size]
		if r.size < 0 {
			list = byName[path.Base(r.path)]
		}
		to := ""
		for _, key := range list {
			if taken[key] {
				continue
			}
			if p, ok := readOnce(pictures[key]); ok && p.sha == r.sha {
				to = key
				break
			}
		}
		if to == "" {
			gone = append(gone, r)
			continue
		}
		taken[to], there[to] = true, true
		if id := coverAt[r.key]; id != 0 {
			if err := im.lib.MoveCover(ctx, id, ref(to)); err != nil {
				return 0, err
			}
			delete(coverAt, r.key)
			coverAt[to] = id
		}
		if err := drop(r.key); err != nil {
			return 0, err
		}
		if err := keep(pictures[to], r.sha); err != nil {
			return 0, err
		}
		n++
	}

	// lose lets go of the cover read from key, whose picture is not there any more: read from another
	// copy, or gone from its albums.
	lose := func(key, sha string) error {
		id := coverAt[key]
		if id == 0 {
			return nil
		}
		delete(coverAt, key)
		if c := copyOf(sha, key); c != "" {
			coverAt[c] = id
			return im.lib.MoveCover(ctx, id, ref(c))
		}
		return im.lib.LoseCover(ctx, id)
	}
	// Other bytes: a cover of their own for the albums of the folder, which had the old one.
	for _, c := range changed {
		old, _, err := im.lib.CoverBySHA(ctx, c.r.sha)
		if err != nil {
			return 0, err
		}
		oldAt := coverAt[c.r.key]
		delete(coverAt, c.r.key) // not the new cover's place: it is looked for below, or lost
		if oldAt != 0 {
			if other := copyOf(c.r.sha, c.r.key); other != "" {
				if err := im.lib.MoveCover(ctx, oldAt, ref(other)); err != nil {
					return 0, err
				}
				coverAt[other], oldAt = oldAt, 0
			}
		}
		var now int64
		if c.now.mime == "image/jpeg" || c.now.mime == "image/png" {
			id, have, err := im.lib.CoverBySHA(ctx, c.now.sha)
			if err != nil {
				return 0, err
			}
			if id == 0 || have == "" || im.coverMoves(ctx, have, ref(c.o.Key)) {
				if id, err = im.lib.AddCover(ctx, c.now.sha, c.now.mime, ref(c.o.Key)); err != nil {
					return 0, err
				}
				coverAt[c.o.Key] = id
			}
			now = id
			if old != 0 {
				if _, err := im.lib.FolderCover(ctx, st.Kind, st.ID, coverDir(c.o.Path), old, now); err != nil {
					return 0, err
				}
			}
			there[c.o.Key] = true
			if err := keep(c.o, c.now.sha); err != nil {
				return 0, err
			}
		} else if err := drop(c.r.key); err != nil {
			return 0, err
		}
		if oldAt != 0 {
			if err := im.lib.LoseCover(ctx, oldAt); err != nil {
				return 0, err
			}
		}
		n++
	}
	if held {
		return n, nil
	}
	for _, r := range gone {
		if err := drop(r.key); err != nil {
			return 0, err
		}
		if coverAt[r.key] != 0 {
			n++
		}
		if err := lose(r.key, r.sha); err != nil {
			return 0, err
		}
	}
	return n, nil
}
