package mover

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/storage"
	"github.com/HHim8826/kanade/server/internal/storage/storagetest"
)

type fixture struct {
	t      *testing.T
	db     *sql.DB
	m      *Mover
	lib    *library.Store
	set    *storage.Set
	local  *storage.Store // a managed folder
	bucket *storage.Store // a managed bucket
	fake   *storagetest.S3
	root   string
	moved  map[string]string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f := &fixture{t: t, db: d, lib: library.New(d), set: &storage.Set{DB: d, Log: log}, root: t.TempDir(), moved: map[string]string{}}
	cfg, _ := json.Marshal(storage.LocalConfig{Root: f.root})
	if f.local, err = f.set.Add(ctx, storage.NewStore{Kind: storage.KindLocal, Name: "Disk", Mode: storage.Managed, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	f.fake = storagetest.NewS3(t)
	cfg, _ = json.Marshal(storage.S3Config{Endpoint: f.fake.URL, Bucket: f.fake.Bucket, Prefix: "kanade"})
	sec, _ := json.Marshal(storage.S3Secret{AccessKey: f.fake.AccessKey, SecretKey: f.fake.SecretKey})
	if f.bucket, err = f.set.Add(ctx, storage.NewStore{Kind: storage.KindS3, Name: "R2", Mode: storage.Managed, Config: cfg, Secret: sec}); err != nil {
		t.Fatal(err)
	}
	f.m = New(d, f.lib, f.set, filepath.Join(t.TempDir(), "staging"), log)
	f.m.OnMoved = func(from, to string) { f.moved[from] = to }
	return f
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// keep puts data in the folder store at key, as an import would, and returns its reference.
func (f *fixture) keep(key string, data []byte) string {
	f.t.Helper()
	p := filepath.Join(f.root, filepath.FromSlash(key))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		f.t.Fatal(err)
	}
	return storage.Ref(storage.KindLocal, f.local.ID, key)
}

type album struct {
	id                         int64
	song, cover, sidecar, scan []byte
	refs                       map[string]string // by kind
}

// album imports an album into the folder store: a song, its cover, a CUE sheet and a scan.
func (f *fixture) album(title string) *album {
	f.t.Helper()
	ctx := context.Background()
	a := &album{song: []byte("song of " + title), cover: []byte("cover of " + title), sidecar: []byte("cue of " + title),
		scan: []byte("scan of " + title), refs: map[string]string{}}
	dir := "Band/" + title
	a.refs["song"] = f.keep("library/"+dir+"/01 Song.flac", a.song)
	asset, err := f.lib.CreateAsset(ctx, library.Asset{SHA256: sum(a.song), Size: int64(len(a.song)), Format: "flac", Codec: "flac"})
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.lib.MarkVerified(ctx, asset.ID, a.refs["song"]); err != nil {
		f.t.Fatal(err)
	}
	pub, err := f.lib.Publish(ctx, asset.ID, library.EntryInput{Title: "Song " + title, Artist: "Band", Album: title, AlbumArtist: "Band"})
	if err != nil {
		f.t.Fatal(err)
	}
	f.db.QueryRowContext(ctx, `SELECT album_id FROM album_entries WHERE id = ?`, pub.EntryID).Scan(&a.id)
	a.refs["cover"] = f.keep("covers/"+sum(a.cover)[:12]+".jpg", a.cover)
	cover, err := f.lib.AddCover(ctx, sum(a.cover), "image/jpeg", a.refs["cover"])
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.lib.SetAlbumCover(ctx, a.id, cover); err != nil {
		f.t.Fatal(err)
	}
	a.refs["sidecar"] = f.keep("sidecars/"+dir+"/"+title+".cue", a.sidecar)
	if _, err := f.lib.AddSidecar(ctx, a.id, title+".cue", "cue", sum(a.sidecar), int64(len(a.sidecar)), a.refs["sidecar"]); err != nil {
		f.t.Fatal(err)
	}
	a.refs["image"] = f.keep("images/"+dir+"/Scans/back.jpg", a.scan)
	if _, err := f.lib.AddImage(ctx, library.AlbumImage{AlbumID: a.id, Path: "Scans/back.jpg", SHA256: sum(a.scan), Size: int64(len(a.scan)),
		MIME: "image/jpeg", Ref: a.refs["image"]}); err != nil {
		f.t.Fatal(err)
	}
	return a
}

// refs is where the library reads an album's files now, by kind.
func (f *fixture) refs(a *album) map[string]string {
	f.t.Helper()
	files, err := f.lib.AlbumFiles(context.Background(), []int64{a.id})
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]string{}
	for _, x := range files {
		out[x.Kind] = x.Ref
	}
	return out
}

func (f *fixture) read(ref string) []byte {
	f.t.Helper()
	rc, err := f.set.Open(context.Background(), ref, 0, -1)
	if err != nil {
		f.t.Fatalf("open %s: %v", ref, err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return b
}

func (f *fixture) owed(ref string) (bool, int64) {
	var next int64
	err := f.db.QueryRow(`SELECT next_at FROM file_trash WHERE file_id = ? AND done_at = 0`, ref).Scan(&next)
	return err == nil, next
}

func (f *fixture) work() { f.m.work(context.Background()) }

func (f *fixture) get(id int64) *Move {
	f.t.Helper()
	v, err := f.m.Get(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

// Every file of a folder store moved to a bucket, where an import would have put it, checked; the
// library reads them there, the stream cache is told, and the old copies are owed to the trash a
// while later. Then an album moved back.
func TestMoveLocation(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	a, b := f.album("One"), f.album("Two")
	p, err := f.m.Plan(ctx, Request{Dest: "2", Origin: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Files != 8 || p.Songs != 2 || p.Here != 0 || p.Fixed != 0 || p.DestName != "R2" || p.Title != "Disk" {
		t.Fatalf("plan %+v", p)
	}
	if want := int64(len(a.song) + len(a.sidecar) + len(a.scan) + len(b.song) + len(b.sidecar) + len(b.scan)); p.Bytes != want {
		t.Fatalf("bytes %d, want %d", p.Bytes, want)
	}
	before := db.Now()
	v, err := f.m.Start(ctx, Request{Dest: "2", Origin: "1"})
	if err != nil || v.State != Running || v.Files != 8 {
		t.Fatalf("start %+v %v", v, err)
	}
	f.work()
	if v = f.get(v.ID); v.State != Done || v.Moved != 8 || v.MovedBytes != p.Bytes || v.FinishedAt == 0 {
		t.Fatalf("after %+v", v)
	}
	names := f.fake.Names()
	sort.Strings(names)
	for _, want := range []string{"kanade/library/Band/One/01 Song.flac", "kanade/sidecars/Band/Two/Two.cue",
		"kanade/images/Band/One/Scans/back.jpg", "kanade/covers/" + sum(a.cover)[:12] + ".jpg"} {
		if !strings.Contains(strings.Join(names, "\n"), want) {
			t.Fatalf("%s not in the bucket: %v", want, names)
		}
	}
	now := f.refs(a)
	for kind, old := range a.refs {
		ref := now[kind]
		if !strings.HasPrefix(ref, "s3:2:") || f.moved[old] != ref {
			t.Fatalf("%s at %s (told %q)", kind, ref, f.moved[old])
		}
		owed, next := f.owed(old)
		if !owed || next < before+(14*time.Minute).Milliseconds() {
			t.Fatalf("%s's old copy owed %v at %d", kind, owed, next)
		}
	}
	if !bytes.Equal(f.read(now["song"]), a.song) || !bytes.Equal(f.read(now["image"]), a.scan) {
		t.Fatal("the copies")
	}
	// Their wait over, the old copies go to the folder's trash.
	f.db.Exec(`UPDATE file_trash SET next_at = 0`)
	f.work()
	for kind, old := range a.refs {
		if owed, _ := f.owed(old); owed {
			t.Fatalf("%s's old copy still owed", kind)
		}
	}
	if _, err := os.Stat(filepath.Join(f.root, "library/Band/One/01 Song.flac")); !os.IsNotExist(err) {
		t.Fatalf("the old copy is still there: %v", err)
	}
	if trash, _ := os.ReadDir(filepath.Join(f.root, ".kanade-trash")); len(trash) == 0 {
		t.Fatal("nothing in the folder's trash")
	}

	// One album back to the folder: from the bucket through staging. Moving it again finds it there.
	v, err = f.m.Start(ctx, Request{Dest: "1", Albums: []int64{b.id}})
	if err != nil || v.Title != "Two" || v.Files != 4 {
		t.Fatalf("start %+v %v", v, err)
	}
	f.work()
	if v = f.get(v.ID); v.State != Done || v.Moved != 4 {
		t.Fatalf("back %+v", v)
	}
	back := f.refs(b)
	if back["song"] != b.refs["song"] || back["image"] != b.refs["image"] {
		t.Fatalf("back at %v, was at %v", back, b.refs)
	}
	if owed, _ := f.owed(b.refs["song"]); owed {
		t.Fatal("the copy at the old place, the library's again, is still owed to the trash")
	}
	if entries, _ := os.ReadDir(f.m.Temp); len(entries) != 0 {
		t.Fatalf("left in staging: %v", entries)
	}
	if p, err := f.m.Plan(ctx, Request{Dest: "1", Albums: []int64{b.id, a.id}}); err != nil || p.Here != 4 || p.Files != 4 || p.Title != "Two" {
		t.Fatalf("plan %+v %v", p, err)
	}
}

// What cannot be asked for, and a source's files staying where they are.
func TestMovePlanRefused(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	a := f.album("One")
	cfg, _ := json.Marshal(storage.LocalConfig{Root: t.TempDir()})
	src, err := f.set.Add(ctx, storage.NewStore{Kind: storage.KindLocal, Name: "Shelf", Mode: storage.Source, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []Request{{Origin: "1"}, {Dest: "9", Origin: "1"}, {Dest: "3", Origin: "1"}, {Dest: "2", Origin: "3"},
		{Dest: "1", Origin: "1"}, {Dest: "2"}, {Dest: "2", Origin: "1", Albums: []int64{a.id}}, {Dest: "2", Albums: []int64{99}}} {
		var bad *ErrInvalid
		if _, err := f.m.Plan(ctx, r); !errors.As(err, &bad) {
			t.Fatalf("%+v: %v", r, err)
		}
	}
	// The song in the source: it stays.
	f.db.Exec(`UPDATE assets SET file_ref = ? WHERE file_ref = ?`, storage.Ref(storage.KindLocal, src.ID, "a.flac"), a.refs["song"])
	p, err := f.m.Plan(ctx, Request{Dest: "2", Albums: []int64{a.id}})
	if err != nil || p.Fixed != 1 || p.Files != 3 || p.Songs != 0 {
		t.Fatalf("plan %+v %v", p, err)
	}
	f.set.SetDefault(ctx, "1")
	if p, err := f.m.Plan(ctx, Request{Dest: "2", Origin: "1"}); err != nil || !p.Default {
		t.Fatalf("plan %+v %v", p, err)
	}
	if _, err := f.m.Start(ctx, Request{Dest: "1", Albums: []int64{a.id}}); err == nil {
		t.Fatal("started a move of nothing")
	}
}

// A file whose bytes are not the ones imported, and one gone, are left where they are and listed;
// one the library let go of meanwhile is passed over. The destination offline: the move waits, and
// goes on once it is back. Canceled, it stops; tried again, the failed ones too.
func TestMoveTrouble(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	a, b := f.album("One"), f.album("Two")
	other := t.TempDir()
	cfg, _ := json.Marshal(storage.LocalConfig{Root: other})
	if _, err := f.set.Add(ctx, storage.NewStore{Kind: storage.KindLocal, Name: "Other", Mode: storage.Managed, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	v, err := f.m.Start(ctx, Request{Dest: "3", Albums: []int64{a.id, b.id}})
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(f.root, "library/Band/One/01 Song.flac"), []byte("other bytes"), 0o644)
	os.Remove(filepath.Join(f.root, "sidecars/Band/One/One.cue"))
	f.db.Exec(`UPDATE album_images SET hidden = 1, file_ref = 'x' WHERE file_ref = ?`, b.refs["image"]) // taken out meanwhile

	// Offline: the folder is not there.
	gone := other + ".away"
	os.Rename(other, gone)
	f.work()
	if v = f.get(v.ID); v.State != Waiting || v.Reason != ReasonDestOffline || v.NextAt <= db.Now() {
		t.Fatalf("offline %+v", v)
	}
	if _, err := f.m.Cancel(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	f.work()
	if v = f.get(v.ID); v.State != Canceled || v.Moved != 0 {
		t.Fatalf("canceled %+v", v)
	}
	if _, err := f.m.Clear(ctx, v.ID+1); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	os.Rename(gone, other)
	if v, err = f.m.Retry(ctx, v.ID); err != nil || v.State != Running {
		t.Fatalf("retry %+v %v", v, err)
	}
	if _, err := f.m.Clear(ctx, v.ID); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	f.work()
	v = f.get(v.ID)
	if v.State != Done || v.Moved != 5 || v.Skipped != 1 || v.Failed != 2 || len(v.Failures) != 2 {
		t.Fatalf("done %+v", v)
	}
	reasons := map[string]string{}
	for _, x := range v.Failures {
		reasons[x.Kind] = x.Reason
	}
	if reasons["song"] != ReasonChanged || reasons["sidecar"] != ReasonGone {
		t.Fatalf("failures %+v", v.Failures)
	}
	if now := f.refs(a); now["song"] != a.refs["song"] || now["sidecar"] != a.refs["sidecar"] || !strings.HasPrefix(now["cover"], "local:3:") {
		t.Fatalf("refs %v", now)
	}
	// Put right, and tried again.
	os.WriteFile(filepath.Join(f.root, "library/Band/One/01 Song.flac"), a.song, 0o644)
	f.m.Retry(ctx, v.ID)
	f.work()
	if v = f.get(v.ID); v.State != Done || v.Moved != 6 || v.Failed != 1 {
		t.Fatalf("again %+v", v)
	}
	if n, err := f.m.Clear(ctx, 0); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if list, _ := f.m.List(ctx); len(list) != 0 {
		t.Fatalf("listed %+v", list)
	}
}

// A file that fails for no known reason is tried three times, the move waiting between tries.
func TestMoveTries(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	a := f.album("One")
	f.fake.Fail = func(r *http.Request) int {
		if r.Method == "PUT" && strings.Contains(r.URL.Path, "Song") {
			return 400
		}
		return 0
	}
	v, _ := f.m.Start(ctx, Request{Dest: "2", Albums: []int64{a.id}})
	for i := range 3 {
		f.db.Exec(`UPDATE moves SET next_at = 0`)
		f.work()
		if v = f.get(v.ID); (i < 2) != (v.State == Waiting) {
			t.Fatalf("try %d: %+v", i, v)
		}
	}
	if v.State != Done || v.Failed != 1 || v.Moved != 3 || v.Failures[0].Reason != ReasonError {
		t.Fatalf("%+v", v)
	}
}
