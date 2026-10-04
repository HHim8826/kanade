package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/loudness"
)

// Loudness for the player's volume balance (review #136).

func idsParam(r *http.Request, name string, limit int) []int64 {
	var ids []int64
	for _, p := range strings.Split(r.URL.Query().Get(name), ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64); err == nil && id > 0 && len(ids) < limit {
			ids = append(ids, id)
		}
	}
	return ids
}

// getLoudness answers ?assets=1,2&albums=3: the measured ones among them, and the library's median,
// what an unmeasured song is taken for.
func (s *Server) getLoudness(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	assets, err := s.lib.AssetLoudness(ctx, idsParam(r, "assets", 500))
	if err != nil {
		s.internal(w, r, err)
		return
	}
	albums, err := s.lib.AlbumLoudness(ctx, idsParam(r, "albums", 200))
	if err != nil {
		s.internal(w, r, err)
		return
	}
	st, err := s.lib.LoudnessStatus(ctx)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	type out struct {
		Assets map[int64]library.Loudness      `json:"assets"`
		Albums map[int64]library.AlbumLoudness `json:"albums"`
		Median *float64                        `json:"median"`
	}
	writeJSON(w, http.StatusOK, out{Assets: assets, Albums: albums, Median: st.Median})
}

// loudnessScan is how much of the library is measured, and the scan's state.
func (s *Server) loudnessScan(w http.ResponseWriter, r *http.Request) {
	st, err := s.lib.LoudnessStatus(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	var scan loudness.ScanState
	if s.loudness != nil {
		scan = s.loudness.State()
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": st, "scan": scan, "available": s.loudness.Available()})
}

// runLoudnessScan starts ({"run": true}, with "failed": true also the files that could not be
// measured) or stops ({"run": false}) measuring the library.
func (s *Server) runLoudnessScan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Run    bool `json:"run"`
		Failed bool `json:"failed"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !s.loudness.Available() {
		writeError(w, http.StatusServiceUnavailable, errors.New("FFmpeg is not installed, so loudness cannot be measured"))
		return
	}
	if req.Run {
		if err := s.loudness.Scan(req.Failed); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
	} else {
		s.loudness.Stop()
	}
	s.loudnessScan(w, r)
}
