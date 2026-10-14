package library

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Relocate: the library reads a file elsewhere, the old copy owed to the trash after a while; a copy
// owed to the trash is the library's again; one of a file let go of meanwhile is owed instead; a
// Drive file that just went to the trash is refused, a store's key that did is free again.
func TestRelocate(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	id := verifiedAsset(t, s, "aa")
	owed := func(ref string) (bool, int64) {
		var next int64
		err := s.db.QueryRow(`SELECT next_at FROM file_trash WHERE file_id = ? AND done_at = 0`, ref).Scan(&next)
		return err == nil, next
	}
	ref := func() (r string) {
		s.db.QueryRow(`SELECT file_ref FROM assets WHERE id = ?`, id).Scan(&r)
		return
	}
	s.db.Exec(`INSERT INTO file_trash (file_id, created_at) VALUES ('local:1:a.flac', 0)`) // moved away before
	if moved, err := s.Relocate(ctx, "drive-aa", "local:1:a.flac", time.Hour); err != nil || !moved || ref() != "local:1:a.flac" {
		t.Fatalf("moved %v %v: at %s", moved, err, ref())
	}
	if o, next := owed("drive-aa"); !o || next < time.Now().Add(50*time.Minute).UnixMilli() {
		t.Fatalf("the old copy owed %v at %d", o, next)
	}
	if o, _ := owed("local:1:a.flac"); o {
		t.Fatal("the library's copy owed to the trash")
	}
	// Moved by another meanwhile: the copy just made goes.
	if moved, err := s.Relocate(ctx, "drive-aa", "s3:2:a.flac", 0); err != nil || moved {
		t.Fatalf("moved %v %v", moved, err)
	}
	if o, _ := owed("s3:2:a.flac"); !o {
		t.Fatal("the copy of a file let go of is not owed to the trash")
	}
	s.db.Exec(`INSERT INTO file_trash (file_id, created_at, done_at) VALUES ('drive-new', 0, ?1), ('s3:2:b.flac', 0, ?1)`, time.Now().UnixMilli())
	if _, err := s.Relocate(ctx, "local:1:a.flac", "drive-new", 0); !errors.Is(err, ErrInTrash) {
		t.Fatal(err)
	}
	if moved, err := s.Relocate(ctx, "local:1:a.flac", "s3:2:b.flac", 0); err != nil || !moved || ref() != "s3:2:b.flac" {
		t.Fatalf("to a store's key that went to the trash: %v %v", moved, err)
	}
}
