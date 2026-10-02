// Package api is the HTTP surface: public pages, the OAuth callback and the JSON API.
package api

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/HHim8826/kanade/server/internal/auth"
	"github.com/HHim8826/kanade/server/internal/config"
	"github.com/HHim8826/kanade/server/internal/downloader"
	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/importer"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/stream"
	"github.com/HHim8826/kanade/server/internal/uploads"
	"github.com/HHim8826/kanade/server/internal/web"
)

//go:embed pages/*.html
var pages embed.FS

// Deps are the services the HTTP layer exposes.
type Deps struct {
	Config    config.Config
	DB        *sql.DB
	Auth      *auth.Service
	Drive     *gdrive.Client
	Library   *library.Store
	Importer  *importer.Importer
	Cache     *stream.Cache
	Downloads *downloader.Service
	Aria2     *downloader.Aria2
	Uploads   *uploads.Store
	StreamKey []byte // HMAC key for signed stream URLs
	Log       *slog.Logger
}

type Server struct {
	cfg       config.Config
	db        *sql.DB
	auth      *auth.Service
	drive     *gdrive.Client
	lib       *library.Store
	importer  *importer.Importer
	cache     *stream.Cache
	downloads *downloader.Service
	aria2     *downloader.Aria2
	uploads   *uploads.Store
	streamKey []byte
	log       *slog.Logger
	started   time.Time
}

func New(d Deps) *Server {
	return &Server{cfg: d.Config, db: d.DB, auth: d.Auth, drive: d.Drive, lib: d.Library, importer: d.Importer,
		cache: d.Cache, downloads: d.Downloads, aria2: d.Aria2, uploads: d.Uploads, streamKey: d.StreamKey, log: d.Log, started: time.Now()}
}

type ctxKey int

const userKey ctxKey = 0

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page("pages/index.html"))
	mux.HandleFunc("GET /privacy", s.page("pages/privacy.html"))
	mux.HandleFunc("GET /oauth/google/callback", s.oauthCallback)
	mux.Handle("GET /app/", web.Handler())
	mux.Handle("GET /app", http.RedirectHandler("/app/", http.StatusMovedPermanently))

	mux.HandleFunc("POST /api/v1/setup", s.setup)
	mux.HandleFunc("POST /api/v1/login", s.login)
	mux.Handle("POST /api/v1/logout", s.authed(s.logout))
	mux.Handle("GET /api/v1/status", s.authed(s.status))
	mux.Handle("GET /api/v1/drive", s.authed(s.driveInfo))
	mux.Handle("POST /api/v1/drive/client", s.authed(s.driveSetClient))
	mux.Handle("POST /api/v1/drive/auth", s.authed(s.driveAuth))
	mux.Handle("POST /api/v1/drive/auth/paste", s.authed(s.driveAuthPaste))

	mux.Handle("GET /api/v1/home", s.authed(s.home))
	mux.Handle("POST /api/v1/plays", s.authed(s.recordPlay))
	mux.Handle("GET /api/v1/assets/{id}/resume", s.authed(s.resumePosition))
	mux.Handle("GET /api/v1/albums/random", s.authed(s.randomAlbum))
	mux.Handle("GET /api/v1/albums", s.authed(s.albums))
	mux.Handle("GET /api/v1/albums/{id}", s.authed(s.album))
	mux.Handle("GET /api/v1/tracks", s.authed(s.tracks))
	mux.Handle("GET /api/v1/artists", s.authed(s.artists))
	mux.Handle("GET /api/v1/artists/{id}", s.authed(s.artist))
	mux.Handle("GET /api/v1/search", s.authed(s.search))

	mux.Handle("GET /api/v1/favorites", s.authed(s.favorites))
	mux.Handle("GET /api/v1/favorites/ids", s.authed(s.favoriteIDs))
	mux.Handle("PUT /api/v1/favorites/tracks/{id}", s.authed(s.setFavorite("track")))
	mux.Handle("DELETE /api/v1/favorites/tracks/{id}", s.authed(s.setFavorite("track")))
	mux.Handle("PUT /api/v1/favorites/albums/{id}", s.authed(s.setFavorite("album")))
	mux.Handle("DELETE /api/v1/favorites/albums/{id}", s.authed(s.setFavorite("album")))
	mux.Handle("GET /api/v1/playlists", s.authed(s.playlists))
	mux.Handle("POST /api/v1/playlists", s.authed(s.createPlaylist))
	mux.Handle("GET /api/v1/playlists/{id}", s.authed(s.playlist))
	mux.Handle("PATCH /api/v1/playlists/{id}", s.authed(s.updatePlaylist))
	mux.Handle("DELETE /api/v1/playlists/{id}", s.authed(s.deletePlaylist))
	mux.Handle("POST /api/v1/playlists/{id}/items", s.authed(s.addPlaylistItems))
	mux.Handle("DELETE /api/v1/playlists/{id}/items/{item}", s.authed(s.removePlaylistItem))
	mux.Handle("PUT /api/v1/playlists/{id}/order", s.authed(s.reorderPlaylist))
	mux.Handle("GET /api/v1/tracks/{id}/lyrics", s.authed(s.lyrics))
	mux.Handle("PUT /api/v1/tracks/{id}/lyrics", s.authed(s.setLyrics))
	mux.Handle("DELETE /api/v1/tracks/{id}/lyrics", s.authed(s.setLyrics))
	mux.Handle("GET /api/v1/history", s.authed(s.history))
	mux.Handle("GET /api/v1/history/top", s.authed(s.topTracks))
	mux.Handle("GET /api/v1/covers/{id}", s.authed(s.cover))
	mux.Handle("POST /api/v1/stream/{id}/url", s.authed(s.streamURL))
	mux.HandleFunc("GET /api/v1/stream/{id}", s.stream) // header token or signed URL, checked inside
	mux.HandleFunc("HEAD /api/v1/stream/{id}", s.stream)

	mux.Handle("POST /api/v1/imports", s.authed(s.createImport))
	mux.Handle("GET /api/v1/imports", s.authed(s.imports))
	mux.Handle("GET /api/v1/imports/{id}", s.authed(s.importBatch))
	mux.Handle("POST /api/v1/imports/{id}/retry", s.authed(s.retryImport))

	mux.Handle("POST /api/v1/downloads", s.authed(s.createDownload))
	mux.Handle("GET /api/v1/downloads", s.authed(s.listDownloads))
	mux.Handle("GET /api/v1/downloads/{id}", s.authed(s.getDownload))
	mux.Handle("POST /api/v1/downloads/{id}/select", s.authed(s.selectFiles))
	mux.Handle("POST /api/v1/downloads/{id}/pause", s.authed(s.downloadAction(func(d *downloader.Service, r *http.Request, id int64) error { return d.Pause(r.Context(), id) })))
	mux.Handle("POST /api/v1/downloads/{id}/resume", s.authed(s.downloadAction(func(d *downloader.Service, r *http.Request, id int64) error { return d.Resume(r.Context(), id) })))
	mux.Handle("POST /api/v1/downloads/{id}/cancel", s.authed(s.downloadAction(func(d *downloader.Service, r *http.Request, id int64) error { return d.Cancel(r.Context(), id) })))
	mux.Handle("GET /api/v1/tasks", s.authed(s.tasks))

	mux.Handle("POST /api/v1/uploads", s.authed(s.createUpload))
	mux.Handle("GET /api/v1/uploads/{id}", s.authed(s.getUpload))
	mux.Handle("PUT /api/v1/uploads/{id}", s.authed(s.appendUpload))
	mux.Handle("POST /api/v1/uploads/{id}/complete", s.authed(s.completeUpload))
	return s.logRequests(mux)
}

// ---- helpers ----

type apiError struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, apiError{Error: err.Error()})
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return errors.New("invalid JSON body: " + err.Error())
	}
	return nil
}

// clientIP trusts proxy headers only from loopback, i.e. the Cloudflare tunnel on this host.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if v := r.Header.Get("CF-Connecting-IP"); v != "" {
			return v
		}
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			return strings.TrimSpace(strings.Split(v, ",")[0])
		}
	}
	return host
}

// sessionCookie carries the login token for the web client. It is HttpOnly, so page scripts
// cannot read it, and SameSite=Strict.
const sessionCookie = "kanade_session"

// csrfHeader must accompany cookie-authenticated requests that change state. A cross-site page
// cannot add a custom header without a CORS preflight, which this server never grants.
const csrfHeader = "X-Requested-With"

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	return ""
}

// sessionToken returns the Bearer token, or else the session cookie.
func sessionToken(r *http.Request) (token string, fromCookie bool) {
	if t := bearerToken(r); t != "" {
		return t, false
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value, true
	}
	return "", false
}

func (s *Server) authed(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, fromCookie := sessionToken(r)
		if fromCookie && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(csrfHeader) != "kanade" {
			writeError(w, http.StatusForbidden, errors.New("missing "+csrfHeader+" header"))
			return
		}
		uid, err := s.auth.Authenticate(r.Context(), token)
		if errors.Is(err, auth.ErrNoSession) {
			writeError(w, http.StatusUnauthorized, err)
			return
		}
		if err != nil {
			s.internal(w, r, err)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey, uid)))
	})
}

func (s *Server) internal(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, errors.New("internal error"))
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logRequests never logs query strings: they can carry OAuth codes or tokens.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t0 := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"ms", time.Since(t0).Milliseconds(), "ip", clientIP(r), "ua", r.UserAgent())
	})
}

func (s *Server) page(name string) http.HandlerFunc {
	body, err := pages.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Write(body)
	}
}

// ---- accounts ----

// SetupCodePath holds the one-time code required to create the first account, so nobody who
// merely reaches the public URL can claim the server before its owner does.
func (s *Server) SetupCodePath() string { return s.cfg.Path("setup-code") }

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SetupCode string `json:"setup_code"`
		Username  string `json:"username"`
		Password  string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	code, err := os.ReadFile(s.SetupCodePath())
	if err != nil {
		writeError(w, http.StatusForbidden, errors.New("setup is not open"))
		return
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(code))), []byte(req.SetupCode)) != 1 {
		writeError(w, http.StatusForbidden, errors.New("wrong setup code"))
		return
	}
	if err := s.auth.CreateUser(r.Context(), req.Username, req.Password, true); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	os.Remove(s.SetupCodePath())
	writeJSON(w, http.StatusCreated, map[string]string{"username": req.Username})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Device   string `json:"device"`
		Cookie   bool   `json:"cookie"` // web client: set an HttpOnly cookie instead of returning the token
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	token, err := s.auth.Login(r.Context(), req.Username, req.Password, req.Device, clientIP(r))
	switch {
	case errors.Is(err, auth.ErrThrottled):
		writeError(w, http.StatusTooManyRequests, err)
	case errors.Is(err, auth.ErrBadCredentials):
		writeError(w, http.StatusUnauthorized, err)
	case err != nil:
		s.internal(w, r, err)
	case req.Cookie:
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", MaxAge: 90 * 24 * 3600,
			HttpOnly: true, Secure: strings.HasPrefix(s.cfg.PublicURL, "https://"), SameSite: http.SameSiteStrictMode})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"token": token})
	}
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	token, _ := sessionToken(r)
	if err := s.auth.Logout(r.Context(), token); err != nil {
		s.internal(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: strings.HasPrefix(s.cfg.PublicURL, "https://"), SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	ds, err := s.drive.Status(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"uptime_seconds": int(time.Since(s.started).Seconds()),
		"drive":          ds,
		"aria2_ready":    s.aria2 != nil && s.aria2.Ready(),
	})
}
