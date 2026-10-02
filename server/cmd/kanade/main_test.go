package main

import (
	"os"
	"path/filepath"
	"testing"
)

// aria2c installed in tools/aria2/ of a checkout anywhere is found from there (review #10).
func TestDefaultAria2FindsToolsDir(t *testing.T) {
	repo := t.TempDir()
	bin := filepath.Join(repo, "tools", "aria2", "aria2c")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	os.MkdirAll(filepath.Join(repo, "server"), 0o755)
	t.Setenv("KANADE_ARIA2", "")
	t.Setenv("PATH", "")
	t.Chdir(filepath.Join(repo, "server")) // like running ./bin/kanade from server/
	if got := defaultAria2(); got != bin {
		t.Fatalf("found %q, want %q", got, bin)
	}
	t.Setenv("KANADE_ARIA2", "/opt/aria2c")
	if got := defaultAria2(); got != "/opt/aria2c" {
		t.Fatalf("env ignored: %q", got)
	}
	t.Setenv("KANADE_ARIA2", "")
	t.Chdir(t.TempDir())
	if got := defaultAria2(); got != "" {
		t.Fatalf("found %q with none installed", got)
	}
}
