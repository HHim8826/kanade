package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/image/draw"

	"github.com/HHim8826/kanade/server/internal/config"
	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/importer"
	"github.com/HHim8826/kanade/server/internal/library"
)

func pageArgs(r *http.Request) (limit, offset int) {
	limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ = strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return limit, max(offset, 0)
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("bad id")
	}
	return id, nil
}

func (s *Server) albums(w http.ResponseWriter, r *http.Request) {
	limit, offset := pageArgs(r)
	q := library.AlbumQuery{Limit: limit, Offset: offset, Recent: r.URL.Query().Get("sort") == "recent", Search: r.URL.Query().Get("q")}
	switch c := r.URL.Query().Get("category"); c {
	case "":
	case "none": // in no category (review #92)
		q.Category = -1
	default:
		id, err := strconv.ParseInt(c, 10, 64)
		if err != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, errors.New("category is a number or none"))
			return
		}
		q.Category = id
	}
	list, err := s.lib.AlbumsBy(r.Context(), q)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) album(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	a, err := s.lib.Album(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if a == nil {
		writeError(w, http.StatusNotFound, errors.New("no such album"))
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) tracks(w http.ResponseWriter, r *http.Request) {
	limit, offset := pageArgs(r)
	list, err := s.lib.Tracks(r.Context(), limit, offset, r.URL.Query().Get("filter"))
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// randomTracks picks ?n (1 to 50, default 10) songs at random from the whole library, of ?kind
// (music, spoken or all; music by default), leaving out ?not (IDs, comma-separated: songs just
// played).
func (s *Server) randomTracks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	n, _ := strconv.Atoi(q.Get("n"))
	if n <= 0 || n > 50 {
		n = 10
	}
	kind := q.Get("kind")
	switch kind {
	case "", "music":
		kind = "music"
	case "all":
		kind = ""
	case "spoken":
	default:
		writeError(w, http.StatusBadRequest, errors.New("kind: music, spoken or all"))
		return
	}
	var not []int64
	for _, f := range strings.Split(q.Get("not"), ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(f), 10, 64); err == nil && id > 0 && len(not) < 200 {
			not = append(not, id)
		}
	}
	list, err := s.lib.RandomTracks(r.Context(), n, kind, not)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) artists(w http.ResponseWriter, r *http.Request) {
	limit, offset := pageArgs(r)
	list, err := s.lib.Artists(r.Context(), limit, offset)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	res, err := s.lib.Search(r.Context(), r.URL.Query().Get("q"), 50)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---- streaming ----

func (s *Server) streamSig(assetID, exp int64) string {
	m := hmac.New(sha256.New, s.streamKey)
	fmt.Fprintf(m, "%d|%d", assetID, exp)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// streamURL returns a time-limited URL for players that cannot send an Authorization header,
// so the login token itself never appears in a URL.
func (s *Server) streamURL(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	exp := time.Now().Add(12 * time.Hour).Unix()
	u := fmt.Sprintf("%s/api/v1/stream/%d?exp=%d&sig=%s", strings.TrimRight(s.cfg.PublicURL, "/"), id, exp, s.streamSig(id, exp))
	writeJSON(w, http.StatusOK, map[string]any{"url": u, "expires_at": exp})
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if sig := r.URL.Query().Get("sig"); sig != "" {
		exp, _ := strconv.ParseInt(r.URL.Query().Get("exp"), 10, 64)
		if time.Now().Unix() > exp || !hmac.Equal([]byte(sig), []byte(s.streamSig(id, exp))) {
			writeError(w, http.StatusForbidden, errors.New("stream link expired or invalid"))
			return
		}
	} else {
		token, _ := sessionToken(r) // header, or the web client's cookie (an <audio> element sends it)
		if _, err := s.auth.Authenticate(r.Context(), token); err != nil {
			writeError(w, http.StatusUnauthorized, err)
			return
		}
	}
	a, err := s.lib.StreamTarget(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	if a == nil {
		writeError(w, http.StatusNotFound, errors.New("no such asset"))
		return
	}
	s.cache.Serve(w, r, a.DriveFileID, a.Size, contentType(a.Format), a.SHA256)
}

func contentType(format string) string {
	switch format {
	case "flac":
		return "audio/flac"
	case "mp3":
		return "audio/mpeg"
	case "m4a":
		return "audio/mp4"
	case "ogg", "opus":
		return "audio/ogg"
	}
	return "application/octet-stream"
}

// ---- covers ----

const maxCoverPixels = 25_000_000 // refuse to decode huge scans on a 1.5 GB VPS (plan §5)

func (s *Server) cover(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	size, _ := strconv.Atoi(r.URL.Query().Get("size"))
	if size < 0 || size > 1024 {
		size = 300
	}
	c, err := s.lib.Cover(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if c == nil || c.DriveFileID == "" {
		writeError(w, http.StatusNotFound, errors.New("no such cover"))
		return
	}
	name := fmt.Sprintf("%s-%d.jpg", c.SHA256, size)
	mime := "image/jpeg"
	if size == 0 {
		name, mime = c.SHA256+"-orig", c.MIME
	}
	path := s.cfg.Path(config.DirThumbs, name)
	data, err := os.ReadFile(path)
	if err != nil {
		if data, err = s.renderCover(r, c.DriveFileID, size); err != nil {
			upstreamError(w, err, 0)
			return
		}
		os.WriteFile(path, data, 0o600)
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable") // content-addressed
	w.Write(data)
}

func (s *Server) renderCover(r *http.Request, driveID string, size int) ([]byte, error) {
	resp, err := s.drive.Do(r.Context(), http.MethodGet, gdrive.MediaURL(driveID), nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	orig, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return orig, nil
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(orig))
	if err != nil {
		return nil, err
	}
	if cfg.Width*cfg.Height > maxCoverPixels {
		return nil, errors.New("cover image too large to resize")
	}
	img, _, err := image.Decode(bytes.NewReader(orig))
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	scale := float64(size) / float64(max(b.Dx(), b.Dy()))
	if scale > 1 {
		scale = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, max(int(float64(b.Dx())*scale), 1), max(int(float64(b.Dy())*scale), 1)))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// ---- imports ----

// importRoots are the only places a server-side import may read from.
func (s *Server) importRoots() []string {
	return []string{s.cfg.Path(config.DirImports), s.cfg.Path(config.DirDownloads), s.cfg.Path(config.DirStaging)}
}

func (s *Server) createImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path        string `json:"path"`         // absolute, or relative to the imports directory
		UploadGroup string `json:"upload_group"` // or: everything a client uploaded in one group
		Preview     bool   `json:"preview"`      // wait in review after analysis (P2-3)
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.UploadGroup != "" {
		s.importUploadGroup(w, r, req.UploadGroup, req.Preview)
		return
	}
	p := req.Path
	if !filepath.IsAbs(p) {
		p = filepath.Join(s.cfg.Path(config.DirImports), p)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("path not found"))
		return
	}
	allowed := false
	for _, root := range s.importRoots() {
		if rr, err := filepath.EvalSymlinks(root); err == nil {
			if rel, err := filepath.Rel(rr, real); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
				allowed = true
			}
		}
	}
	if !allowed {
		writeError(w, http.StatusForbidden, fmt.Errorf("imports must come from %s", strings.Join(s.importRoots(), ", ")))
		return
	}
	id, n, err := s.importer.CreateBatch(r.Context(), "local", req.Path, real, req.Preview)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "files": n})
}

func (s *Server) imports(w http.ResponseWriter, r *http.Request) {
	list, _, err := s.importer.Batches(r.Context(), importer.Page{History: 50})
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) importBatch(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	b, err := s.importer.Batch(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if b == nil {
		writeError(w, http.StatusNotFound, errors.New("no such import"))
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// importPreview shows how a batch's files will be grouped (P2-3).
func (s *Server) importPreview(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p, err := s.importer.Preview(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, errors.New("no such import"))
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) importError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, importer.ErrNotInReview):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, importer.ErrBadOp):
		writeError(w, http.StatusBadRequest, err)
	default:
		s.internal(w, r, err)
	}
}

// editImportPlan applies one change to a batch in review and answers with the new preview.
func (s *Server) editImportPlan(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var op importer.PlanOp
	if err := readJSON(r, &op); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.importer.ApplyOp(r.Context(), id, op); err != nil {
		s.importError(w, r, err)
		return
	}
	s.importPreview(w, r)
}

func (s *Server) startImport(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.importer.Start(r.Context(), id); err != nil {
		s.importError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) cancelImport(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.importer.Cancel(r.Context(), id); err != nil {
		s.importError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sidecar sends a CUE sheet or rip log kept with an album.
func (s *Server) sidecar(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	c, err := s.lib.Sidecar(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if c == nil {
		writeError(w, http.StatusNotFound, errors.New("no such file"))
		return
	}
	resp, err := s.drive.OpenRange(r.Context(), c.DriveFileID, 0, -1)
	if err != nil {
		upstreamError(w, err, 0)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": c.Name}))
	w.Header().Set("Cache-Control", "private, max-age=3600")
	io.Copy(w, io.LimitReader(resp.Body, c.Size))
}

// discardImport lets go of a finished batch's files that are not in the library, so their
// sources can be cleaned up (review #1).
func (s *Server) discardImport(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	n, err := s.importer.Discard(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"discarded": n})
}

func (s *Server) retryImport(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.importer.Retry(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
