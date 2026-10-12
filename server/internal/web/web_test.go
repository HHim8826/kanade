package web

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

// fakeSite is named and pictured as set; an icon's files are its version and name.
type fakeSite struct{ name, icon string }

func (f *fakeSite) Name() string { return f.name }
func (f *fakeSite) Icon() string { return f.icon }
func (f *fakeSite) IconFile(name string) ([]byte, bool) {
	if f.icon == "" {
		return nil, false
	}
	return []byte(f.icon + " " + name), true
}

func TestVersionedAssets(t *testing.T) {
	h := Handler(&fakeSite{name: "Kanade"})
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
	site := &fakeSite{name: "Kanade"}
	before := handlerFor(fstest.MapFS{"index.html": {Data: shell}, "js/app.js": {Data: []byte("1")}}, site)
	after := handlerFor(fstest.MapFS{"index.html": {Data: shell}, "js/app.js": {Data: []byte("1 // changed")}}, site)
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

// The shell and the manifest name the site as it is named now and point to its icon; a renamed
// site gives the shell a new validator.
func TestSite(t *testing.T) {
	site := &fakeSite{name: "Kanade"}
	h := Handler(site)
	get := func(path, etag string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	shell := get("/app/", "")
	body := shell.Body.String()
	for _, want := range []string{"<title>Kanade</title>", `<meta name="application-name" content="Kanade" data-icon="">`,
		`/icon.svg" type="image/svg+xml">`, `/icon-180.png">`, `<link rel="manifest" href="manifest.webmanifest">`} {
		if !strings.Contains(body, want) {
			t.Fatalf("built-in: no %s in\n%s", want, body)
		}
	}
	m := regexp.MustCompile(`href="(v/[0-9a-f]+)/icon-180.png"`).FindStringSubmatch(body)
	if m == nil || get("/app/"+m[1]+"/icon-180.png", "").Code != 200 {
		t.Fatalf("the built-in iPhone icon: %v", m)
	}
	man := get("/app/manifest.webmanifest", "")
	if man.Code != 200 || man.Header().Get("Content-Type") != "application/manifest+json" || man.Header().Get("Cache-Control") != "no-cache" ||
		!strings.Contains(man.Body.String(), `"name": "Kanade"`) || !strings.Contains(man.Body.String(), `"src": "`+m[1]+`/icon.svg"`) {
		t.Fatalf("manifest: %d %v\n%s", man.Code, man.Header(), man.Body)
	}
	if rec := get("/app/site/abc/icon-32.png", ""); rec.Code != 404 {
		t.Fatalf("an icon file without an icon: %d", rec.Code)
	}

	site.name, site.icon = `<奏> & "音"`, "0123456789ab"
	again := get("/app/", shell.Header().Get("ETag"))
	body = again.Body.String()
	if again.Code != 200 || again.Header().Get("ETag") == shell.Header().Get("ETag") {
		t.Fatalf("renamed: %d %s", again.Code, again.Header().Get("ETag"))
	}
	for _, want := range []string{"<title>&lt;奏&gt; &amp; &#34;音&#34;</title>", `content="&lt;奏&gt; &amp; &#34;音&#34;" data-icon="0123456789ab">`,
		`<link rel="icon" href="site/0123456789ab/icon-32.png" sizes="32x32" type="image/png">`,
		`<link rel="apple-touch-icon" href="site/0123456789ab/icon-180.png">`} {
		if !strings.Contains(body, want) {
			t.Fatalf("named: no %s in\n%s", want, body)
		}
	}
	if strings.Contains(body, "icon.svg") {
		t.Fatal("the built-in icon is still named")
	}
	if rec := get("/app/library", ""); !strings.Contains(rec.Body.String(), "&lt;奏&gt;") {
		t.Fatal("a client route's shell is not named")
	}
	man = get("/app/manifest.webmanifest", "")
	for _, want := range []string{`"short_name": "\u003c奏\u003e \u0026 \"音\""`, `"src": "site/0123456789ab/icon-512.png"`,
		`"src": "site/0123456789ab/icon-maskable.png"`, `"purpose": "maskable"`} {
		if !strings.Contains(man.Body.String(), want) {
			t.Fatalf("manifest: no %s in\n%s", want, man.Body)
		}
	}
	icon := get("/app/site/0123456789ab/icon-192.png", "")
	if icon.Code != 200 || icon.Body.String() != "0123456789ab icon-192.png" || icon.Header().Get("Content-Type") != "image/png" ||
		!strings.Contains(icon.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("icon: %d %q %v", icon.Code, icon.Body, icon.Header())
	}
	// A page naming the icon replaced gets the current one, not kept.
	if old := get("/app/site/fedcba987654/icon-192.png", ""); old.Code != 200 || old.Body.String() != "0123456789ab icon-192.png" ||
		old.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("an old icon: %d %q %v", old.Code, old.Body, old.Header())
	}
}
