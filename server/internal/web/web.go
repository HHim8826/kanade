// Package web serves the browser client (decision D8) from files embedded in the binary.
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
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

// Handler serves /app/... . Static files are addressed as /app/v/<version>/<path>, where the
// version is a hash of every embedded file: Cloudflare rewrites Cache-Control on .js/.css to
// an hour of browser caching, so only a changing URL makes browsers pick up a new build.
// index.html (whose no-cache Cloudflare respects) names the current version. Unknown paths
// without a file extension return the app shell, so reloading a client-side route works.
func Handler() http.Handler {
	static, _ := fs.Sub(files, "static")
	return handlerFor(static)
}

func handlerFor(static fs.FS) http.Handler {
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
		ctype := mime.TypeByExtension(path.Ext(p))
		if path.Ext(p) == ".webmanifest" {
			ctype = "application/manifest+json"
		}
		assets[p] = asset{body: body, etag: `"` + hex.EncodeToString(sum[:8]) + `"`, ctype: ctype}
		return nil
	})
	version := hex.EncodeToString(all.Sum(nil)[:6])
	shell := assets["index.html"]
	shell.body = []byte(strings.ReplaceAll(string(shell.body), "{{V}}", "v/"+version))
	// The validator is of the shell as sent, which names the version: a release that changes only
	// scripts or styles still changes it (review #11).
	sum := sha256.Sum256(shell.body)
	shell.etag = `"` + hex.EncodeToString(sum[:8]) + `"`
	assets["index.html"] = shell

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/app/")
		versioned := false
		if rest, ok := strings.CutPrefix(p, "v/"); ok {
			if _, after, ok := strings.Cut(rest, "/"); ok {
				p, versioned = after, true
			}
		}
		if p == "" || p == "index.html" {
			p, versioned = "index.html", false
		}
		a, ok := assets[p]
		if !ok {
			if path.Ext(p) != "" {
				http.NotFound(w, r)
				return
			}
			a, versioned = assets["index.html"], false
		}
		h := w.Header()
		h.Set("Content-Type", a.ctype)
		h.Set("ETag", a.etag)
		if versioned {
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
	})
}
