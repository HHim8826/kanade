// Package config holds the service settings and the data directory layout.
package config

import (
	"os"
	"path/filepath"
)

type Config struct {
	DataDir   string // everything the service writes locally lives here
	Listen    string // e.g. 127.0.0.1:8080; the tunnel or a TLS front end faces the internet
	PublicURL string // e.g. https://music.ser1ka.com; used for the OAuth redirect URI
	Aria2Path string // path to the aria2c binary
}

// Subdirectories of DataDir, see docs/backend-design.md.
const (
	DirStaging   = "staging"
	DirDownloads = "downloads"
	DirCache     = "cache"
	DirThumbs    = "thumbs"
	DirAria2     = "aria2"
	DirImports   = "imports" // drop folders here (e.g. over SSH) to import them
)

func (c Config) DBPath() string { return filepath.Join(c.DataDir, "db.sqlite") }
func (c Config) Path(sub ...string) string {
	return filepath.Join(append([]string{c.DataDir}, sub...)...)
}

// Prepare creates the data directory tree, private to the service user.
func (c Config) Prepare() error {
	for _, d := range []string{"", DirStaging, DirDownloads, DirCache, DirThumbs, DirAria2, DirImports} {
		if err := os.MkdirAll(c.Path(d), 0o700); err != nil {
			return err
		}
	}
	return os.Chmod(c.DataDir, 0o700)
}
