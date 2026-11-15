package db

import (
	"context"
	"path/filepath"
	"testing"
)

// The planner's statistics are made for a table that has grown, at open and when asked again
// (review T3).
func TestOptimize(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "db.sqlite")
	d, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	stat := func() int {
		var n int
		if err := d.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name = 'sqlite_stat1'`).Scan(&n); err != nil || n == 0 {
			return 0
		}
		d.QueryRow(`SELECT count(*) FROM sqlite_stat1 WHERE tbl = 'settings'`).Scan(&n)
		return n
	}
	tx, _ := d.Begin()
	for i := 0; i < 2000; i++ {
		if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?, 'x')`, "k"+string(rune('a'+i%26))+string(rune(i))); err != nil {
			t.Fatal(err)
		}
	}
	tx.Commit()
	if err := Optimize(ctx, d); err != nil {
		t.Fatal(err)
	}
	if stat() == 0 {
		t.Fatal("no statistics for a table of 2000 rows")
	}
}
