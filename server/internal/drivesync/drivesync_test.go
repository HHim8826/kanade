package drivesync

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/library"
)

type fakeDrive struct {
	changes []gdrive.Change
	files   []string
	listErr error
}

func (f *fakeDrive) StartPageToken(context.Context) (string, error) { return "t1", nil }
func (f *fakeDrive) Changes(_ context.Context, token string) ([]gdrive.Change, string, error) {
	c := f.changes
	f.changes = nil
	return c, token + "+", nil
}
func (f *fakeDrive) List(_ context.Context, q string, fn func([]gdrive.File) error) error {
	var page []gdrive.File
	for _, id := range f.files {
		page = append(page, gdrive.File{ID: id})
	}
	if err := fn(page); err != nil {
		return err
	}
	return f.listErr // a failure after a first page
}

func TestChangesAndFullPass(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	lib := library.New(d)
	for _, id := range []string{"a", "b", "c"} {
		as, _ := lib.CreateAsset(ctx, library.Asset{SHA256: id, Size: 1, Format: "flac", Codec: "flac"})
		lib.MarkVerified(ctx, as.ID, "drive-"+id)
	}
	state := func(id string) string {
		var s string
		d.QueryRow(`SELECT state FROM assets WHERE drive_file_id = ?`, "drive-"+id).Scan(&s)
		return s
	}
	fd := &fakeDrive{}
	s := &Syncer{DB: d, Drive: fd, Lib: lib, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// The first look only takes the starting point.
	fd.changes = []gdrive.Change{{FileID: "drive-a", Removed: true}}
	if r, err := s.Changes(ctx); err != nil || r.Checked != 0 || state("a") != library.AssetVerified {
		t.Fatalf("first: %+v %v", r, err)
	}
	fd.changes = []gdrive.Change{{FileID: "drive-a", File: &gdrive.File{ID: "drive-a", Trashed: true}},
		{FileID: "drive-b", Removed: true}, {FileID: "other", Removed: true}}
	if r, err := s.Changes(ctx); err != nil || r.Missing != 2 || state("a") != library.AssetMissing || state("b") != library.AssetMissing {
		t.Fatalf("changes: %+v %v", r, err)
	}
	fd.changes = []gdrive.Change{{FileID: "drive-a", File: &gdrive.File{ID: "drive-a"}}} // restored from the trash
	if r, _ := s.Changes(ctx); r.Restored != 1 || state("a") != library.AssetVerified {
		t.Fatalf("restore: %+v", r)
	}
	if a, _ := lib.Attention(ctx); a.Missing != 1 {
		t.Fatalf("attention %+v", a)
	}

	// A listing that fails part way marks nothing.
	fd.files, fd.listErr = []string{"drive-a"}, errors.New("quota")
	if _, err := s.Full(ctx); err == nil || state("c") != library.AssetVerified {
		t.Fatalf("partial listing: %v %s", err, state("c"))
	}
	fd.listErr = nil
	fd.files = []string{"drive-a", "drive-b"}
	r, err := s.Full(ctx)
	if err != nil || r.Checked != 3 || r.Missing != 1 || r.Restored != 1 || state("b") != library.AssetVerified || state("c") != library.AssetMissing {
		t.Fatalf("full: %+v %v", r, err)
	}
	if st := s.Status(); st.LastFull == 0 || st.LastFullRes.Missing != 1 {
		t.Fatalf("status %+v", st)
	}
	if m, _ := lib.Missing(ctx); len(m) != 0 { // no track uses these bare assets
		t.Fatalf("missing tracks %+v", m)
	}
}
