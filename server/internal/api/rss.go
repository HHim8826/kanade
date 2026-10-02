package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/HHim8826/kanade/server/internal/downloader"
	"github.com/HHim8826/kanade/server/internal/rss"
)

// RSS sources and items (P2-5).

func (s *Server) rssError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, rss.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, rss.ErrInvalid):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, downloader.ErrNotReady):
		writeError(w, http.StatusServiceUnavailable, err)
	case r.Context().Err() != nil:
		s.internal(w, r, err)
	default: // the feed or the torrent link failed: the site's problem, said as it is
		writeError(w, http.StatusBadGateway, err)
	}
}

func (s *Server) rssSources(w http.ResponseWriter, r *http.Request) {
	list, err := s.rss.Sources(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) createRSSSource(w http.ResponseWriter, r *http.Request) {
	var in rss.SourceInput
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	src, err := s.rss.Create(r.Context(), in)
	if err != nil {
		s.rssError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, src)
}

func (s *Server) updateRSSSource(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var in rss.SourceInput
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	src, err := s.rss.Update(r.Context(), id, in)
	if err != nil {
		s.rssError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, src)
}

func (s *Server) deleteRSSSource(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.rss.Delete(r.Context(), id); err != nil {
		s.rssError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// refreshRSSSource fetches a source now and says what came in.
func (s *Server) refreshRSSSource(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.rss.Poll(r.Context(), id)
	if err != nil {
		s.rssError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) searchRSSSource(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	list, err := s.rss.Search(r.Context(), id, r.URL.Query().Get("q"))
	if err != nil {
		s.rssError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// downloadRSSLink starts a download from a live search result of a source (with its credentials).
func (s *Server) downloadRSSLink(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Link string `json:"link"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	dl, err := s.rss.DownloadLink(r.Context(), id, req.Link)
	if err != nil {
		s.rssError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": dl})
}

func (s *Server) rssItems(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	source, _ := strconv.ParseInt(q.Get("source"), 10, 64)
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	list, err := s.rss.Items(r.Context(), rss.ItemQuery{Source: source, Q: q.Get("q"), Only: q.Get("only"), Limit: limit, Before: before})
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) downloadRSSItem(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	dl, err := s.rss.Download(r.Context(), id)
	if err != nil {
		s.rssError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": dl})
}
