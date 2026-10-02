package api

import (
	"context"
	"errors"
	"fmt"
	"github.com/HHim8826/kanade/server/internal/drivesync"
	"html"
	"io"
	"net/http"
	"time"

	"github.com/HHim8826/kanade/server/internal/gdrive"
)

func (s *Server) driveInfo(w http.ResponseWriter, r *http.Request) {
	st, err := s.drive.Status(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := map[string]any{"status": st}
	if st.Connected {
		about, err := s.drive.About(r.Context())
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		root, err := s.drive.Root(r.Context())
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		out["account"] = about
		out["root_folder_id"] = root
	}
	writeJSON(w, http.StatusOK, out)
}

// driveSetClient stores the OAuth client JSON downloaded from Google Cloud.
func (s *Server) driveSetClient(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.drive.SetClientConfig(r.Context(), raw); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) driveAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LoginHint string `json:"login_hint"`
	}
	if r.ContentLength > 0 {
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	u, err := s.drive.AuthURL(r.Context(), req.LoginHint)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": u})
}

func (s *Server) driveAuthPaste(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	st, err := s.drive.CompletePasted(r.Context(), req.URL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// oauthCallback is public: Google redirects the administrator's browser here.
// It only succeeds with a state that an authenticated /drive/auth call created.
func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request) {
	st, err := s.drive.Complete(r.Context(), r.URL.Query())
	title, msg, code := "授權完成", "Kanade 已連線 Google Drive，可以關閉這個頁面。", http.StatusOK
	if err != nil {
		title, msg, code = "授權未完成", err.Error(), http.StatusBadRequest
		if errors.Is(err, gdrive.ErrBadState) {
			code = http.StatusForbidden
		}
		s.log.Warn("oauth callback rejected", "err", err)
	} else if st.TestingMode {
		msg += " 注意：應用程式仍在「測試中」，授權 7 天後會失效。"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>%[1]s</title><body style="font:16px/1.6 system-ui,sans-serif;max-width:32rem;margin:3rem auto;padding:0 1rem">
<h1>%[1]s</h1><p>%[2]s</p></body>`, html.EscapeString(title), html.EscapeString(msg))
}

// ---- reconciling with Drive and the inbox (P2-6) ----

func (s *Server) driveSync(w http.ResponseWriter, r *http.Request) {
	if s.sync == nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	writeJSON(w, http.StatusOK, s.sync.Status())
}

// driveReconcile starts a full check of every library file against Drive; it runs in the
// background and its result shows in the sync status.
func (s *Server) driveReconcile(w http.ResponseWriter, r *http.Request) {
	if s.sync == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("drive sync is not running"))
		return
	}
	if s.sync.Status().FullRunning {
		writeError(w, http.StatusConflict, drivesync.ErrRunning)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		if _, err := s.sync.Full(ctx); err != nil {
			s.log.Warn("drive full reconcile", "err", err)
		}
	}()
	w.WriteHeader(http.StatusAccepted)
}

// driveInbox looks in the inbox folder now instead of waiting for the next round.
func (s *Server) driveInbox(w http.ResponseWriter, r *http.Request) {
	n, err := s.importer.ScanInbox(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"files": n})
}

func (s *Server) missing(w http.ResponseWriter, r *http.Request) {
	list, err := s.lib.Missing(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}
