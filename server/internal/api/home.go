package api

import (
	"errors"
	"net/http"

	"github.com/HHim8826/kanade/server/internal/library"
)

// recordPlay receives playback reports from clients (decision D9).
func (s *Server) recordPlay(w http.ResponseWriter, r *http.Request) {
	var rep library.PlayReport
	if err := readJSON(r, &rep); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.lib.RecordPlay(r.Context(), rep); err != nil {
		if errors.Is(err, library.ErrBadPlay) {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		s.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) resumePosition(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	pos, err := s.lib.ResumePosition(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"position_ms": pos})
}

func (s *Server) randomAlbum(w http.ResponseWriter, r *http.Request) {
	id, err := s.lib.RandomAlbum(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if id == 0 {
		writeError(w, http.StatusNotFound, errors.New("no albums yet"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"id": id})
}

type taskSummary struct {
	Downloads map[string]int `json:"downloads"` // active states only
	Importing int            `json:"importing"` // files waiting or uploading
}

// home gathers everything the home page shows in one request.
func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cont, err := s.lib.Continue(ctx)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	played, err := s.lib.RecentlyPlayedAlbums(ctx, 12)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	added, err := s.lib.Albums(ctx, 12, 0, true)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	spoken, err := s.lib.UnfinishedSpoken(ctx, 10)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	attention, err := s.lib.Attention(ctx)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	tasks := taskSummary{Downloads: map[string]int{}}
	rows, err := s.db.QueryContext(ctx, `SELECT state, count(*) FROM downloads
		WHERE state IN ('metadata', 'selecting', 'queued', 'downloading', 'paused', 'seeding') GROUP BY state`)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	for rows.Next() {
		var state string
		var n int
		if rows.Scan(&state, &n) == nil {
			tasks.Downloads[state] = n
		}
	}
	rows.Close()
	s.db.QueryRowContext(ctx, `SELECT count(*) FROM import_items WHERE state IN ('pending', 'uploading')`).Scan(&tasks.Importing)
	writeJSON(w, http.StatusOK, map[string]any{
		"continue":        cont, // null when nothing is half-heard
		"recently_played": played,
		"recently_added":  added,
		"spoken":          spoken,
		"tasks":           tasks,
		"attention":       attention,
	})
}
