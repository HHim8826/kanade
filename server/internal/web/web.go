// Package web serves the browser client (decision D8) from files embedded in the binary.
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"html"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed static
var files embed.FS

type asset struct {
	body        []byte
	etag, ctype string
}

// csp allows only same-origin scripts, styles and media: no inline code, no third parties. Media
// made by the page itself (blob:) plays too: the player's silent sound on Android.
const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; " +
	"media-src 'self' blob:; connect-src 'self'; manifest-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'"

// Site is the name and icon the owner gave the service (internal/site).
type Site interface {
	Name() string
	Icon() string // the uploaded icon's version; "" for the built-in icon
	IconFile(name string) ([]byte, bool)
}

// Handler serves /app/... . Static files are addressed as /app/v/<version>/<path>, where the
// version is a hash of every embedded file: Cloudflare rewrites Cache-Control on .js/.css to
// an hour of browser caching, so only a changing URL makes browsers pick up a new build.
// index.html (whose no-cache Cloudflare respects) names the current version. Unknown paths
// without a file extension return the app shell, so reloading a client-side route works.
//
// The shell and the manifest name the site as it is named now, and point to its icon: an uploaded
// one is served from /app/site/<its version>/.
func Handler(site Site) http.Handler {
	static, _ := fs.Sub(files, "static")
	return handlerFor(static, site)
}

func handlerFor(static fs.FS, site Site) http.Handler {
	assets := map[string]asset{}
	all := sha256.New()
	fs.WalkDir(static, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(static, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		all.Write([]byte(p))
		all.Write(sum[:])
		assets[p] = asset{body: body, etag: `"` + hex.EncodeToString(sum[:8]) + `"`, ctype: mime.TypeByExtension(path.Ext(p))}
		return nil
	})
	version := "v/" + hex.EncodeToString(all.Sum(nil)[:6])
	shell := strings.ReplaceAll(string(assets["index.html"].body), "{{V}}", version)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/app/")
		switch {
		case p == "manifest.webmanifest":
			serve(w, r, made(manifest(site, version), "application/manifest+json"), false)
			return
		case strings.HasPrefix(p, "site/"):
			siteIcon(w, r, site, strings.TrimPrefix(p, "site/"))
			return
		}
		versioned := false
		if rest, ok := strings.CutPrefix(p, "v/"); ok {
			if _, after, ok := strings.Cut(rest, "/"); ok {
				p, versioned = after, true
			}
		}
		a, ok := assets[p]
		if p == "" || p == "index.html" || !ok && path.Ext(p) == "" {
			// The validator is of the shell as sent, which names the version and the site: a
			// release that changes only scripts or styles still changes it (review #11).
			serve(w, r, made([]byte(named(shell, site, version)), "text/html; charset=utf-8"), false)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		serve(w, r, a, versioned)
	})
}

func made(body []byte, ctype string) asset {
	sum := sha256.Sum256(body)
	return asset{body: body, etag: `"` + hex.EncodeToString(sum[:8]) + `"`, ctype: ctype}
}

func serve(w http.ResponseWriter, r *http.Request, a asset, immutable bool) {
	h := w.Header()
	h.Set("Content-Type", a.ctype)
	h.Set("ETag", a.etag)
	if immutable {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	if r.Header.Get("If-None-Match") == a.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Write(a.body)
}

// named fills the shell in with the site's name and icon. The page reads them back from its
// application-name meta (the login page shows them before anything is asked of the server).
func named(shell string, site Site, version string) string {
	icon := site.Icon()
	links := `<link rel="icon" href="` + version + `/icon.svg" type="image/svg+xml">
<link rel="apple-touch-icon" href="` + version + `/icon-180.png">`
	if icon != "" {
		links = `<link rel="icon" href="site/` + icon + `/icon-32.png" sizes="32x32" type="image/png">
<link rel="icon" href="site/` + icon + `/icon-192.png" sizes="192x192" type="image/png">
<link rel="apple-touch-icon" href="site/` + icon + `/icon-180.png">`
	}
	return strings.NewReplacer("{{NAME}}", html.EscapeString(site.Name()), "{{ICON}}", icon, "{{ICONS}}", links).Replace(shell)
}

type manifestIcon struct {
	Src     string `json:"src"`
	Sizes   string `json:"sizes"`
	Type    string `json:"type"`
	Purpose string `json:"purpose"`
}

// manifest is the installed app's: named as the site, with its icon.
func manifest(site Site, version string) []byte {
	icons := []manifestIcon{{Src: version + "/icon.svg", Sizes: "any", Type: "image/svg+xml", Purpose: "any maskable"}}
	if v := site.Icon(); v != "" {
		at := "site/" + v + "/"
		icons = []manifestIcon{{at + "icon-192.png", "192x192", "image/png", "any"}, {at + "icon-512.png", "512x512", "image/png", "any"},
			{at + "icon-maskable.png", "512x512", "image/png", "maskable"}}
	}
	b, _ := json.MarshalIndent(map[string]any{
		"name": site.Name(), "short_name": site.Name(), "start_url": "/app/", "scope": "/app/", "display": "standalone",
		"background_color": "#121318", "theme_color": "#4355b9", "icons": icons,
	}, "", "  ")
	return b
}

// siteIcon serves a file of the uploaded icon, asked for as <version>/<file>. One of an icon
// since replaced is the current one's, for pages still naming the old: not kept long.
func siteIcon(w http.ResponseWriter, r *http.Request, site Site, p string) {
	v, name, _ := strings.Cut(p, "/")
	b, ok := site.IconFile(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	serve(w, r, made(b, "image/png"), v == site.Icon())
}
