package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/HHim8826/kanade/server/internal/library"
)

// Smart playlists (review #96).

// previewRules shows what rules pick before they are kept: how many songs fit, and the first of
// what they list.
func (s *Server) previewRules(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Rules library.Rules `json:"rules"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := req.Rules.Check(); err != nil {
		s.libError(w, r, err)
		return
	}
	tracks, matches, err := s.lib.SmartTracks(r.Context(), req.Rules, nil, 0)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	var duration int64
	for _, t := range tracks {
		duration += t.Asset.DurationMS
	}
	first := tracks
	if len(first) > 50 {
		first = first[:50]
	}
	writeJSON(w, http.StatusOK, map[string]any{"matches": matches, "count": len(tracks), "duration_ms": duration, "tracks": first})
}

func (s *Server) setPlaylistRules(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Rules library.Rules `json:"rules"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.lib.SetPlaylistRules(r.Context(), id, req.Rules); err != nil {
		s.libError(w, r, err)
		return
	}
	s.playlistByID(w, r, id, http.StatusOK)
}

// smartNext picks the next songs of a smart playlist played on and on: n songs by its rules and
// order, leaving out those queued and just played (not, the latest first), round again when only
// those are left.
func (s *Server) smartNext(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rules, err := s.lib.PlaylistRules(r.Context(), id)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	if rules == nil {
		writeError(w, http.StatusBadRequest, library.ErrInvalid)
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	n = min(max(n, 1), 20)
	var not []int64
	for _, p := range strings.Split(r.URL.Query().Get("not"), ",") {
		if id, err := strconv.ParseInt(p, 10, 64); err == nil && id > 0 && len(not) < 200 {
			not = append(not, id)
		}
	}
	tracks, err := s.lib.SmartNext(r.Context(), *rules, not, n)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, tracks)
}
