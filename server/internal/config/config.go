// Package config holds the service settings and the data directory layout.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/HHim8826/kanade/server/internal/clientip"
)

type Config struct {
	DataDir   string // everything the service writes locally lives here
	Listen    string // e.g. 127.0.0.1:8080; the tunnel or a TLS front end faces the internet
	PublicURL string // e.g. https://music.example.com; used for the OAuth redirect URI and passkeys
	Aria2Path string // path to the aria2c binary
	// TrustedProxy says whose forwarding headers tell a client's address (clientip.Parse).
	TrustedProxy string
}

// File is the settings kept in the data directory (config.json), so the service starts the same
// way whoever starts it: the install script writes it, `kanade config set` changes it, and the
// flags of `serve` override it for one run.
type File struct {
	Listen    string `json:"listen,omitempty"`
	PublicURL string `json:"public_url,omitempty"`
	Aria2     string `json:"aria2,omitempty"`
	// TrustedProxy: cloudflare, loopback, or the proxies' addresses (clientip.Parse).
	TrustedProxy string `json:"trusted_proxy,omitempty"`
}

func (c Config) filePath() string { return filepath.Join(c.DataDir, "config.json") }

// ReadFile reads the data directory's settings; none is an empty File.
func (c Config) ReadFile() (File, error) {
	var f File
	raw, err := os.ReadFile(c.filePath())
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, fmt.Errorf("%s: %w", c.filePath(), err)
	}
	return f, nil
}

// Apply puts the data directory's settings over c.
func (c *Config) Apply() error {
	f, err := c.ReadFile()
	if err != nil {
		return err
	}
	if f.Listen != "" {
		c.Listen = f.Listen
	}
	if f.PublicURL != "" {
		c.PublicURL = f.PublicURL
	}
	if f.Aria2 != "" {
		c.Aria2Path = f.Aria2
	}
	c.TrustedProxy = f.TrustedProxy
	return nil
}

// Set changes one setting in the data directory's file: listen, public_url, aria2 or
// trusted_proxy ("" removes it, back to the default).
func (c Config) Set(key, value string) error {
	f, err := c.ReadFile()
	if err != nil {
		return err
	}
	value = strings.TrimSpace(value)
	switch key {
	case "listen":
		if _, port, err := net.SplitHostPort(value); value != "" && (err != nil || port == "") {
			return fmt.Errorf("listen: want host:port, like 127.0.0.1:8080 or 0.0.0.0:8080")
		}
		f.Listen = value
	case "public_url":
		value = strings.TrimRight(value, "/")
		if u, err := url.Parse(value); value != "" && (err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "") {
			return fmt.Errorf("public_url: want the site's address, like https://music.example.com")
		}
		f.PublicURL = value
	case "aria2":
		f.Aria2 = value
	case "trusted_proxy":
		if _, err := clientip.Parse(value); err != nil {
			return err
		}
		f.TrustedProxy = value
	default:
		return fmt.Errorf("unknown setting %q: listen, public_url, aria2 or trusted_proxy", key)
	}
	if err := c.Prepare(); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(f, "", "  ")
	tmp := c.filePath() + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.filePath())
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
