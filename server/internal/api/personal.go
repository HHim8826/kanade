package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/lrclib"
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

// playlists lists the playlists; ?plain=1 only those songs can be added to (review #162).
func (s *Server) playlists(w http.ResponseWriter, r *http.Request) {
	list, err := s.lib.Playlists(r.Context(), r.URL.Query().Get("plain") == "1")
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
	Rules       *library.Rules        `json:"rules"` // a smart playlist (review #96)
}

func (s *Server) createPlaylist(w http.ResponseWriter, r *http.Request) {
	var req playlistBody
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var id int64
	var err error
	if req.Rules != nil {
		id, err = s.lib.CreateSmartPlaylist(r.Context(), req.Name, req.Description, *req.Rules)
	} else {
		id, err = s.lib.CreatePlaylist(r.Context(), req.Name, req.Description)
	}
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

// findLyrics looks the track's lyrics up in LRCLIB (on request: only this song's title and artist
// are sent; its length ranks the answers) and lists what fits, best first. A search adjusted by
// hand gives the title and artist to look for (title, artist), or keywords (q) (review #122).
func (s *Server) findLyrics(w http.ResponseWriter, r *http.Request) {
	t, ok := s.lyricsTrack(w, r)
	if !ok {
		return
	}
	song := lrclib.Song{Title: t.Title, Artist: t.Artist, Album: t.Album, DurationMS: t.Asset.DurationMS}
	q := r.URL.Query()
	if q.Has("title") {
		song.Title, song.Artist = strings.TrimSpace(q.Get("title")), strings.TrimSpace(q.Get("artist"))
	}
	if len(song.Title) > 500 || len(song.Artist) > 500 || len(q.Get("q")) > 500 {
		writeError(w, http.StatusBadRequest, errors.New("search too long"))
		return
	}
	var list []lrclib.Candidate
	var err error
	if keywords := strings.TrimSpace(q.Get("q")); keywords != "" {
		list, err = s.lrclib.Keywords(r.Context(), song, keywords)
	} else if song.Title == "" {
		writeError(w, http.StatusBadRequest, errors.New("a title or keywords to look for"))
		return
	} else {
		list, err = s.lrclib.Find(r.Context(), song)
	}
	if err != nil {
		s.lrclibError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// useFoundLyrics stores lyrics found in LRCLIB: {"id": n} as the user's choice, which replaces
// what is there, or with "auto": true only where the track has none yet (an exact match the page
// took without asking).
func (s *Server) useFoundLyrics(w http.ResponseWriter, r *http.Request) {
	t, ok := s.lyricsTrack(w, r)
	if !ok {
		return
	}
	var req struct {
		ID   int64 `json:"id"`
		Auto bool  `json:"auto"`
	}
	if err := readJSON(r, &req); err != nil || req.ID <= 0 {
		writeError(w, http.StatusBadRequest, errors.New("expected the LRCLIB id"))
		return
	}
	l, err := s.lrclib.Get(r.Context(), req.ID)
	if err != nil {
		s.lrclibError(w, r, err)
		return
	}
	if l.Instrumental || strings.TrimSpace(l.Text()) == "" {
		writeError(w, http.StatusUnprocessableEntity, errors.New("LRCLIB has no words for it (instrumental)"))
		return
	}
	saved, err := s.lib.SetFoundLyrics(r.Context(), t.ID, l.Text(), !req.Auto)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"saved": saved})
}

func (s *Server) lyricsTrack(w http.ResponseWriter, r *http.Request) (*library.TrackDetail, bool) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return nil, false
	}
	if s.lrclib == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("online lyrics are not set up"))
		return nil, false
	}
	t, err := s.lib.Track(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return nil, false
	}
	if t == nil {
		writeError(w, http.StatusNotFound, errors.New("no such track"))
		return nil, false
	}
	return t, true
}

func (s *Server) lrclibError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, lrclib.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, lrclib.ErrUnavailable):
		upstreamError(w, err, 30)
	default:
		s.log.Warn("lrclib", "path", r.URL.Path, "err", err)
		upstreamError(w, err, 0)
	}
}
