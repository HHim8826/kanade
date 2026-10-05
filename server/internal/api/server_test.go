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
	"strings"
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

// A proxy's headers are believed only as trusted_proxy says (review #148; the cases are in
// clientip): by default the throttle sees the connection's address.
func TestClientIPFollowsTrustedProxy(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("CF-Connecting-IP", "203.0.113.9")
	r.RemoteAddr = "127.0.0.1:5000"
	s := New(Deps{Config: config.Config{}})
	if got := s.clientIP(r); got != "127.0.0.1" {
		t.Fatalf("no proxy set: %s", got)
	}
	s = New(Deps{Config: config.Config{TrustedProxy: "cloudflare"}})
	if got := s.clientIP(r); got != "203.0.113.9" {
		t.Fatalf("via the tunnel: %s", got)
	}
	r.RemoteAddr = "198.51.100.7:5000"
	if got := s.clientIP(r); got != "198.51.100.7" {
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

// A browser that logged in here before still logs in while someone guesses passwords from
// everywhere (review #182); others are told why they cannot.
func TestKnownBrowserLogsIn(t *testing.T) {
	s, h := newTestServer(t)
	s.auth.CreateUser(context.Background(), "admin", "correct horse battery", false)
	login := func(cookies []*http.Cookie) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"username": "admin", "password": "correct horse battery", "cookie": true})
		req := httptest.NewRequest("POST", "/api/v1/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(csrfHeader, "kanade")
		for _, c := range cookies {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	rec := login(nil)
	var known *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == knownCookie {
			known = c
		}
	}
	if rec.Code != 200 || known == nil || !known.HttpOnly {
		t.Fatalf("login: %d, known %+v", rec.Code, known)
	}
	for i := range 30 {
		s.auth.Login(context.Background(), "admin", "wrong password!", "", fmt.Sprintf("192.0.2.%d", i))
	}
	if rec := login(nil); rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), `"reason":"throttled_all"`) {
		t.Fatalf("an unknown browser: %d %s", rec.Code, rec.Body)
	}
	forged := *known
	forged.Value = strings.Replace(known.Value, ".", "1.", 1)
	if rec := login([]*http.Cookie{&forged}); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a forged mark: %d", rec.Code)
	}
	if rec := login([]*http.Cookie{known}); rec.Code != 200 {
		t.Fatalf("the known browser: %d %s", rec.Code, rec.Body)
	}
}

// The web player's preload of the next song is a preload (review #175): the cache keeps the song
// playing, and takes the next as next.
func TestPrefetchIsNoPlay(t *testing.T) {
	ctx := context.Background()
	s, h := newTestServer(t)
	token := loginToken(t, s, h)
	a, _ := s.lib.CreateAsset(ctx, library.Asset{SHA256: "b1", Size: 1 << 20, Format: "flac", Codec: "flac"})
	s.lib.MarkVerified(ctx, a.ID, "d-b")
	path := fmt.Sprintf("/api/v1/stream/%d/prefetch", a.ID)
	if rec := do(t, h, "POST", path, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: %d", rec.Code)
	}
	if rec := do(t, h, "POST", path, token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("prefetch: %d %s", rec.Code, rec.Body)
	}
	if cur, next := s.cache.Playing(); cur != "" || next != "d-b" {
		t.Fatalf("playing %q, next %q", cur, next)
	}
	if rec := do(t, h, "POST", "/api/v1/stream/999/prefetch", token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("no such song: %d", rec.Code)
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

func TestHomeAndPlaysEndpoints(t *testing.T) {
	s, h := newTestServer(t)
	token := loginToken(t, s, h)
	rec := do(t, h, "GET", "/api/v1/home", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("home: %d %s", rec.Code, rec.Body)
	}
	var home map[string]json.RawMessage
	json.Unmarshal(rec.Body.Bytes(), &home)
	for _, k := range []string{"continue", "recently_played", "recently_added", "spoken", "tasks", "attention"} {
		if _, ok := home[k]; !ok {
			t.Errorf("home has no %q", k)
		}
	}
	if r := do(t, h, "POST", "/api/v1/plays", token, map[string]any{"session": "s1", "asset_id": 42}); r.Code != http.StatusBadRequest {
		t.Fatalf("play for a missing asset: %d", r.Code)
	}
	if r := do(t, h, "GET", "/api/v1/albums/random", token, nil); r.Code != http.StatusNotFound {
		t.Fatalf("random album in an empty library: %d", r.Code)
	}
}

func TestEditUndoEndpoints(t *testing.T) {
	s, h := newTestServer(t)
	token := loginToken(t, s, h)
	ctx := context.Background()
	a, _ := s.lib.CreateAsset(ctx, library.Asset{SHA256: "aa", Size: 1, Format: "flac", Codec: "flac"})
	s.lib.MarkVerified(ctx, a.ID, "drive-aa")
	r, _ := s.lib.Publish(ctx, a.ID, library.EntryInput{Title: "Old", Artist: "A", Album: "Al", AlbumArtist: "A"})
	path := fmt.Sprintf("/api/v1/tracks/%d", r.TrackID)

	rec := do(t, h, "PATCH", path, token, map[string]any{"title": "New", "aliases": []string{"alias"}})
	var res struct{ Group int64 }
	json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != http.StatusOK || res.Group == 0 {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "PATCH", path, token, map[string]any{"title": ""}); rec.Code != http.StatusBadRequest {
		t.Fatalf("blank title: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/api/v1/edits", token, nil); rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("編輯歌曲「Old」")) {
		t.Fatalf("edits: %d %s", rec.Code, rec.Body)
	}
	undo := fmt.Sprintf("/api/v1/edits/%d/undo", res.Group)
	if rec := do(t, h, "POST", undo, token, nil); rec.Code != http.StatusOK {
		t.Fatalf("undo: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", undo, token, nil); rec.Code != http.StatusConflict {
		t.Fatalf("second undo: %d", rec.Code)
	}
	rec = do(t, h, "GET", path, token, nil)
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"title":"Old"`)) {
		t.Fatalf("track after undo: %s", rec.Body)
	}
	if rec := do(t, h, "GET", "/api/v1/artists/1", token, nil); !bytes.Contains(rec.Body.Bytes(), []byte(`"items":[`)) {
		t.Fatalf("artist: %s", rec.Body)
	}
	if rec := do(t, h, "POST", "/api/v1/albums/1/merge", token, map[string]int{"into": 1}); rec.Code != http.StatusBadRequest {
		t.Fatalf("self merge: %d", rec.Code)
	}
}

// The landing and privacy pages name the site they are served from.
func TestPagesNameTheSite(t *testing.T) {
	_, h := newTestServer(t) // public URL https://music.example
	for _, path := range []string{"/", "/privacy"} {
		body := do(t, h, "GET", path, "", nil).Body.String()
		if !strings.Contains(body, "music.example") || strings.Contains(body, "{{SITE}}") || strings.Contains(body, "ser1ka") {
			t.Fatalf("%s: %s", path, body)
		}
	}
}
