package api

import (
	"errors"
	"net/http"

	"github.com/HHim8826/kanade/server/internal/auth"
)

// The account's own settings (review #76): its password and its logins. Each acts on the account of
// the login asking, never one named in the request.

// changePassword sets a new password after the current one is confirmed. Every login of the
// account ends, this one too: the client logs in again with the new password.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current  string `json:"current"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	err := s.auth.ChangePassword(r.Context(), userID(r), req.Current, req.Password, clientIP(r))
	switch {
	case errors.Is(err, auth.ErrThrottled):
		writeError(w, http.StatusTooManyRequests, err)
	case errors.Is(err, auth.ErrBadCredentials): // not 401: the login itself is fine
		writeError(w, http.StatusForbidden, errors.New("wrong password"))
	case errors.Is(err, auth.ErrSamePassword), errors.Is(err, auth.ErrShortPassword):
		writeError(w, http.StatusBadRequest, err)
	case err != nil:
		s.internal(w, r, err)
	default:
		s.clearCookie(w)
		w.WriteHeader(http.StatusNoContent)
	}
}

// sessions lists the account's logins.
func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	token, _ := sessionToken(r)
	list, err := s.auth.Sessions(r.Context(), userID(r), token)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// endSession ends another login of the account.
func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	token, _ := sessionToken(r)
	err = s.auth.EndSession(r.Context(), userID(r), id, token)
	switch {
	case errors.Is(err, auth.ErrNoSuchSession):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, auth.ErrCurrentSession):
		writeError(w, http.StatusConflict, err)
	case err != nil:
		s.internal(w, r, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// endOtherSessions ends every login of the account but this one.
func (s *Server) endOtherSessions(w http.ResponseWriter, r *http.Request) {
	token, _ := sessionToken(r)
	n, err := s.auth.EndOtherSessions(r.Context(), userID(r), token)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"ended": n})
}
