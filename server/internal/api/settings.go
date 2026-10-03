package api

import (
	"errors"
	"net/http"

	"github.com/HHim8826/kanade/server/internal/settings"
)

// The service settings (reviews #74, #75, #77). They are the whole service's, so any account
// changes them: every Kanade account administers the server it runs on.

// serviceSettings answers the settings with what is in effect now: resources given as serve flags
// win until the next start without them.
func (s *Server) serviceSettings(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("settings are not available"))
		return
	}
	ctx := r.Context()
	res, err := s.settings.Resources(ctx)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	dl, err := s.settings.Downloads(ctx)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	dr, err := s.settings.Drive(ctx)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	now := map[string]int64{}
	usage := map[string]int64{}
	if s.cache != nil {
		now["cache_mib"], usage["cache_bytes"] = s.cache.Budget()>>20, s.cache.Used()
	}
	if s.staging != nil {
		limit, reserve := s.staging.Limits()
		now["staging_mib"], now["reserve_gib"], usage["staging_bytes"] = limit>>20, reserve>>30, s.staging.Used(ctx)
	}
	pinned := []string{}
	for k, on := range s.pinned {
		if on {
			pinned = append(pinned, k)
		}
	}
	server := map[string]any{"listen": s.cfg.Listen, "public_url": s.cfg.PublicURL, "data_dir": s.cfg.DataDir,
		"aria2": s.cfg.Aria2Path, "ffmpeg": ""}
	if s.importer != nil && s.importer.FFmpeg != nil {
		server["ffmpeg"] = s.importer.FFmpeg.Path()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resources": map[string]any{"saved": res, "now": now, "pinned": pinned, "usage": usage},
		"downloads": dl,
		"drive":     dr,
		"server":    server,
	})
}

func (s *Server) settingsSaved(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, settings.ErrInvalid):
		writeError(w, http.StatusBadRequest, err)
	case err != nil:
		s.internal(w, r, err)
	default:
		s.serviceSettings(w, r)
	}
}

func (s *Server) setResources(w http.ResponseWriter, r *http.Request) {
	var v settings.Resources
	if err := readJSON(r, &v); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.settingsSaved(w, r, s.settings.SetResources(r.Context(), v))
}

func (s *Server) setDownloads(w http.ResponseWriter, r *http.Request) {
	var v settings.Downloads
	if err := readJSON(r, &v); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.settingsSaved(w, r, s.settings.SetDownloads(r.Context(), v))
}

func (s *Server) setDriveSync(w http.ResponseWriter, r *http.Request) {
	var v settings.Drive
	if err := readJSON(r, &v); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.settingsSaved(w, r, s.settings.SetDrive(r.Context(), v))
}

// trimCache drops the stream cache's files nobody is playing now and says what that freed; the
// library and the files of imports are not in it.
func (s *Server) trimCache(w http.ResponseWriter, r *http.Request) {
	if s.cache == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("no stream cache"))
		return
	}
	freed := s.cache.Trim()
	writeJSON(w, http.StatusOK, map[string]int64{"freed_bytes": freed, "cache_bytes": s.cache.Used()})
}
