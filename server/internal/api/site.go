package api

import (
	"errors"
	"net/http"

	"github.com/HHim8826/kanade/server/internal/site"
)

// The site's name and icon: the whole service's, so any account changes them, like the other
// service settings. The pages read them from the shell (internal/web); these are for the settings.

func (s *Server) siteInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"name": s.site.Name(), "default": site.Default, "icon": s.site.Icon()})
}

// siteSaved answers a change with the site as it is now, or says what was wrong: the reason is for
// the page to say it in its own words.
func (s *Server) siteSaved(w http.ResponseWriter, r *http.Request, err error) {
	reasons := map[error]string{site.ErrName: "name", site.ErrFormat: "format", site.ErrSmall: "small", site.ErrLarge: "large"}
	for e, reason := range reasons {
		if errors.Is(err, e) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "reason": reason})
			return
		}
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	s.siteInfo(w, r)
}

func (s *Server) setSiteName(w http.ResponseWriter, r *http.Request) {
	if s.site == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("the site cannot be renamed here"))
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.siteSaved(w, r, s.site.SetName(r.Context(), req.Name))
}

// setSiteIcon takes the image as the request's body.
func (s *Server) setSiteIcon(w http.ResponseWriter, r *http.Request) {
	if s.site == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("the site's icon cannot be changed here"))
		return
	}
	s.siteSaved(w, r, s.site.SetIcon(r.Context(), r.Body))
}

func (s *Server) clearSiteIcon(w http.ResponseWriter, r *http.Request) {
	if s.site == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("the site's icon cannot be changed here"))
		return
	}
	s.siteSaved(w, r, s.site.ClearIcon(r.Context()))
}
