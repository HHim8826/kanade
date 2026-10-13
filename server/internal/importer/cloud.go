package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/media"
	"github.com/HHim8826/kanade/server/internal/storage"
)

// Cloud sources (#95, #100): a OneDrive's or a bucket's files are not on the server. A scan queues
// them without a local path; analysis reads their tags with ranged reads, as the Drive inbox's;
// and each is fetched to staging when its turn comes, with the pictures and the .lrc beside it,
// checked against what the store says of it (its size, OneDrive's quickXorHash) and against
// changing while it was fetched, and hashed there: the SHA-256 is always Kanade's own. The library
// then reads the file where it is; the copy goes once the import is recorded.

// Files reads storage locations' files; *storage.Set is one.
type Files interface {
	Open(ctx context.Context, ref string, start, end int64) (io.ReadCloser, error)
	OpenRange(ctx context.Context, ref string, start, end int64) (*http.Response, error)
	Stat(ctx context.Context, ref string) (storage.Object, error)
}

// fetchInfo is what a scan saw of a cloud source's file (import_items.fetch).
type fetchInfo struct {
	Version string  `json:"version"`
	Hash    string  `json:"hash,omitempty"`
	With    []extra `json:"with,omitempty"` // the pictures in its folder (and the album folder above a disc's), its .lrc
}

type extra struct {
	Key     string `json:"key"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Hash    string `json:"hash,omitempty"`
	Version string `json:"version,omitempty"` // a picture taken as a cover is recorded as the scan saw it (review #221)
}

// maxExtras bounds the pictures fetched with a song.
const maxExtras = 12

// maxLrcFile is the largest .lrc kept as a song's lyrics (larger ones are not lyrics).
const maxLrcFile = 256 << 10

// besides collects the files of a cloud source that are fetched with its songs: pictures and .lrc.
type besides map[string][]storage.Object // by folder

func (b besides) add(o storage.Object) {
	ext := strings.ToLower(path.Ext(o.Path))
	if (imageExt[ext] && o.Size <= maxCoverFile) || (ext == ".lrc" && o.Size <= maxLrcFile) {
		dir := path.Dir(o.Path)
		b[dir] = append(b[dir], o)
	}
}

// fetchFor is the fetch record of o.
func (b besides) fetchFor(o storage.Object) string {
	fi := fetchInfo{Version: o.Version, Hash: o.Hash}
	dir := path.Dir(o.Path)
	lrc := strings.ToLower(strings.TrimSuffix(path.Base(o.Path), path.Ext(o.Path)) + ".lrc")
	pick := func(dir string, lyrics bool) {
		for _, x := range b[dir] {
			isLrc := strings.EqualFold(path.Ext(x.Path), ".lrc")
			if isLrc && (!lyrics || strings.ToLower(path.Base(x.Path)) != lrc) {
				continue
			}
			if !isLrc && len(fi.With) >= maxExtras {
				continue
			}
			fi.With = append(fi.With, extra{Key: x.Key, Path: x.Path, Size: x.Size, Hash: x.Hash, Version: x.Version})
		}
	}
	pick(dir, true)
	if discDir.MatchString(path.Base(dir)) && dir != "." {
		pick(path.Dir(dir), false)
	}
	raw, _ := json.Marshal(fi)
	return string(raw)
}

// workPath is where a cloud source's file at rel is fetched: the source's folders again, below the
// batch's work folder, so a song finds its pictures and lyrics as it would in a folder.
func (im *Importer) workPath(batchID int64, rel string) string {
	p := filepath.FromSlash(rel)
	if !filepath.IsLocal(p) {
		p = filepath.Base(p)
	}
	return filepath.Join(im.workDir(batchID), "source", p)
}

func (im *Importer) files() (Files, bool) {
	f, ok := im.Stores.(Files)
	return f, ok
}

// fetchRef fetches a cloud source's file to staging and records the copy as the item's local file.
func (im *Importer) fetchRef(ctx context.Context, batchID, itemID int64, ref, rel string, size int64, raw string) (string, error) {
	files, ok := im.files()
	if !ok {
		return "", errors.New("storage locations are not available")
	}
	var fi fetchInfo
	json.Unmarshal([]byte(raw), &fi)
	o, err := files.Stat(ctx, ref)
	if err != nil {
		return "", err
	}
	if fi.Version != "" && (o.Version != fi.Version || o.Size != size) {
		im.log.Info("source: file changed since the scan; fetching it as it is now", "ref", ref)
	}
	release, err := im.hold(ctx, o.Size)
	if err != nil {
		return "", fmt.Errorf("not enough staging space to fetch it: %w", err)
	}
	defer release()
	dst := im.workPath(batchID, rel)
	if err := fetchObject(ctx, files, ref, o, dst); err != nil {
		return "", err
	}
	release() // on disk now, where WorkCommitted counts it
	if again, err := files.Stat(ctx, ref); err != nil {
		os.Remove(dst)
		return "", err
	} else if again.Version != o.Version || again.Size != o.Size {
		os.Remove(dst)
		return "", errors.New("the file changed while it was being fetched; it is imported again on the next scan")
	}
	kind, store, _, _ := storage.Parse(ref)
	for _, x := range fi.With {
		p := im.workPath(batchID, x.Path)
		if st, err := os.Stat(p); err == nil && st.Size() == x.Size {
			continue // fetched with another song of the folder
		}
		if strings.EqualFold(path.Ext(x.Path), ".lrc") && x.Size > maxLrcFile {
			continue // recorded before the limit
		}
		// Each picture and the lyrics hold their own staging space, until they are on disk where
		// WorkCommitted counts them; without room they are left out, the song goes on (review #217).
		release, err := im.hold(ctx, x.Size)
		if err != nil {
			im.log.Info("source: no staging space for a picture or lyrics; left out", "file", x.Path, "err", err)
			continue
		}
		xo := storage.Object{Key: x.Key, Path: x.Path, Size: x.Size, Hash: x.Hash}
		err = fetchObject(ctx, files, storage.Ref(kind, store, x.Key), xo, p)
		release()
		if err != nil && ctx.Err() == nil {
			im.log.Warn("source: fetch a picture or lyrics", "file", x.Path, "err", err)
		}
	}
	_, err = im.db.ExecContext(ctx, `UPDATE import_items SET local_path = ?, temp = 1, updated_at = ? WHERE id = ?`, dst, db.Now(), itemID)
	return dst, err
}

// fetchObject copies o to dst, checked against its size and the store's own checksum.
func fetchObject(ctx context.Context, files Files, ref string, o storage.Object, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	rc, err := files.Open(ctx, ref, 0, -1)
	if err != nil {
		return err
	}
	defer rc.Close()
	part := dst + ".part"
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	check := storage.NewChecker(o)
	n, err := io.Copy(check.Writer(f), io.LimitReader(rc, o.Size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
	case n != o.Size:
		err = fmt.Errorf("fetched %d of %d bytes", n, o.Size)
	case !check.Matches():
		err = errors.New("the fetched copy does not match the storage location's checksum of it")
	}
	if err != nil {
		os.Remove(part)
		return err
	}
	return os.Rename(part, dst)
}

// probeRef reads a cloud source's file's tags in place, with ranged reads.
func (im *Importer) probeRef(ctx context.Context, ref string, size int64) (*media.Info, error) {
	files, ok := im.files()
	if !ok {
		return nil, errors.New("storage locations are not available")
	}
	return media.Probe(gdrive.NewReaderAt(ctx, files, ref, size), size)
}

// fetchSourceLocal fetches the batch's cloud sidecars and archives: they are small, or have to be
// unpacked.
func (im *Importer) fetchSourceLocal(ctx context.Context, batchID int64) error {
	rows, err := im.db.QueryContext(ctx, `SELECT id, ref, rel_path, drive_size, fetch FROM import_items
		WHERE batch_id = ? AND local_path = '' AND ref != '' AND role IN (?, ?) AND state = 'pending'`, batchID, RoleSidecar, RoleZip)
	if err != nil {
		return err
	}
	type item struct {
		id              int64
		ref, rel, fetch string
		size            int64
	}
	var list []item
	for rows.Next() {
		var it item
		if rows.Scan(&it.id, &it.ref, &it.rel, &it.size, &it.fetch) == nil {
			list = append(list, it)
		}
	}
	rows.Close()
	for _, it := range list {
		if it.size > maxSidecar && roleOf(it.rel) == RoleSidecar {
			im.itemFailed(ctx, it.id, StateSkipped, "larger than 16 MB: not a CUE sheet or rip log")
			continue
		}
		if _, err := im.fetchRef(ctx, batchID, it.id, it.ref, it.rel, it.size, it.fetch); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			im.itemFailed(ctx, it.id, StateFailed, "cannot fetch it: "+err.Error())
		}
	}
	return nil
}

// sourceFile is what maps a file beside a source's song (a picture in its folder, or fetched with
// it) to its reference in the source, with the file as it was before it is read (as a scan sees
// it); nil for a song from elsewhere.
func (im *Importer) sourceFile(it *item) func(string) (string, storage.Object) {
	kind, store, key, ok := storage.Parse(it.ref)
	if !ok || it.sourcePath != "" {
		return nil
	}
	if !it.temp { // in a folder: its root is the song's path without its key
		root := strings.TrimSuffix(it.path, filepath.FromSlash(key))
		return func(p string) (string, storage.Object) {
			rel, err := filepath.Rel(root, p)
			info, serr := os.Stat(p)
			if err != nil || !filepath.IsLocal(rel) || serr != nil {
				return "", storage.Object{}
			}
			k := filepath.ToSlash(rel)
			return storage.Ref(kind, store, k), storage.Object{Key: k, Path: k, Size: info.Size(), Version: storage.LocalVersion(info)}
		}
	}
	var fi fetchInfo
	json.Unmarshal([]byte(it.fetch), &fi)
	return func(p string) (string, storage.Object) {
		for _, x := range fi.With {
			if im.workPath(it.batchID, x.Path) == p {
				return storage.Ref(kind, store, x.Key), storage.Object{Key: x.Key, Path: x.Path, Size: x.Size, Version: x.Version}
			}
		}
		return "", storage.Object{}
	}
}
