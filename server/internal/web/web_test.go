package web

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
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
