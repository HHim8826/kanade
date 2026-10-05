package api

import "net/http"

// Database copies in Drive (review #161).

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
