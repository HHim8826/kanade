package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/HHim8826/kanade/server/internal/media"
	"github.com/HHim8826/kanade/server/internal/storage"
)

const maxCoverFile = 16 << 20

var (
	coverName = regexp.MustCompile(`(?i)^(cover|folder|front|jacket)$`)
	coverLike = regexp.MustCompile(`(?i)(cover|front|jacket)|[_-]0*1$`)
	scansDir  = regexp.MustCompile(`(?i)^(scans?|bk|booklet|artwork|covers?)$`)
	imageExt  = map[string]bool{".jpg": true, ".jpeg": true, ".png": true}
)

// coverFor picks album art in the order decided in D2: embedded picture, then a
// cover/folder/front/jacket image next to the album, then the scans folder. The art of a folder is
// kept for the rest of the batch (review #150: a folder can be used again by a later one), unless
// storing it failed: the next song tries again.
//
// inPlace, for a source's song (#99), is the reference in the source of a picture found beside it
// (and the picture as a scan sees it): a source's pictures are the library's covers where they are,
// as its songs are.
func (im *Importer) coverFor(ctx context.Context, batchID int64, path string, info *media.Info, inPlace func(string) (string, storage.Object)) int64 {
	if info.Cover != nil {
		if id, _ := im.storeCover(ctx, info.Cover.Data); id != 0 {
			return id
		}
	}
	dir := filepath.Dir(path)
	if id, seen := im.coverSeen(batchID, dir); seen {
		return id
	}
	var id int64
	if p := findCoverFile(dir); p != "" {
		ref, seen := "", storage.Object{}
		if inPlace != nil {
			ref, seen = inPlace(p) // before it is read: changed meanwhile, the next scan reads it again
		}
		f, err := os.Open(p)
		if err != nil {
			return 0
		}
		data, err := io.ReadAll(io.LimitReader(f, maxCoverFile+1))
		f.Close()
		if err != nil {
			return 0
		}
		if len(data) <= maxCoverFile {
			if ref != "" {
				id, err = im.placeCover(ctx, data, ref, seen)
			} else {
				id, err = im.storeCover(ctx, data)
			}
			if err != nil {
				return 0
			}
		}
	}
	im.coverFound(batchID, dir, id)
	return id
}

// coverKey is a folder's art in a batch.
type coverKey struct {
	batch  int64
	folder string // a local path, or "drive:" and the Drive folder ID
}

func (im *Importer) coverSeen(batchID int64, folder string) (int64, bool) {
	im.mu.Lock()
	defer im.mu.Unlock()
	id, ok := im.covers[coverKey{batchID, folder}]
	return id, ok
}

func (im *Importer) coverFound(batchID int64, folder string, id int64) {
	im.mu.Lock()
	defer im.mu.Unlock()
	im.covers[coverKey{batchID, folder}] = id
}

// forgetCovers drops a finished batch's folder art.
func (im *Importer) forgetCovers(batchID int64) {
	im.mu.Lock()
	defer im.mu.Unlock()
	for k := range im.covers {
		if k.batch == batchID {
			delete(im.covers, k)
		}
	}
}

func images(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.Type().IsRegular() && imageExt[strings.ToLower(filepath.Ext(e.Name()))] {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

func stem(p string) string { return strings.TrimSuffix(filepath.Base(p), filepath.Ext(p)) }

func findCoverFile(dir string) string {
	root := dir
	if discDir.MatchString(filepath.Base(dir)) {
		root = filepath.Dir(dir) // Disc 1/, CD2/ ... belong to the album folder above
	}
	for _, d := range []string{dir, root} {
		for _, p := range images(d) {
			if coverName.MatchString(stem(p)) {
				return p
			}
		}
	}
	for _, p := range images(root) { // catalog-number scans like VTCL-60116_01.jpg in the album folder
		if coverLike.MatchString(stem(p)) {
			return p
		}
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if !e.IsDir() || !scansDir.MatchString(e.Name()) {
			continue
		}
		list := images(filepath.Join(root, e.Name()))
		for _, p := range list {
			if coverLike.MatchString(stem(p)) {
				return p
			}
		}
		if len(list) > 0 {
			return list[0]
		}
	}
	return ""
}

// storeCover uploads an image once per content hash and returns its cover ID: 0 for what is no
// JPEG or PNG, and an error when it could not be stored (a missing cover never fails an import).
func (im *Importer) storeCover(ctx context.Context, data []byte) (int64, error) {
	mime := http.DetectContentType(data)
	ext := map[string]string{"image/jpeg": ".jpg", "image/png": ".png"}[mime]
	if ext == "" {
		return 0, nil
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	if id, driveID, err := im.lib.CoverBySHA(ctx, sha); err == nil && id != 0 && driveID != "" {
		return id, nil
	}
	id, err := im.uploadCover(ctx, data, sha, mime, ext)
	if err != nil {
		im.log.Warn("cover upload failed", "err", err)
	}
	return id, err
}

// placeCover takes a picture in a source as a cover, where it is, seen as it was before it was read;
// the source's scans follow it from now (review #221). The library having the picture already, its
// cover is read from here when it was from elsewhere in this source (both are followed alike; this
// one was read just now), or from a place it cannot be read from (review #221).
func (im *Importer) placeCover(ctx context.Context, data []byte, ref string, seen storage.Object) (int64, error) {
	mime := http.DetectContentType(data)
	if mime != "image/jpeg" && mime != "image/png" {
		return 0, nil
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	id, have, err := im.lib.CoverBySHA(ctx, sha)
	if err != nil {
		return 0, err
	}
	if id == 0 || have == "" || im.coverMoves(ctx, have, ref) {
		if id, err = im.lib.AddCover(ctx, sha, mime, ref); err != nil {
			return 0, err
		}
	}
	if seen.Key != "" {
		im.keepSourceCover(ctx, ref, seen, sha)
	}
	return id, nil
}

// coverMoves says whether a cover read from have is to be read from ref, a picture of a source with
// the same bytes: have is in the same source, or cannot be read.
func (im *Importer) coverMoves(ctx context.Context, have, ref string) bool {
	_, from, _, ok := storage.Parse(have)
	if !ok {
		return false // in Google Drive: Kanade's own copy
	}
	if _, to, _, _ := storage.Parse(ref); from == to {
		return true
	}
	files, ok := im.files()
	if !ok {
		return false
	}
	_, err := files.Stat(ctx, have)
	return err != nil
}

// keepSourceCover records a source's picture taken as a cover (source_covers), for its scans to follow.
func (im *Importer) keepSourceCover(ctx context.Context, ref string, o storage.Object, sha string) {
	_, store, _, ok := storage.Parse(ref)
	if !ok {
		return
	}
	if _, err := im.db.ExecContext(ctx, `INSERT INTO source_covers (store_id, key, path, size, version, sha256) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (store_id, key) DO UPDATE SET path = excluded.path, size = excluded.size, version = excluded.version, sha256 = excluded.sha256`,
		store, o.Key, o.Path, o.Size, o.Version, sha); err != nil {
		im.log.Warn("record a source's cover", "ref", ref, "err", err)
	}
}

func (im *Importer) uploadCover(ctx context.Context, data []byte, sha, mime, ext string) (int64, error) {
	tmp := filepath.Join(im.staging, "cover-"+sha+ext)
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return 0, err
	}
	defer os.Remove(tmp)
	ref, err := im.put(ctx, 0, storage.PutFile{Path: tmp, Dir: "covers", Name: sha + ext, MIME: mime,
		Size: int64(len(data)), SHA256: sha, Sessions: &memSessions{}})
	if err != nil {
		return 0, err
	}
	return im.lib.AddCover(ctx, sha, mime, ref)
}

// memSessions keeps a session only for the life of one small upload.
type memSessions struct{ uri string }

func (m *memSessions) LoadSession(context.Context) (string, error)     { return m.uri, nil }
func (m *memSessions) SaveSession(_ context.Context, uri string) error { m.uri = uri; return nil }
func (m *memSessions) DeleteSession(context.Context) error             { m.uri = ""; return nil }
