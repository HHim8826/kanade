package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/HHim8826/kanade/server/internal/config"
	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/importer"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/thumbs"
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

// prefetchStream starts filling the cache with a song a player will play next: a preload, which
// leaves the song playing its download (a stream request is the song played; review #175), and
// which the cache skips when the disk is low or downloads are busy.
func (s *Server) prefetchStream(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	a, err := s.lib.StreamTarget(r.Context(), id)
	if err != nil || a == nil {
		w.WriteHeader(http.StatusNoContent) // nothing to preload: played, it says why
		return
	}
	if err := s.cache.Prefetch(a.DriveFileID, a.Size); err != nil {
		s.log.Info("prefetch", "asset", id, "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
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

// cover is a cover at ?size (one of thumbs.Sizes, the nearest above; 0 or none: the original),
// made once and kept (review #157).
func (s *Server) cover(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	size, _ := strconv.Atoi(r.URL.Query().Get("size"))
	size = thumbs.Size(size)
	c, err := s.lib.Cover(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if c == nil || c.DriveFileID == "" {
		writeError(w, http.StatusNotFound, errors.New("no such cover"))
		return
	}
	data, err := s.thumbs.Get(r.Context(), c.SHA256, size, func(ctx context.Context) (io.ReadCloser, error) {
		resp, err := s.drive.Do(ctx, http.MethodGet, gdrive.MediaURL(c.DriveFileID), nil, nil)
		if err != nil {
			return nil, err
		}
		return resp.Body, nil
	})
	switch {
	case errors.Is(err, thumbs.ErrUndecodable):
		writeError(w, http.StatusNotFound, err)
		return
	case err != nil:
		if r.Context().Err() == nil {
			upstreamError(w, err, 0)
		}
		return
	}
	mime := "image/jpeg"
	if size == 0 {
		mime = c.MIME
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable") // content-addressed
	w.Write(data)
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
