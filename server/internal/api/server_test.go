package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/HHim8826/kanade/server/internal/auth"
	"github.com/HHim8826/kanade/server/internal/config"
	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/importer"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/stream"
	"github.com/HHim8826/kanade/server/internal/uploads"
)

func newTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	cfg := config.Config{DataDir: t.TempDir(), PublicURL: "https://music.example"}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(context.Background(), cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	drive := gdrive.New(d, cfg.PublicURL+"/oauth/google/callback")
	lib := library.New(d)
	cache, err := stream.NewCache(drive, cfg.Path(config.DirCache), 64<<20, log)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Deps{Config: cfg, DB: d, Auth: auth.New(d), Drive: drive, Library: lib,
		Importer: importer.New(d, lib, drive, cfg.Path(config.DirStaging), log), Cache: cache,
		Uploads: uploads.New(d, cfg.Path(config.DirStaging, "uploads"), 1<<30), StreamKey: []byte("test-key"), Log: log})
	return s, s.Handler()
}

func do(t *testing.T, h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSetupNeedsCodeThenLoginWorks(t *testing.T) {
	s, h := newTestServer(t)
	if err := os.WriteFile(s.SetupCodePath(), []byte("abc123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	creds := map[string]string{"username": "admin", "password": "correct horse battery"}

	bad := map[string]string{"setup_code": "nope", "username": "admin", "password": "correct horse battery"}
	if rec := do(t, h, "POST", "/api/v1/setup", "", bad); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong code: %d", rec.Code)
	}
	good := map[string]string{"setup_code": "abc123", "username": "admin", "password": "correct horse battery"}
	if rec := do(t, h, "POST", "/api/v1/setup", "", good); rec.Code != http.StatusCreated {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(s.SetupCodePath()); !os.IsNotExist(err) {
		t.Fatal("setup code file should be removed after setup")
	}

	if rec := do(t, h, "GET", "/api/v1/status", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status without login: %d", rec.Code)
	}
	rec := do(t, h, "POST", "/api/v1/login", "", creds)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	var lr struct{ Token string }
	json.Unmarshal(rec.Body.Bytes(), &lr)
	if rec := do(t, h, "GET", "/api/v1/status", lr.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("status with login: %d %s", rec.Code, rec.Body)
	}
}

func TestPublicPagesAndForgedCallback(t *testing.T) {
	_, h := newTestServer(t)
	for _, p := range []string{"/", "/privacy"} {
		if rec := do(t, h, "GET", p, "", nil); rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("Kanade")) {
			t.Fatalf("%s: %d", p, rec.Code)
		}
	}
	if rec := do(t, h, "GET", "/oauth/google/callback?state=forged&code=x", "", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("forged callback: %d", rec.Code)
	}
}

func TestClientIPTrustsProxyHeadersOnlyFromLoopback(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("CF-Connecting-IP", "203.0.113.9")
	r.RemoteAddr = "127.0.0.1:5000"
	if got := clientIP(r); got != "203.0.113.9" {
		t.Fatalf("via tunnel: %s", got)
	}
	r.RemoteAddr = "198.51.100.7:5000"
	if got := clientIP(r); got != "198.51.100.7" {
		t.Fatalf("direct client spoofing the header: %s", got)
	}
}

func loginToken(t *testing.T, s *Server, h http.Handler) string {
	t.Helper()
	if err := s.auth.CreateUser(context.Background(), "admin", "correct horse battery", false); err != nil {
		t.Fatal(err)
	}
	rec := do(t, h, "POST", "/api/v1/login", "", map[string]string{"username": "admin", "password": "correct horse battery"})
	var lr struct{ Token string }
	json.Unmarshal(rec.Body.Bytes(), &lr)
	return lr.Token
}

func TestStreamRequiresTokenOrValidSignature(t *testing.T) {
	s, h := newTestServer(t)
	token := loginToken(t, s, h)
	if rec := do(t, h, "GET", "/api/v1/stream/1", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/api/v1/stream/1?exp=9999999999&sig=forged", "", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("forged signature: %d", rec.Code)
	}
	rec := do(t, h, "POST", "/api/v1/stream/1/url", token, nil)
	var u struct{ URL string }
	json.Unmarshal(rec.Body.Bytes(), &u)
	path := u.URL[len("https://music.example"):]
	// A valid signature gets past auth; asset 1 does not exist, so 404.
	if rec := do(t, h, "GET", path, "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("signed URL: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", path[:len(path)-4]+"AAAA", "", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("tampered signature: %d", rec.Code)
	}
	expired := fmt.Sprintf("/api/v1/stream/1?exp=1&sig=%s", s.streamSig(1, 1))
	if rec := do(t, h, "GET", expired, "", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("expired signature: %d", rec.Code)
	}
}

func TestImportPathMustStayInsideRoots(t *testing.T) {
	s, h := newTestServer(t)
	token := loginToken(t, s, h)
	os.MkdirAll(s.cfg.Path("imports", "album"), 0o755)
	for _, p := range []string{"/etc", "../../", s.cfg.DataDir} {
		if rec := do(t, h, "POST", "/api/v1/imports", token, map[string]string{"path": p}); rec.Code != http.StatusForbidden {
			t.Fatalf("%q: %d %s", p, rec.Code, rec.Body)
		}
	}
	// Inside the imports directory but empty: allowed, then rejected for having no audio.
	if rec := do(t, h, "POST", "/api/v1/imports", token, map[string]string{"path": "album"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty album dir: %d %s", rec.Code, rec.Body)
	}
}

func TestUploadOverHTTPThenImportGroup(t *testing.T) {
	s, h := newTestServer(t)
	token := loginToken(t, s, h)
	audio, err := os.ReadFile("../media/testdata/tone.ogg")
	if err != nil {
		t.Fatal(err)
	}
	rec := do(t, h, "POST", "/api/v1/uploads", token, map[string]any{"group": "phone-abcdef01", "path": "Album/01.ogg", "size": len(audio)})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var cr struct {
		Upload struct{ ID int64 }
	}
	json.Unmarshal(rec.Body.Bytes(), &cr)
	if r := do(t, h, "POST", "/api/v1/imports", token, map[string]string{"upload_group": "phone-abcdef01"}); r.Code != http.StatusConflict {
		t.Fatalf("import before upload finished: %d", r.Code)
	}
	put := func(offset int, chunk []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", fmt.Sprintf("/api/v1/uploads/%d?offset=%d", cr.Upload.ID, offset), bytes.NewReader(chunk))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	half := len(audio) / 2
	if r := put(0, audio[:half]); r.Code != http.StatusOK {
		t.Fatalf("chunk 1: %d %s", r.Code, r.Body)
	}
	if r := put(0, audio[:half]); r.Code != http.StatusConflict || !bytes.Contains(r.Body.Bytes(), []byte(fmt.Sprintf(`"received":%d`, half))) {
		t.Fatalf("repeated chunk should say where to resume: %d %s", r.Code, r.Body)
	}
	if r := put(half, audio[half:]); r.Code != http.StatusOK {
		t.Fatalf("chunk 2: %d %s", r.Code, r.Body)
	}
	if r := do(t, h, "POST", fmt.Sprintf("/api/v1/uploads/%d/complete", cr.Upload.ID), token, nil); r.Code != http.StatusOK {
		t.Fatalf("complete: %d %s", r.Code, r.Body)
	}
	if r := do(t, h, "POST", "/api/v1/imports", token, map[string]string{"upload_group": "phone-abcdef01"}); r.Code != http.StatusCreated {
		t.Fatalf("import: %d %s", r.Code, r.Body)
	}
}
