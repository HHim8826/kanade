package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/HHim8826/kanade/server/internal/auth"
	"github.com/HHim8826/kanade/server/internal/config"
	"github.com/HHim8826/kanade/server/internal/db"
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

// A backup is a whole copy of the database, made while it is open, private to the owner; an
// existing file is not overwritten.
func TestBackupCopiesTheDatabase(t *testing.T) {
	ctx := context.Background()
	cfg := config.Config{DataDir: filepath.Join(t.TempDir(), "data")}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(ctx, cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := auth.New(d).CreateUser(ctx, "admin", "correct horse battery", false); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "backup.sqlite")
	if err := backupCmd(ctx, cfg, []string{dst}); err != nil {
		t.Fatal(err)
	}
	if err := backupCmd(ctx, cfg, []string{dst}); err == nil {
		t.Fatal("overwrote an existing file")
	}
	if st, _ := os.Stat(dst); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	b, err := db.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var n int
	if b.QueryRow(`SELECT count(*) FROM users`).Scan(&n); n != 1 {
		t.Fatalf("users in the copy: %d", n)
	}
}
