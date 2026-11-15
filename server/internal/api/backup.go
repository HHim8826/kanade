package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/HHim8826/kanade/server/internal/backup"
)

// Database copies in Drive or a managed storage location (review #161).

// backupState is the last copy, and the last failure since.
func (s *Server) backupState(w http.ResponseWriter, r *http.Request) {
	if s.backup == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "state": s.backup.State(r.Context())})
}

// backupNow makes a copy now, in the background.
func (s *Server) backupNow(w http.ResponseWriter, r *http.Request) {
	if s.backup != nil {
		s.backup.Start(r.Context())
	}
	s.backupState(w, r)
}

// setBackupPlace sends the copies from now on to Google Drive ("drive") or a managed storage
// location (its ID).
func (s *Server) setBackupPlace(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Place json.RawMessage `json:"place"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if s.backup == nil {
		writeError(w, http.StatusNotFound, errors.New("backups are off"))
		return
	}
	place := strings.Trim(string(req.Place), `"`) // "drive", or a location's ID as a number or a string
	if err := s.backup.SetPlace(r.Context(), place); err != nil {
		if errors.Is(err, backup.ErrNoPlace) {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		s.storeError(w, r, err)
		return
	}
	s.backupState(w, r)
}
