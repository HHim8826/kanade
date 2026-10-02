package api

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/HHim8826/kanade/server/internal/downloader"
)

func (s *Server) downloadError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, downloader.ErrNotReady):
		writeError(w, http.StatusServiceUnavailable, err)
	case errors.Is(err, downloader.ErrOverBudget), errors.Is(err, downloader.ErrBadState):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, downloader.ErrLowDisk):
		writeError(w, http.StatusInsufficientStorage, err)
	default:
		writeError(w, http.StatusBadRequest, err)
	}
}

// createDownload accepts {"uri": "magnet:..."} / {"uri": "https://.../x.torrent"} as JSON,
// or the raw bytes of a .torrent file with Content-Type application/x-bittorrent.
func (s *Server) createDownload(w http.ResponseWriter, r *http.Request) {
	var uri string
	var torrent []byte
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-bittorrent") {
		b, err := io.ReadAll(io.LimitReader(r.Body, 10<<20+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		torrent = b
	} else {
		var req struct {
			URI string `json:"uri"`
		}
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		uri = req.URI
	}
	id, err := s.downloads.Add(r.Context(), uri, torrent, false)
	if err != nil {
		s.downloadError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (s *Server) listDownloads(w http.ResponseWriter, r *http.Request) {
	limit, _ := pageArgs(r)
	list, err := s.downloads.List(r.Context(), limit)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) getDownload(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	v, err := s.downloads.Get(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if v == nil {
		writeError(w, http.StatusNotFound, errors.New("no such download"))
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) selectFiles(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Files []int `json:"files"` // indexes from the file list; omit to take the suggested set
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(req.Files) == 0 {
		v, err := s.downloads.Get(r.Context(), id)
		if err != nil || v == nil {
			writeError(w, http.StatusNotFound, errors.New("no such download"))
			return
		}
		for _, f := range v.Files {
			if f.Suggested {
				req.Files = append(req.Files, f.Index)
			}
		}
	}
	if err := s.downloads.Select(r.Context(), id, req.Files); err != nil {
		s.downloadError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) downloadAction(action func(*downloader.Service, *http.Request, int64) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := action(s.downloads, r, id); err != nil {
			s.downloadError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// tasks is the task center (plan §2): downloads and import batches in one response.
func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	downloads, err := s.downloads.List(r.Context(), 50)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	imports, err := s.importer.Batches(r.Context(), 50)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := map[string]any{"downloads": downloads, "imports": imports}
	if s.disk != nil {
		out["disk"] = s.disk.Status() // low disk: the task page says what is held back
	}
	writeJSON(w, http.StatusOK, out)
}
