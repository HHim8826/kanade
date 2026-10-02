package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/HHim8826/kanade/server/internal/library"
)

// Favorites, playlists, lyrics and history (P2-1).

// libError maps the library's sentinel errors onto HTTP statuses.
func (s *Server) libError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, library.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, library.ErrInvalid):
		writeError(w, http.StatusBadRequest, err)
	default:
		s.internal(w, r, err)
	}
}

func (s *Server) favorites(w http.ResponseWriter, r *http.Request) {
	f, err := s.lib.Favorites(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) favoriteIDs(w http.ResponseWriter, r *http.Request) {
	ids, err := s.lib.FavoriteIDs(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ids)
}

// setFavorite serves PUT (add) and DELETE (remove) on /favorites/{kind}s/{id}.
func (s *Server) setFavorite(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.lib.SetFavorite(r.Context(), kind, id, r.Method == http.MethodPut); err != nil {
			s.libError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) playlists(w http.ResponseWriter, r *http.Request) {
	list, err := s.lib.Playlists(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

type playlistBody struct {
	Name        string                `json:"name"`
	Description string                `json:"description"`
	Items       []library.PlaylistAdd `json:"items"` // on create: initial items
}

func (s *Server) createPlaylist(w http.ResponseWriter, r *http.Request) {
	var req playlistBody
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	id, err := s.lib.CreatePlaylist(r.Context(), req.Name, req.Description)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	if len(req.Items) > 0 {
		if _, err := s.lib.AddToPlaylist(r.Context(), id, req.Items); err != nil && !errors.Is(err, library.ErrNotFound) {
			s.libError(w, r, err)
			return
		}
	}
	s.playlistByID(w, r, id, http.StatusCreated)
}

func (s *Server) playlist(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.playlistByID(w, r, id, http.StatusOK)
}

func (s *Server) playlistByID(w http.ResponseWriter, r *http.Request, id int64, status int) {
	p, err := s.lib.Playlist(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, library.ErrNotFound)
		return
	}
	writeJSON(w, status, p)
}

func (s *Server) updatePlaylist(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req playlistBody
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.lib.UpdatePlaylist(r.Context(), id, req.Name, req.Description); err != nil {
		s.libError(w, r, err)
		return
	}
	s.playlistByID(w, r, id, http.StatusOK)
}

func (s *Server) deletePlaylist(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.lib.DeletePlaylist(r.Context(), id); err != nil {
		s.libError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) addPlaylistItems(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req playlistBody
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	n, err := s.lib.AddToPlaylist(r.Context(), id, req.Items)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"added": n})
}

func (s *Server) removePlaylistItem(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	item, err := strconv.ParseInt(r.PathValue("item"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("bad item id"))
		return
	}
	if err := s.lib.RemovePlaylistItem(r.Context(), id, item); err != nil {
		s.libError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) reorderPlaylist(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Items []int64 `json:"items"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.lib.ReorderPlaylist(r.Context(), id, req.Items); err != nil {
		s.libError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) lyrics(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	l, err := s.lib.Lyrics(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if l == nil {
		writeError(w, http.StatusNotFound, errors.New("no lyrics"))
		return
	}
	writeJSON(w, http.StatusOK, l)
}

// setLyrics stores hand-written lyrics (PUT) or removes them (DELETE).
func (s *Server) setLyrics(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if r.Method == http.MethodPut {
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	if _, err := s.lib.SetLyrics(r.Context(), id, library.LyricsManual, req.Text); err != nil {
		s.libError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	limit, _ := pageArgs(r)
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	beforeID, _ := strconv.ParseInt(r.URL.Query().Get("before_id"), 10, 64)
	list, err := s.lib.History(r.Context(), limit, before, beforeID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) topTracks(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 3650 {
		days = 30
	}
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	list, err := s.lib.TopTracks(r.Context(), since, 50)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}
