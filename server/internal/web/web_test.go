package web

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

func TestVersionedAssets(t *testing.T) {
	h := Handler()
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}
	shell := get("/app/")
	if shell.Code != 200 || shell.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("shell: %d %q", shell.Code, shell.Header().Get("Cache-Control"))
	}
	m := regexp.MustCompile(`src="(v/[0-9a-f]+)/js/app.js"`).FindStringSubmatch(shell.Body.String())
	if m == nil || strings.Contains(shell.Body.String(), "{{V}}") {
		t.Fatalf("shell does not reference a versioned app.js:\n%s", shell.Body)
	}
	js := get("/app/" + m[1] + "/js/app.js")
	if js.Code != 200 || !strings.Contains(js.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("versioned js: %d %q", js.Code, js.Header().Get("Cache-Control"))
	}
	if !strings.Contains(js.Header().Get("Content-Type"), "javascript") || js.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("headers: %v", js.Header())
	}
	if rec := get("/app/" + m[1] + "/js/missing.js"); rec.Code != 404 {
		t.Fatalf("missing module: %d", rec.Code)
	}
	if rec := get("/app/library/albums"); rec.Code != 200 || !strings.Contains(rec.Body.String(), m[1]) {
		t.Fatalf("client route should get the shell: %d", rec.Code)
	}
}

// A release that changes only a script gives the shell a new validator, so a client holding the
// old one gets the new shell instead of 304 (review #11).
func TestShellETagFollowsVersion(t *testing.T) {
	shell := []byte(`<script type="module" src="/app/{{V}}/js/app.js"></script>`)
	before := handlerFor(fstest.MapFS{"index.html": {Data: shell}, "js/app.js": {Data: []byte("1")}})
	after := handlerFor(fstest.MapFS{"index.html": {Data: shell}, "js/app.js": {Data: []byte("1 // changed")}})
	get := func(h http.Handler, etag string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/app/", nil)
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	old := get(before, "")
	fresh := get(after, old.Header().Get("ETag"))
	if fresh.Code != http.StatusOK || fresh.Header().Get("ETag") == old.Header().Get("ETag") || fresh.Body.String() == old.Body.String() {
		t.Fatalf("old validator: %d %s", fresh.Code, fresh.Header().Get("ETag"))
	}
	if again := get(after, fresh.Header().Get("ETag")); again.Code != http.StatusNotModified {
		t.Fatalf("same version: %d", again.Code)
	}
	for _, p := range []string{"/app/index.html", "/app/library"} {
		req := httptest.NewRequest("GET", p, nil)
		req.Header.Set("If-None-Match", old.Header().Get("ETag"))
		rec := httptest.NewRecorder()
		after.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s with the old validator: %d", p, rec.Code)
		}
	}
}
