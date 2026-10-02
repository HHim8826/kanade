package drivesync

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/library"
)

type fakeDrive struct {
	mu       sync.Mutex
	changes  []gdrive.Change
	files    []gdrive.File
	listErr  error
	lists    int
	block    chan struct{} // when set, List waits for it after taking its snapshot
	listing  chan struct{} // closed once a blocked List has its snapshot
	expired  bool          // the next Changes call fails as an expired position
	trashed  []string
	trashErr error
}

func (f *fakeDrive) Trash(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.trashErr != nil {
		return f.trashErr
	}
	f.trashed = append(f.trashed, id)
	return nil
}

func (f *fakeDrive) StartPageToken(context.Context) (string, error) { return "t1", nil }
func (f *fakeDrive) Changes(_ context.Context, token string) ([]gdrive.Change, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.expired {
		f.expired = false
		return nil, "", &gdrive.APIError{Status: 400}
	}
	c := f.changes
	f.changes = nil
	return c, token + "+", nil
}
func (f *fakeDrive) List(_ context.Context, q string, fn func([]gdrive.File) error) error {
	f.mu.Lock()
	f.lists++
	page := append([]gdrive.File(nil), f.files...)
	block, listing, err := f.block, f.listing, f.listErr
	f.mu.Unlock()
	if block != nil {
		close(listing)
		<-block
	}
	if err := fn(page); err != nil {
		return err
	}
	return err // a failure after a first page
}

func file(id string) gdrive.File {
	return gdrive.File{ID: "drive-" + id, SHA256Checksum: id, Size: "1"}
}

func setup(t *testing.T, ids ...string) (*Syncer, *fakeDrive, *library.Store, func(string) string) {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	lib := library.New(d)
	for _, id := range ids {
		as, _ := lib.CreateAsset(ctx, library.Asset{SHA256: id, Size: 1, Format: "flac", Codec: "flac"})
		lib.MarkVerified(ctx, as.ID, "drive-"+id)
	}
	state := func(id string) string {
		var s string
		d.QueryRow(`SELECT state FROM assets WHERE drive_file_id = ?`, "drive-"+id).Scan(&s)
		return s
	}
	fd := &fakeDrive{}
	return &Syncer{DB: d, Drive: fd, Lib: lib, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, fd, lib, state
}

func TestChangesAndFullPass(t *testing.T) {
	ctx := context.Background()
	s, fd, lib, state := setup(t, "a", "b", "c")

	// The first look only takes the starting point, and asks for a baseline.
	fd.changes = []gdrive.Change{{FileID: "drive-a", Removed: true}}
	if r, err := s.Changes(ctx); err != nil || r.Checked != 0 || state("a") != library.AssetVerified || !s.Status().BaselinePending {
		t.Fatalf("first: %+v %v", r, err)
	}
	fd.changes = []gdrive.Change{{FileID: "drive-a", File: &gdrive.File{ID: "drive-a", Trashed: true}},
		{FileID: "drive-b", Removed: true}, {FileID: "other", Removed: true}}
	if r, err := s.Changes(ctx); err != nil || r.Missing != 2 || state("a") != library.AssetMissing || state("b") != library.AssetMissing {
		t.Fatalf("changes: %+v %v", r, err)
	}
	a := file("a")
	fd.changes = []gdrive.Change{{FileID: "drive-a", File: &a}} // restored from the trash
	if r, _ := s.Changes(ctx); r.Restored != 1 || state("a") != library.AssetVerified {
		t.Fatalf("restore: %+v", r)
	}
	if a, _ := lib.Attention(ctx); a.Missing != 1 {
		t.Fatalf("attention %+v", a)
	}

	// A listing that fails part way marks nothing, and the baseline stays pending.
	fd.files, fd.listErr = []gdrive.File{file("a")}, errors.New("quota")
	if _, err := s.Full(ctx); err == nil || state("c") != library.AssetVerified || !s.Status().BaselinePending {
		t.Fatalf("partial listing: %v %s", err, state("c"))
	}
	fd.listErr = nil
	fd.files = []gdrive.File{file("a"), file("b")}
	r, err := s.Full(ctx)
	if err != nil || r.Checked != 3 || r.Missing != 1 || r.Restored != 1 || state("b") != library.AssetVerified || state("c") != library.AssetMissing {
		t.Fatalf("full: %+v %v", r, err)
	}
	if st := s.Status(); st.LastFull == 0 || st.LastFullRes.Missing != 1 || st.BaselinePending {
		t.Fatalf("status %+v", st)
	}
	again := &Syncer{DB: s.DB, Drive: fd, Lib: lib, Log: s.Log} // after a restart
	if st := again.Status(); st.LastFull == 0 || st.LastFullRes == nil || st.LastFullRes.Missing != 1 {
		t.Fatalf("status after restart %+v", st)
	}
	if m, _ := lib.Missing(ctx); len(m) != 0 { // no track uses these bare assets
		t.Fatalf("missing tracks %+v", m)
	}
}

// Files that went missing before the change feed's starting point are found by the baseline that
// follows the first look, and again after the position expires.
func TestBaselineAfterStartAndExpiry(t *testing.T) {
	ctx := context.Background()
	s, fd, _, state := setup(t, "a", "b")
	fd.files = []gdrive.File{file("a")} // b was deleted before syncing started
	s.Changes(ctx)
	if !s.baselinePending(ctx) || fd.lists != 0 {
		t.Fatal("no baseline asked for")
	}
	if _, err := s.Full(ctx); err != nil || state("b") != library.AssetMissing || s.baselinePending(ctx) {
		t.Fatalf("baseline: %v %s", err, state("b"))
	}
	// The position expires; a is deleted in the gap.
	fd.expired = true
	if _, err := s.Changes(ctx); err == nil {
		t.Fatal("expired position not reported")
	}
	fd.files = nil
	s.Changes(ctx) // takes a new position
	if !s.baselinePending(ctx) {
		t.Fatal("no baseline after the position expired")
	}
	s.Full(ctx)
	if state("a") != library.AssetMissing || s.baselinePending(ctx) {
		t.Fatalf("after expiry: %s", state("a"))
	}
}

// A full pass's listing never overrides a newer change: the feed waits for the pass and applies
// what happened meanwhile after it.
func TestFullAndChangesKeepNewest(t *testing.T) {
	ctx := context.Background()
	s, fd, _, state := setup(t, "a")
	s.Changes(ctx) // the position
	d := s.DB
	d.Exec(`UPDATE assets SET state = 'missing'`)
	fd.files = []gdrive.File{file("a")} // the listing sees it back ...
	fd.block, fd.listing = make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.Full(ctx) }()
	<-fd.listing
	fd.mu.Lock()
	fd.changes = []gdrive.Change{{FileID: "drive-a", Removed: true}} // ... but it is deleted again before the pass applies
	fd.mu.Unlock()
	go func() { defer wg.Done(); s.Changes(ctx) }()
	close(fd.block)
	wg.Wait()
	if state("a") != library.AssetMissing {
		t.Fatalf("older listing won: %s", state("a"))
	}
}

// Bytes replaced under the same file ID are not trusted.
func TestChangedContentIsNotVerified(t *testing.T) {
	ctx := context.Background()
	s, fd, _, state := setup(t, "a", "b")
	s.Changes(ctx)
	other := gdrive.File{ID: "drive-a", SHA256Checksum: "different", Size: "1"}
	fd.changes = []gdrive.Change{{FileID: "drive-a", File: &other}}
	if r, _ := s.Changes(ctx); r.Missing != 1 || state("a") != library.AssetMissing {
		t.Fatalf("changed bytes kept verified: %+v %s", r, state("a"))
	}
	// A full pass does not bring it back while the content differs, but does once it matches.
	fd.files = []gdrive.File{other, file("b")}
	s.Full(ctx)
	if state("a") != library.AssetMissing {
		t.Fatal("full pass restored different bytes")
	}
	resized := gdrive.File{ID: "drive-b", SHA256Checksum: "b", Size: "2"}
	fd.files = []gdrive.File{file("a"), resized}
	s.Full(ctx)
	if state("a") != library.AssetVerified || state("b") != library.AssetMissing {
		t.Fatalf("full: a=%s b=%s", state("a"), state("b"))
	}
}

// A deleted song's file owed to the Drive trash is moved there by the background retry once Drive
// answers again (review #26).
func TestTrashIsRetried(t *testing.T) {
	ctx := context.Background()
	s, fd, lib, _ := setup(t)
	as, _ := lib.CreateAsset(ctx, library.Asset{SHA256: "t1", Size: 1, Format: "flac", Codec: "flac"})
	lib.MarkVerified(ctx, as.ID, "drive-t1")
	res, _ := lib.Publish(ctx, as.ID, library.EntryInput{Title: "Gone"})
	files, err := lib.DeleteTrack(ctx, res.TrackID)
	if err != nil || len(files) != 1 {
		t.Fatalf("delete %v %v", files, err)
	}
	fd.trashErr = errors.New("drive is down")
	lib.TrashFailed(ctx, "drive-t1", fd.trashErr) // the request's own try failed
	if n, last, _ := lib.TrashPending(ctx); n != 1 || last == "" || s.Status().TrashPending != 1 {
		t.Fatalf("pending %d %q", n, last)
	}
	s.DB.Exec(`UPDATE drive_trash SET next_at = 0`)
	if s.RetryTrash(ctx) != 0 {
		t.Fatal("retried while drive is down")
	}
	fd.trashErr = nil
	s.DB.Exec(`UPDATE drive_trash SET next_at = 0`)
	if s.RetryTrash(ctx) != 1 || len(fd.trashed) != 1 || fd.trashed[0] != "drive-t1" {
		t.Fatalf("not retried: %v", fd.trashed)
	}
	if n, _, _ := lib.TrashPending(ctx); n != 0 {
		t.Fatalf("still pending %d", n)
	}
}
