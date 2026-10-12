package api

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/HHim8826/kanade/server/internal/site"
)

// Renaming the site and giving it an icon: the shell, the manifest, the public pages and new
// passkeys follow; a bad name or image is refused with a reason the page words.
func TestSite(t *testing.T) {
	s, h := newTestServer(t, func(d *Deps) {
		named, err := site.Open(context.Background(), d.DB)
		if err != nil {
			t.Fatal(err)
		}
		d.Site = named
	})
	token := loginToken(t, s, h)
	info := func(rec *httptest.ResponseRecorder) (v struct{ Name, Default, Icon string }) {
		t.Helper()
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		return v
	}
	page := func(path string) string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Body.String()
	}
	if v := info(do(t, h, "GET", "/api/v1/site", token, nil)); v.Name != "Kanade" || v.Default != "Kanade" || v.Icon != "" {
		t.Fatalf("default: %+v", v)
	}
	if rec := do(t, h, "PUT", "/api/v1/site", "", map[string]string{"name": "x"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("not logged in: %d", rec.Code)
	}

	if v := info(do(t, h, "PUT", "/api/v1/site", token, map[string]string{"name": " 奏の音楽室 "})); v.Name != "奏の音楽室" {
		t.Fatalf("renamed: %+v", v)
	}
	if rec := do(t, h, "PUT", "/api/v1/site", token, map[string]string{"name": strings.Repeat("長", 33)}); rec.Code != 400 ||
		!strings.Contains(rec.Body.String(), `"reason":"name"`) {
		t.Fatalf("a long name: %d %s", rec.Code, rec.Body)
	}
	for path, want := range map[string]string{
		"/app/":                     "<title>奏の音楽室</title>",
		"/app/manifest.webmanifest": `"name": "奏の音楽室"`,
		"/":                         "<h1>奏の音楽室</h1>",
		"/privacy":                  "<title>Privacy Policy - 奏の音楽室</title>",
	} {
		if body := page(path); !strings.Contains(body, want) || strings.Contains(body, "{{") {
			t.Fatalf("%s: no %s in\n%s", path, want, body)
		}
	}
	rec := do(t, h, "POST", "/api/v1/passkeys/options", token, map[string]string{"password": "correct horse battery"})
	if !strings.Contains(rec.Body.String(), `"name":"奏の音楽室"`) {
		t.Fatalf("passkey options: %d %s", rec.Code, rec.Body)
	}

	img := image.NewRGBA(image.Rect(0, 0, 120, 80))
	for x := range 120 {
		for y := range 80 {
			img.Set(x, y, color.RGBA{200, 50, 50, 255})
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	put := func(body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/api/v1/site/icon", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "image/png")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	v := info(put(buf.Bytes()))
	if v.Icon == "" || v.Name != "奏の音楽室" {
		t.Fatalf("icon: %+v", v)
	}
	if !strings.Contains(page("/app/"), `href="site/`+v.Icon+`/icon-180.png"`) || !strings.Contains(page("/app/manifest.webmanifest"), "site/"+v.Icon+"/icon-maskable.png") {
		t.Fatal("the shell or manifest does not name the icon")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/app/site/"+v.Icon+"/icon-192.png", nil))
	if got, err := png.DecodeConfig(rec.Body); rec.Code != 200 || err != nil || got.Width != 192 {
		t.Fatalf("icon file: %d %v %+v", rec.Code, err, got)
	}
	if rec := put([]byte("GIF89a not this")); rec.Code != 400 || !strings.Contains(rec.Body.String(), `"reason":"format"`) {
		t.Fatalf("a GIF: %d %s", rec.Code, rec.Body)
	}
	if v := info(do(t, h, "DELETE", "/api/v1/site/icon", token, nil)); v.Icon != "" || !strings.Contains(page("/app/"), "icon.svg") {
		t.Fatalf("cleared: %+v", v)
	}
	if v := info(do(t, h, "PUT", "/api/v1/site", token, map[string]string{"name": ""})); v.Name != "Kanade" || !strings.Contains(page("/"), "<h1>Kanade</h1>") {
		t.Fatalf("back to the default: %+v", v)
	}
}
