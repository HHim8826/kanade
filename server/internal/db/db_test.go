package db

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A database's schema is read without changing it, while it is open and after; one a newer Kanade
// upgraded is not opened, so that the program from before a failed update does not read tables it
// does not know (review #255).
func TestSchemaOfAndNewer(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	p := filepath.Join(dir, "db.sqlite")
	d, err := OpenVersion(ctx, p, 39)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := SchemaOf(ctx, p); err != nil || v != 39 {
		t.Fatalf("while open: %d %v", v, err)
	}
	d.Close()
	if v, err := SchemaOf(ctx, p); err != nil || v != 39 {
		t.Fatalf("closed: %d %v", v, err)
	}
	if d, err = Open(ctx, p); err != nil {
		t.Fatal(err)
	}
	if v, err := SchemaOf(ctx, p); err != nil || v != Latest() || Latest() < 47 {
		t.Fatalf("upgraded: %d %v, latest %d", v, err, Latest())
	}
	// A newer Kanade's migration.
	if _, err := d.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, 0)`, Latest()+1); err != nil {
		t.Fatal(err)
	}
	d.Close()
	if d, err := Open(ctx, p); !errors.Is(err, ErrNewerSchema) {
		if d != nil {
			d.Close()
		}
		t.Fatalf("a newer database opened: %v", err)
	}
	if v, err := SchemaOf(ctx, p); err != nil || v != Latest()+1 {
		t.Fatalf("newer: %d %v", v, err)
	}
	empty := filepath.Join(dir, "empty.sqlite")
	os.WriteFile(empty, nil, 0o600)
	if v, err := SchemaOf(ctx, empty); err != nil || v != 0 {
		t.Fatalf("empty: %d %v", v, err)
	}
	if _, err := SchemaOf(ctx, filepath.Join(dir, "none.sqlite")); err == nil {
		t.Fatal("no file: no error")
	}
	if _, err := os.Stat(filepath.Join(dir, "none.sqlite")); !os.IsNotExist(err) {
		t.Fatal("reading made a file")
	}
}
