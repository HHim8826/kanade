package api

import (
	"net/http"
	"strconv"
)

// Bookmarks: named places in songs (review #98).

func (s *Server) bookmarks(w http.ResponseWriter, r *http.Request) {
	track, _ := strconv.ParseInt(r.URL.Query().Get("track"), 10, 64)
	list, err := s.lib.Bookmarks(r.Context(), track)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) addBookmark(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AssetID    int64  `json:"asset_id"`
		PositionMS int64  `json:"position_ms"`
		Name       string `json:"name"`
		Note       string `json:"note"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	b, err := s.lib.AddBookmark(r.Context(), req.AssetID, req.PositionMS, req.Name, req.Note)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

func (s *Server) updateBookmark(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Name string `json:"name"`
		Note string `json:"note"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.lib.UpdateBookmark(r.Context(), id, req.Name, req.Note); err != nil {
		s.libError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteBookmark(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.lib.DeleteBookmark(r.Context(), id); err != nil {
		s.libError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
