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

	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/media"
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
func (im *Importer) coverFor(ctx context.Context, batchID int64, path string, info *media.Info) int64 {
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
			if id, err = im.storeCover(ctx, data); err != nil {
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

func (im *Importer) uploadCover(ctx context.Context, data []byte, sha, mime, ext string) (int64, error) {
	tmp := filepath.Join(im.staging, "cover-"+sha+ext)
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return 0, err
	}
	defer os.Remove(tmp)
	folder, err := im.drive.Folder(ctx, "covers")
	if err != nil {
		return 0, err
	}
	f, err := im.drive.Upload(ctx, gdrive.Upload{Path: tmp, Name: sha + ext, ParentID: folder, MIME: mime,
		Size: int64(len(data)), SHA256: sha, Sessions: &memSessions{}})
	if err != nil {
		return 0, err
	}
	return im.lib.AddCover(ctx, sha, mime, f.ID)
}

// memSessions keeps a session only for the life of one small upload.
type memSessions struct{ uri string }

func (m *memSessions) LoadSession(context.Context) (string, error)     { return m.uri, nil }
func (m *memSessions) SaveSession(_ context.Context, uri string) error { m.uri = uri; return nil }
func (m *memSessions) DeleteSession(context.Context) error             { m.uri = ""; return nil }
