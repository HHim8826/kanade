package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/HHim8826/kanade/server/internal/mover"
)

// ---- moving the library's files to another storage location ----

// previewMove says what moving {dest, origin} or {dest, albums} would do.
func (s *Server) previewMove(w http.ResponseWriter, r *http.Request) {
	var req mover.Request
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p, err := s.mover.Plan(r.Context(), req)
	if err != nil {
		s.moveError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// startMove asks for a move; its files are moved one after another.
func (s *Server) startMove(w http.ResponseWriter, r *http.Request) {
	var req mover.Request
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	v, err := s.mover.Start(r.Context(), req)
	if err != nil {
		s.moveError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) moves(w http.ResponseWriter, r *http.Request) {
	list, err := s.mover.List(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"moves": list})
}

func (s *Server) move(w http.ResponseWriter, r *http.Request) {
	s.onMove(w, r, s.mover.Get)
}

func (s *Server) cancelMove(w http.ResponseWriter, r *http.Request) {
	s.onMove(w, r, s.mover.Cancel)
}

func (s *Server) retryMove(w http.ResponseWriter, r *http.Request) {
	s.onMove(w, r, s.mover.Retry)
}

func (s *Server) clearMove(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if _, err := s.mover.Clear(r.Context(), id); err != nil {
		s.moveError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) onMove(w http.ResponseWriter, r *http.Request, do func(ctx context.Context, id int64) (*mover.Move, error)) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	v, err := do(r.Context(), id)
	if err != nil {
		s.moveError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) moveError(w http.ResponseWriter, r *http.Request, err error) {
	var bad *mover.ErrInvalid
	switch {
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, mover.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, mover.ErrBusy):
		writeError(w, http.StatusConflict, err)
	default:
		s.internal(w, r, err)
	}
}
