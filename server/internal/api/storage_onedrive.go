package api

import (
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/storage"
)

// Connecting a OneDrive store (#95): signing in, as with Google Drive (the callback, or the address
// the browser ends on pasted), then choosing its folder.

// oneDriveRedirect is where Microsoft sends the browser back to: this server's callback when it has
// https and a domain name, http://localhost otherwise (the address it ends on is then pasted).
func (s *Server) oneDriveRedirect() string {
	return gdrive.RedirectAt(s.cfg.PublicURL, storage.OneDriveCallbackPath)
}

// oneDrivePaste says whether the address a sign-in ends on is pasted: Microsoft cannot send the
// browser back here.
func (s *Server) oneDrivePaste() bool {
	return !strings.HasPrefix(s.oneDriveRedirect(), strings.TrimRight(s.cfg.PublicURL, "/")+"/")
}

// connectStore starts signing a OneDrive store in ({} → {url, redirect_uri, paste}), or finishes it
// with the address the browser ended on ({"url": ...}).
func (s *Server) connectStore(w http.ResponseWriter, r *http.Request) {
	st, ok := s.store(w, r)
	if !ok {
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if r.ContentLength > 0 {
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	ctx := r.Context()
	redirect := s.oneDriveRedirect()
	if req.URL == "" {
		u, err := s.files.OneDriveAuthURL(ctx, st.ID, redirect)
		if err != nil {
			s.storeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"url": u, "redirect_uri": redirect, "paste": s.oneDrivePaste()})
		return
	}
	u, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil || u.Query().Get("state") == "" {
		writeError(w, http.StatusBadRequest, errors.New("paste the whole address the browser ended on (it has code= and state=)"))
		return
	}
	q := u.Query()
	if !strings.HasPrefix(q.Get("state"), "onedrive."+strconv.FormatInt(st.ID, 10)+".") {
		writeError(w, http.StatusBadRequest, storage.ErrBadState)
		return
	}
	done, err := s.files.OneDriveComplete(ctx, q, redirect)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	info, err := s.storeInfo(ctx, *done)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// oneDriveCallback is public: Microsoft sends the administrator's browser here. It only succeeds
// with a state an authenticated connect made.
func (s *Server) oneDriveCallback(w http.ResponseWriter, r *http.Request) {
	st, err := s.files.OneDriveComplete(r.Context(), r.URL.Query(), s.oneDriveRedirect())
	title, msg, code := "授權完成", "", http.StatusOK
	if err != nil {
		title, msg, code = "授權未完成", err.Error(), http.StatusBadRequest
		if errors.Is(err, storage.ErrBadState) {
			code = http.StatusForbidden
		}
		s.log.Warn("onedrive callback rejected", "err", err)
	} else {
		msg = fmt.Sprintf("%s 已連線 OneDrive（%s），可以關閉這個頁面，回到設定頁選擇資料夾。", s.site.Name(), st.Name)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>%[1]s</title><body style="font:16px/1.6 system-ui,sans-serif;max-width:32rem;margin:3rem auto;padding:0 1rem">
<h1>%[1]s</h1><p>%[2]s</p></body>`, html.EscapeString(title), html.EscapeString(msg))
}

// storeFolders lists a OneDrive store's folders below ?parent= (its drive's root without).
func (s *Server) storeFolders(w http.ResponseWriter, r *http.Request) {
	st, ok := s.store(w, r)
	if !ok {
		return
	}
	here, list, err := s.files.OneDriveFolders(r.Context(), st.ID, r.URL.Query().Get("parent"))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	if list == nil {
		list = []storage.Folder{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"here": here, "folders": list})
}

// makeStoreFolder makes a folder in a managed OneDrive store's drive.
func (s *Server) makeStoreFolder(w http.ResponseWriter, r *http.Request) {
	st, ok := s.store(w, r)
	if !ok {
		return
	}
	var req struct {
		Parent string `json:"parent"`
		Name   string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	f, err := s.files.OneDriveMakeFolder(r.Context(), st.ID, req.Parent, req.Name)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

// setStoreFolder chooses a OneDrive store's folder; not while the library has files in the one
// chosen before (moving them is a feature of its own).
func (s *Server) setStoreFolder(w http.ResponseWriter, r *http.Request, st *storage.Store, folder string) bool {
	ctx := r.Context()
	if s.busy(st.ID) {
		writeError(w, http.StatusConflict, errors.New("this storage location is being scanned or checked"))
		return false
	}
	use, err := s.lib.StoreUse(ctx, st.Kind, st.ID)
	if err != nil {
		s.internal(w, r, err)
		return false
	}
	if use.Songs+use.Others > 0 {
		s.storeError(w, r, fmt.Errorf("%w: %d songs and %d covers or sidecars are in the folder chosen before", library.ErrStoreInUse, use.Songs, use.Others))
		return false
	}
	if _, err := s.files.SetFolder(ctx, st.ID, folder); err != nil {
		s.storeError(w, r, err)
		return false
	}
	return true
}
