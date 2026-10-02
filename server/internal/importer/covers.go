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
// cover/folder/front/jacket image next to the album, then the scans folder.
func (im *Importer) coverFor(ctx context.Context, path string, info *media.Info) int64 {
	if info.Cover != nil {
		if id := im.storeCover(ctx, info.Cover.Data); id != 0 {
			return id
		}
	}
	dir := filepath.Dir(path)
	im.mu.Lock()
	id, seen := im.covers[dir]
	im.mu.Unlock()
	if seen {
		return id
	}
	if p := findCoverFile(dir); p != "" {
		if f, err := os.Open(p); err == nil {
			data, err := io.ReadAll(io.LimitReader(f, maxCoverFile+1))
			f.Close()
			if err == nil && len(data) <= maxCoverFile {
				id = im.storeCover(ctx, data)
			}
		}
	}
	im.mu.Lock()
	im.covers[dir] = id
	im.mu.Unlock()
	return id
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

// storeCover uploads an image once per content hash and returns its cover ID (0 on failure;
// a missing cover never fails an import).
func (im *Importer) storeCover(ctx context.Context, data []byte) int64 {
	mime := http.DetectContentType(data)
	ext := map[string]string{"image/jpeg": ".jpg", "image/png": ".png"}[mime]
	if ext == "" {
		return 0
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	if id, driveID, err := im.lib.CoverBySHA(ctx, sha); err == nil && id != 0 && driveID != "" {
		return id
	}
	tmp := filepath.Join(im.staging, "cover-"+sha+ext)
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		im.log.Warn("cover", "err", err)
		return 0
	}
	defer os.Remove(tmp)
	folder, err := im.drive.Folder(ctx, "covers")
	if err == nil {
		var f gdrive.File
		f, err = im.drive.Upload(ctx, gdrive.Upload{Path: tmp, Name: sha + ext, ParentID: folder, MIME: mime,
			Size: int64(len(data)), SHA256: sha, Sessions: &memSessions{}})
		if err == nil {
			id, err := im.lib.AddCover(ctx, sha, mime, f.ID)
			if err == nil {
				return id
			}
		}
	}
	im.log.Warn("cover upload failed", "err", err)
	return 0
}

// memSessions keeps a session only for the life of one small upload.
type memSessions struct{ uri string }

func (m *memSessions) LoadSession(context.Context) (string, error)     { return m.uri, nil }
func (m *memSessions) SaveSession(_ context.Context, uri string) error { m.uri = uri; return nil }
func (m *memSessions) DeleteSession(context.Context) error             { m.uri = ""; return nil }
