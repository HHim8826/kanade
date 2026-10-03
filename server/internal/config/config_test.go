package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Settings in config.json go over the defaults; bad values are refused; "" goes back to the default.
func TestSettingsFile(t *testing.T) {
	c := Config{DataDir: filepath.Join(t.TempDir(), "data"), Listen: "127.0.0.1:8080", PublicURL: "http://localhost:8080"}
	for key, bad := range map[string]string{"listen": "8080", "public_url": "music.example.com", "nope": "x"} {
		if err := c.Set(key, bad); err == nil {
			t.Errorf("%s=%q accepted", key, bad)
		}
	}
	if err := c.Set("public_url", "https://music.example.com/"); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("listen", "0.0.0.0:9090"); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(c.DataDir, "config.json")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("config.json %v %v", st, err)
	}
	got := c
	if err := got.Apply(); err != nil || got.PublicURL != "https://music.example.com" || got.Listen != "0.0.0.0:9090" {
		t.Fatalf("applied %+v %v", got, err)
	}
	if err := c.Set("listen", ""); err != nil {
		t.Fatal(err)
	}
	got = c
	if got.Apply(); got.Listen != "127.0.0.1:8080" || got.PublicURL != "https://music.example.com" {
		t.Fatalf("after clearing listen: %+v", got)
	}
}
