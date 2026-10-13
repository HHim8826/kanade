package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HHim8826/kanade/server/internal/db"
)

func newSet(t *testing.T) (*Set, string) {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	data := t.TempDir()
	return &Set{DB: d, DataDir: data}, data
}

func addLocal(t *testing.T, s *Set, root, mode string) *Store {
	t.Helper()
	cfg, _ := json.Marshal(map[string]string{"root": root})
	st, err := s.Add(context.Background(), NewStore{Kind: KindLocal, Name: "NAS " + mode, Mode: mode, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func file(t *testing.T, dir, name, body string) (path, sha string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	return p, hex.EncodeToString(sum[:])
}

func read(t *testing.T, s *Set, ref string, start, end int64) string {
	t.Helper()
	rc, err := s.Open(context.Background(), ref, start, end)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A managed folder keeps files under their names, checked; the same content twice is one file, a
// name taken by other content gets another; ranges read back; the trash keeps what left.
func TestLocalManaged(t *testing.T) {
	ctx := context.Background()
	s, _ := newSet(t)
	root := t.TempDir()
	st := addLocal(t, s, root, Managed)
	if st.State != StateOK || st.Usage == nil || st.Usage.Total == 0 {
		t.Fatalf("added: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(root, markFile)); err != nil {
		t.Fatal("no mark:", err)
	}
	if err := s.SetDefault(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	dest, err := s.Destination(ctx, "")
	if err != nil || dest.ID() != "1" {
		t.Fatalf("destination %v %v", dest, err)
	}
	src := t.TempDir()
	p, sha := file(t, src, "a.flac", "first song bytes")
	put := PutFile{Path: p, Dir: "library/Artist/Album", Name: "01 Song.flac", Size: 16, SHA256: sha}
	ref, err := dest.Put(ctx, put)
	if err != nil {
		t.Fatal(err)
	}
	if want := "local:1:library/Artist/Album/01 Song.flac"; ref != want {
		t.Fatalf("ref %q, want %q", ref, want)
	}
	if again, err := dest.Put(ctx, put); err != nil || again != ref {
		t.Fatalf("again: %q %v", again, err)
	}
	p2, sha2 := file(t, src, "b.flac", "other song bytes")
	ref2, err := dest.Put(ctx, PutFile{Path: p2, Dir: "library/Artist/Album", Name: "01 Song.flac", Size: 16, SHA256: sha2})
	if err != nil || ref2 != "local:1:library/Artist/Album/01 Song (2).flac" {
		t.Fatalf("taken name: %q %v", ref2, err)
	}
	if got := read(t, s, ref, 6, 9); got != "song" {
		t.Fatalf("range %q", got)
	}
	if got := read(t, s, ref2, 0, -1); got != "other song bytes" {
		t.Fatalf("whole %q", got)
	}
	if path, ok, err := s.LocalPath(ctx, ref); !ok || err != nil || path != filepath.Join(root, "library/Artist/Album/01 Song.flac") {
		t.Fatalf("path %q %v %v", path, ok, err)
	}
	// A file that changed after it was hashed is not kept.
	if _, err := dest.Put(ctx, PutFile{Path: p, Dir: "library/X", Name: "x.flac", Size: 16, SHA256: sha2}); err == nil {
		t.Fatal("kept a file that does not hash as said")
	}
	entries, _ := os.ReadDir(filepath.Join(root, tempDir))
	if len(entries) != 0 {
		t.Fatalf("left in %s: %v", tempDir, entries)
	}
	if err := s.Trash(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "library/Artist/Album/01 Song.flac")); !os.IsNotExist(err) {
		t.Fatal("still there after the trash:", err)
	}
	trashed, _ := filepath.Glob(filepath.Join(root, trashDir, "*", "library/Artist/Album/01 Song.flac"))
	if len(trashed) != 1 {
		t.Fatal("not in the trash")
	}
	if err := s.Trash(ctx, ref); err != nil {
		t.Fatal("trashing what is gone:", err)
	}
	if _, err := s.Open(ctx, ref, 0, -1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reading what is gone: %v", err)
	}
	if err := s.Trash(ctx, "local:9:x.flac"); err != nil {
		t.Fatal("a store that was removed:", err)
	}
}

// A managed folder without its mark (a NAS not mounted: the bare folder under it) is offline: not
// written to, its files not gone.
func TestLocalUnmounted(t *testing.T) {
	ctx := context.Background()
	s, _ := newSet(t)
	root := t.TempDir()
	addLocal(t, s, root, Managed)
	src := t.TempDir()
	p, sha := file(t, src, "a.flac", "song")
	dest, _ := s.Destination(ctx, "1")
	ref, err := dest.Put(ctx, PutFile{Path: p, Dir: "library/A", Name: "a.flac", Size: 4, SHA256: sha})
	if err != nil {
		t.Fatal(err)
	}
	hidden := root + ".away"
	if err := os.Rename(root, hidden); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(root, 0o755) // the mount point left behind
	if _, err := dest.Put(ctx, PutFile{Path: p, Dir: "library/A", Name: "b.flac", Size: 4, SHA256: sha}); !errors.Is(err, ErrOffline) {
		t.Fatalf("written while unmounted: %v", err)
	}
	if _, err := s.Open(ctx, ref, 0, -1); !errors.Is(err, ErrOffline) {
		t.Fatalf("read while unmounted: %v", err)
	}
	if reason, lasting := Explain(func() error { _, err := s.Open(ctx, ref, 0, -1); return err }()); reason != "offline" || lasting {
		t.Fatalf("explained as %s %v", reason, lasting)
	}
	if err := s.Trash(ctx, ref); !errors.Is(err, ErrOffline) {
		t.Fatalf("trashed while unmounted: %v", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("written on the disk below: %v", entries)
	}
	st, _ := s.Check(ctx, 1)
	if st.State != StateOffline || !strings.Contains(st.Error, markFile) {
		t.Fatalf("checked: %+v", st)
	}
	os.Remove(root)
	os.Rename(hidden, root)
	if st, _ := s.Check(ctx, 1); st.State != StateOK {
		t.Fatalf("back: %+v", st)
	}
	if got := read(t, s, ref, 0, -1); got != "song" {
		t.Fatal(got)
	}
}

// A source is read and walked, never written; an empty one is offline; its files stay inside it.
func TestLocalSource(t *testing.T) {
	ctx := context.Background()
	s, _ := newSet(t)
	root := t.TempDir()
	file(t, root, "Artist/Album/01.flac", "one")
	file(t, root, "Artist/Album/cover.jpg", "jpg")
	file(t, root, "Artist/Album/._01.flac", "fork")
	file(t, root, "@eaDir/01.flac/SYNOFILE_THUMB_M.jpg", "thumb")
	file(t, root, "Artist/#recycle/x.flac", "x")
	file(t, root, "$RECYCLE.BIN/x.flac", "x")
	file(t, root, ".Trash-1000/files/x.flac", "x")
	// Music in folders whose names only look like the systems' (review #223).
	file(t, root, "@onefive/1518/01.flac", "at")
	file(t, root, "$uicideboy$/Long Term/01.flac", "dollar")
	file(t, root, ".hack G.U/01.flac", "dot")
	outside := t.TempDir()
	file(t, outside, "secret.flac", "secret")
	os.Symlink(filepath.Join(outside, "secret.flac"), filepath.Join(root, "link.flac"))
	st := addLocal(t, s, root, Source)
	if st.State != StateOK {
		t.Fatalf("added: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(root, markFile)); !os.IsNotExist(err) {
		t.Fatal("a source was written to")
	}
	b, _, _ := s.Backend(ctx, 1)
	var keys []string
	if err := b.Walk(ctx, func(o Object) error { keys = append(keys, o.Key); return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, " ") != "$uicideboy$/Long Term/01.flac .hack G.U/01.flac @onefive/1518/01.flac Artist/Album/01.flac Artist/Album/cover.jpg" {
		t.Fatalf("walked %v", keys)
	}
	if got := read(t, s, Ref(KindLocal, 1, "Artist/Album/01.flac"), 0, -1); got != "one" {
		t.Fatal(got)
	}
	for _, key := range []string{"../" + filepath.Base(outside) + "/secret.flac", "link.flac", "/etc/passwd"} {
		if _, err := s.Open(ctx, Ref(KindLocal, 1, key), 0, -1); err == nil {
			t.Fatalf("read %s", key)
		}
	}
	if _, err := s.Destination(ctx, "1"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("a source as a destination: %v", err)
	}
	if err := s.SetDefault(ctx, "1"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("a source as the default: %v", err)
	}
	var forgot []string
	s.Forget = func(_ context.Context, store int64, key string) error { forgot = append(forgot, key); return nil }
	if err := s.Trash(ctx, Ref(KindLocal, 1, "Artist/Album/01.flac")); err != nil || len(forgot) != 1 {
		t.Fatalf("let go of: %v %v", err, forgot)
	}
	if _, err := os.Stat(filepath.Join(root, "Artist/Album/01.flac")); err != nil {
		t.Fatal("a source's file was deleted:", err)
	}
	// Emptied (as a NAS that is not mounted looks): offline, not a source whose files are gone.
	os.RemoveAll(root)
	os.Mkdir(root, 0o755)
	if err := b.Walk(ctx, func(Object) error { return nil }); !errors.Is(err, ErrOffline) {
		t.Fatalf("walking an empty source: %v", err)
	}
}

// Folders that overlap what Kanade empties as it works, or another store, are refused.
func TestLocalOverlaps(t *testing.T) {
	s, data := newSet(t)
	root := t.TempDir()
	os.MkdirAll(filepath.Join(data, "staging", "music"), 0o755)
	os.MkdirAll(filepath.Join(data, "library"), 0o755)
	os.MkdirAll(filepath.Join(root, "inner"), 0o755)
	addLocal(t, s, root, Source)
	for _, dir := range []string{data, filepath.Dir(data), filepath.Join(data, "staging", "music"), filepath.Join(root, "inner"),
		filepath.Dir(root), "relative/path", "/no/such/folder"} {
		cfg, _ := json.Marshal(map[string]string{"root": dir})
		if _, err := s.Add(context.Background(), NewStore{Kind: KindLocal, Name: "x", Mode: Managed, Config: cfg}); err == nil {
			t.Errorf("added %s", dir)
		}
	}
	addLocal(t, s, filepath.Join(data, "library"), Managed) // a folder of its own in the data folder is fine
}
