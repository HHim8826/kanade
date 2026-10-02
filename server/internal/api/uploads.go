package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/HHim8826/kanade/server/internal/uploads"
)

func (s *Server) uploadError(w http.ResponseWriter, u *uploads.Upload, err error) {
	switch {
	case errors.Is(err, uploads.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, uploads.ErrOverBudget):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, uploads.ErrOffset):
		// Tell the client where to resume.
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "received": u.Received})
	default:
		writeError(w, http.StatusBadRequest, err)
	}
}

// createUpload: {"group": "...", "path": "Album/Disc 1/01.flac", "size": 123, "sha256": "..."}.
// Repeating the call for the same group and path returns the existing upload, for resuming.
func (s *Server) createUpload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Group  string `json:"group"`
		Path   string `json:"path"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	u, err := s.uploads.Create(r.Context(), req.Group, req.Path, req.Size, req.SHA256)
	if err != nil {
		s.uploadError(w, u, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"upload": u, "chunk_size": uploads.ChunkSize})
}

// appendUpload: PUT /uploads/{id}?offset=N with the chunk as the raw body.
func (s *Server) appendUpload(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || r.ContentLength <= 0 {
		writeError(w, http.StatusBadRequest, errors.New("offset query parameter and Content-Length are required"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, uploads.ChunkSize)
	u, err := s.uploads.Append(r.Context(), id, offset, r.Body, r.ContentLength)
	if err != nil {
		s.uploadError(w, u, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) getUpload(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	u, err := s.uploads.Get(r.Context(), id)
	if err != nil {
		s.uploadError(w, u, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) completeUpload(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	u, err := s.uploads.Complete(r.Context(), id)
	if err != nil {
		s.uploadError(w, u, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// importUploadGroup turns a fully uploaded group into an import batch.
func (s *Server) importUploadGroup(w http.ResponseWriter, r *http.Request, group string) {
	if _, err := s.uploads.ReadyForImport(r.Context(), group); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	id, n, err := s.importer.CreateBatch(r.Context(), "upload", group, s.uploads.GroupDir(group))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.uploads.MarkImported(r.Context(), group); err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "files": n})
}
