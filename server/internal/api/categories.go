package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/HHim8826/kanade/server/internal/library"
)

// Album categories of the user's own (review #92).

func (s *Server) categories(w http.ResponseWriter, r *http.Request) {
	var albums []int64
	for _, p := range strings.Split(r.URL.Query().Get("albums"), ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64); err == nil && id > 0 {
			albums = append(albums, id)
		}
	}
	list, err := s.lib.Categories(r.Context(), albums)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) createCategory(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	c, err := s.lib.CreateCategory(r.Context(), req.Name)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) renameCategory(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.lib.RenameCategory(r.Context(), id, req.Name); err != nil {
		s.libError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteCategory(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	g, err := s.lib.DeleteCategory(r.Context(), id)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"group": g})
}

// categorize puts albums into categories and out of others, as one edit.
func (s *Server) categorize(w http.ResponseWriter, r *http.Request) {
	var req library.CategorizeRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	g, created, err := s.lib.Categorize(r.Context(), req)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"group": g, "created": created})
}
