package importer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/storage"
)

// withStores gives the importer storage locations (#99): imports go where the Set's default says.
func withStores(t *testing.T, im *Importer) *storage.Set {
	t.Helper()
	set := &storage.Set{DB: im.db, Log: im.log, DataDir: t.TempDir()}
	set.Forget = im.ForgetSource
	im.Stores = set
	return set
}

func addFolder(t *testing.T, set *storage.Set, root, mode string) *storage.Store {
	t.Helper()
	cfg, _ := json.Marshal(map[string]string{"root": root})
	st, err := set.Add(context.Background(), storage.NewStore{Kind: storage.KindLocal, Name: "NAS", Mode: mode, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

var jpeg = append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 16, 'J', 'F', 'I', 'F', 0}, make([]byte, 64)...)

// tree lists the files below dir with their sizes and times, to see nothing changed there.
func tree(t *testing.T, dir string) string {
	t.Helper()
	var out []string
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, fmt.Sprintf("%s %d %d", rel, info.Size(), info.ModTime().UnixNano()))
		}
		return nil
	})
	sort.Strings(out)
	return strings.Join(out, "\n")
}

func assetRefs(t *testing.T, im *Importer) map[string]string {
	t.Helper()
	rows, err := im.db.Query(`SELECT a.state || ' ' || coalesce(a.file_ref, ''), t.title FROM assets a
		JOIN track_assets ta ON ta.asset_id = a.id JOIN tracks t ON t.id = ta.track_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var ref, title string
		rows.Scan(&ref, &title)
		out[title] = ref
	}
	return out
}

// Imports kept in a folder of the server (#99): songs, covers and CUE sheets there, read back; no
// Google Drive needed.
func TestImportToFolder(t *testing.T) {
	ctx := context.Background()
	im, lib, fd := setup(t)
	set := withStores(t, im)
	lib0 := t.TempDir()
	addFolder(t, set, lib0, storage.Managed)
	if err := set.SetDefault(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	taggedMP3(t, filepath.Join(src, "Album", "01.mp3"), map[string]string{"TIT2": "One", "TPE1": "Band", "TALB": "Album", "TRCK": "1"})
	os.WriteFile(filepath.Join(src, "Album", "cover.jpg"), jpeg, 0o644)
	os.WriteFile(filepath.Join(src, "Album", "rip.log"), []byte("EAC extraction logfile"), 0o644)
	b, _, err := im.CreateBatch(ctx, "local", "", src, false)
	if err != nil {
		t.Fatal(err)
	}
	if v := runUntilDone(t, im, b); states(v)["Album/01.mp3"] != StatePublished || states(v)["Album/rip.log"] != StatePublished {
		t.Fatalf("states %v", states(v))
	}
	if fd.uploads != 0 {
		t.Fatalf("%d uploads to Drive", fd.uploads)
	}
	refs := assetRefs(t, im)
	if refs["One"] != "verified local:1:library/Band/Album/01.mp3" {
		t.Fatalf("refs %v", refs)
	}
	stored, _ := os.ReadFile(filepath.Join(lib0, "library/Band/Album/01.mp3"))
	orig, _ := os.ReadFile(filepath.Join(src, "Album", "01.mp3"))
	if string(stored) != string(orig) {
		t.Fatal("the stored copy is not the file")
	}
	albums, _ := lib.Albums(ctx, 10, 0, false)
	cover, err := lib.Cover(ctx, albums[0].CoverID)
	if err != nil || cover == nil || !strings.HasPrefix(cover.Ref, "local:1:covers/") {
		t.Fatalf("cover %+v %v", cover, err)
	}
	var sidecar string
	im.db.QueryRow(`SELECT file_ref FROM sidecars`).Scan(&sidecar)
	if sidecar != "local:1:sidecars/Band/Album/rip.log" {
		t.Fatalf("sidecar %q", sidecar)
	}
	rc, err := set.Open(ctx, sidecar, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	var sessions int
	im.db.QueryRow(`SELECT count(*) FROM upload_sessions`).Scan(&sessions)
	if sessions != 0 {
		t.Fatalf("%d sessions left", sessions)
	}
}

// A source is imported where it is (#99) and never written to; scans follow what changes there.
func TestSourceScans(t *testing.T) {
	ctx := context.Background()
	im, _, fd := setup(t)
	set := withStores(t, im)
	src := t.TempDir()
	song := func(rel, title string) string {
		p := filepath.Join(src, rel)
		taggedMP3(t, p, map[string]string{"TIT2": title, "TPE1": "Band", "TALB": "Album", "TRCK": "1"})
		return p
	}
	song("Band/Album/01.mp3", "One")
	song("Band/Album/02.mp3", "Two")
	os.WriteFile(filepath.Join(src, "Band/Album/folder.jpg"), jpeg, 0o644)
	os.WriteFile(filepath.Join(src, "Band/Album/01.lrc"), []byte("[00:01.00]la"), 0o644)
	os.WriteFile(filepath.Join(src, "Band/Album/notes.txt"), []byte("x"), 0o644)
	st := addFolder(t, set, src, storage.Source)
	before := tree(t, src)
	scan := func() ScanResult {
		t.Helper()
		res, err := im.ScanSource(ctx, set, st.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		if res.Batch != 0 {
			runUntilDone(t, im, res.Batch)
		}
		return res
	}
	res := scan()
	if res.Files != 3 || res.New != 3 || res.Batch == 0 { // the songs, and folder.jpg for the gallery (#134)
		t.Fatalf("first scan %+v", res)
	}
	refs := assetRefs(t, im)
	if refs["One"] != "verified local:1:Band/Album/01.mp3" || refs["Two"] != "verified local:1:Band/Album/02.mp3" {
		t.Fatalf("refs %v", refs)
	}
	if fd.uploads != 0 {
		t.Fatal("uploaded")
	}
	var lyrics string
	im.db.QueryRow(`SELECT l.text FROM lyrics l JOIN tracks t ON t.id = l.track_id WHERE t.title = 'One'`).Scan(&lyrics)
	if !strings.Contains(lyrics, "la") {
		t.Fatalf("lyrics %q", lyrics)
	}
	if after := tree(t, src); after != before {
		t.Fatalf("the source changed:\n%s\n---\n%s", before, after)
	}
	var cover string
	im.db.QueryRow(`SELECT c.file_ref FROM albums al JOIN covers c ON c.id = al.cover_id`).Scan(&cover)
	if cover != "local:1:Band/Album/folder.jpg" {
		t.Fatalf("the album's cover is %q, not the folder's picture where it is", cover)
	}
	if res := scan(); res.New+res.Changed+res.Moved+res.Gone+res.Back != 0 || res.Batch != 0 {
		t.Fatalf("a scan with nothing new: %+v", res)
	}

	// Moved: followed by content, the song kept.
	os.MkdirAll(filepath.Join(src, "Band/Album (2020)"), 0o755)
	os.Rename(filepath.Join(src, "Band/Album/02.mp3"), filepath.Join(src, "Band/Album (2020)/02.mp3"))
	if res := scan(); res.Moved != 1 || res.New != 0 || res.Gone != 0 {
		t.Fatalf("moved: %+v", res)
	}
	if refs := assetRefs(t, im); refs["Two"] != "verified local:1:Band/Album (2020)/02.mp3" {
		t.Fatalf("after the move %v", refs)
	}
	// Gone, then back.
	os.Rename(filepath.Join(src, "Band/Album/01.mp3"), filepath.Join(t.TempDir(), "away.mp3"))
	if res := scan(); res.Gone != 1 {
		t.Fatalf("gone: %+v", res)
	}
	if refs := assetRefs(t, im); refs["One"] != "missing local:1:Band/Album/01.mp3" {
		t.Fatalf("gone %v", refs)
	}
	song("Band/Album/01.mp3", "One")
	// The same tags make the same bytes, but a new time: read again, the same song back.
	res = scan()
	if refs := assetRefs(t, im); refs["One"] != "verified local:1:Band/Album/01.mp3" {
		t.Fatalf("back %v (%+v)", refs, res)
	}
	// Other content at the same place: the old song is missing, the new one imported.
	song("Band/Album/01.mp3", "One (2024 mix)")
	if res := scan(); res.Changed != 1 {
		t.Fatalf("changed: %+v", res)
	}
	refs = assetRefs(t, im)
	if refs["One"] != "missing " || refs["One (2024 mix)"] != "verified local:1:Band/Album/01.mp3" {
		t.Fatalf("changed %v", refs)
	}
}

// A source that is not all there (emptied, as a NAS not mounted looks; or most of it gone at once)
// marks nothing missing.
func TestSourceNotAllThere(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	set := withStores(t, im)
	src := t.TempDir()
	for i := 1; i <= 30; i++ {
		taggedMP3(t, filepath.Join(src, fmt.Sprintf("A/%02d.mp3", i)), map[string]string{"TIT2": fmt.Sprint("Song ", i), "TALB": "A", "TPE1": "B"})
	}
	os.WriteFile(filepath.Join(src, "A/99 broken.mp3"), []byte("not audio"), 0o644)
	os.WriteFile(filepath.Join(src, "A/cover.jpg"), jpeg, 0o644)
	st := addFolder(t, set, src, storage.Source)
	res, err := im.ScanSource(ctx, set, st.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	runUntilDone(t, im, res.Batch)
	cover := func() string {
		var ref string
		im.db.QueryRow(`SELECT coalesce(c.file_ref, '') FROM albums al LEFT JOIN covers c ON c.id = al.cover_id WHERE al.title = 'A'`).Scan(&ref)
		return ref
	}
	if cover() != "local:1:A/cover.jpg" {
		t.Fatalf("cover %q", cover())
	}
	// A song deleted from the library: its file is let go of.
	var track int64
	im.db.QueryRow(`SELECT t.id FROM tracks t WHERE t.title = 'Song 30'`).Scan(&track)
	refs, err := lib.DeleteTrack(ctx, track)
	if err != nil || len(refs) != 1 {
		t.Fatalf("delete: %v %v", refs, err)
	}
	if _, err := lib.TrashFile(ctx, refs[0], set.Trash); err != nil {
		t.Fatal(err)
	}
	hidden := t.TempDir()
	os.Rename(filepath.Join(src, "A"), filepath.Join(hidden, "A"))
	if _, err := im.ScanSource(ctx, set, st.ID, false); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("scanning an empty source: %v", err)
	}
	if s, _ := set.Get(ctx, st.ID); s.State != storage.StateOffline {
		t.Fatalf("state %+v", s)
	}
	os.MkdirAll(filepath.Join(src, "A"), 0o755)
	for i := 1; i <= 5; i++ {
		os.Rename(filepath.Join(hidden, fmt.Sprintf("A/%02d.mp3", i)), filepath.Join(src, fmt.Sprintf("A/%02d.mp3", i)))
	}
	res, err = im.ScanSource(ctx, set, st.ID, false)
	if err != nil || res.Held != 24 || res.Gone != 0 { // 30 imported, one deleted since
		t.Fatalf("most gone at once: %+v %v", res, err)
	}
	var missing int
	im.db.QueryRow(`SELECT count(*) FROM assets WHERE state = 'missing'`).Scan(&missing)
	if missing != 0 || cover() != "local:1:A/cover.jpg" || res.Covers != 0 {
		t.Fatalf("%d marked missing, the cover %q (%d followed)", missing, cover(), res.Covers)
	}
	// All back: the song deleted stays deleted, the broken file is not read again (review #227).
	for _, f := range []string{"A/30.mp3", "A/99 broken.mp3", "A/cover.jpg"} {
		if err := os.Rename(filepath.Join(hidden, f), filepath.Join(src, f)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 6; i <= 29; i++ {
		os.Rename(filepath.Join(hidden, fmt.Sprintf("A/%02d.mp3", i)), filepath.Join(src, fmt.Sprintf("A/%02d.mp3", i)))
	}
	if res, err = im.ScanSource(ctx, set, st.ID, false); err != nil || res.New != 0 || res.Changed != 0 || res.Held != 0 || res.Covers != 0 {
		t.Fatalf("all back: %+v %v", res, err)
	}
	if cover() != "local:1:A/cover.jpg" {
		t.Fatalf("the cover after: %q", cover())
	}
}

// A preview canceled leaves its files to be found again; one imported remembers the files left
// out of it as let go of, and those skipped when analyzed (review #228).
func TestSourcePreviewCanceled(t *testing.T) {
	ctx := context.Background()
	im, _, _ := setup(t)
	set := withStores(t, im)
	startWorker(t, im)
	src := t.TempDir()
	for i := 1; i <= 3; i++ {
		taggedMP3(t, filepath.Join(src, fmt.Sprintf("B/%02d.mp3", i)), map[string]string{"TIT2": fmt.Sprint("Song ", i), "TALB": "B", "TPE1": "C"})
	}
	os.WriteFile(filepath.Join(src, "B/99 broken.mp3"), []byte("not audio"), 0o644)
	st := addFolder(t, set, src, storage.Source)
	states := func() string {
		t.Helper()
		rows, err := im.db.Query(`SELECT path, state FROM source_files WHERE store_id = ? ORDER BY path`, st.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var p, s string
			rows.Scan(&p, &s)
			out = append(out, p+"="+s)
		}
		return strings.Join(out, " ")
	}
	res, err := im.ScanSource(ctx, set, st.ID, true)
	if err != nil || res.New != 4 {
		t.Fatalf("scanned: %+v %v", res, err)
	}
	waitState(t, im, res.Batch, BatchReview)
	if err := im.Cancel(ctx, res.Batch); err != nil {
		t.Fatal(err)
	}
	if got := states(); got != "" {
		t.Fatalf("canceled, still known: %s", got)
	}
	res, err = im.ScanSource(ctx, set, st.ID, true)
	if err != nil || res.New != 4 {
		t.Fatalf("scanned again: %+v %v", res, err)
	}
	waitState(t, im, res.Batch, BatchReview)
	p, err := im.Preview(ctx, res.Batch)
	if err != nil {
		t.Fatal(err)
	}
	g := group(t, p, "B")
	if err := im.ApplyOp(ctx, res.Batch, PlanOp{Op: "exclude", Items: []int64{g.Items[2].ID}}); err != nil {
		t.Fatal(err)
	}
	if err := im.Start(ctx, res.Batch); err != nil {
		t.Fatal(err)
	}
	waitState(t, im, res.Batch, BatchDone)
	if got, want := states(), "B/01.mp3=imported B/02.mp3=imported B/03.mp3=ignored B/99 broken.mp3=skipped"; got != want {
		t.Fatalf("imported: %s", got)
	}
	if res, err = im.ScanSource(ctx, set, st.ID, false); err != nil || res.New+res.Changed != 0 {
		t.Fatalf("nothing new: %+v %v", res, err)
	}
	// The broken file mended: read again.
	taggedMP3(t, filepath.Join(src, "B/99 broken.mp3"), map[string]string{"TIT2": "Mended", "TALB": "B", "TPE1": "C"})
	future := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(src, "B/99 broken.mp3"), future, future)
	if res, err = im.ScanSource(ctx, set, st.ID, false); err != nil || res.Changed != 1 {
		t.Fatalf("mended: %+v %v", res, err)
	}
}

// A song deleted from the library leaves its file in the source alone, and scans leave the file
// out until it changes.
func TestSourceSongDeleted(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	set := withStores(t, im)
	src := t.TempDir()
	taggedMP3(t, filepath.Join(src, "01.mp3"), map[string]string{"TIT2": "One"})
	st := addFolder(t, set, src, storage.Source)
	res, _ := im.ScanSource(ctx, set, st.ID, false)
	runUntilDone(t, im, res.Batch)
	var track int64
	im.db.QueryRow(`SELECT id FROM tracks`).Scan(&track)
	refs, err := lib.DeleteTrack(ctx, track)
	if err != nil || len(refs) != 1 {
		t.Fatalf("delete: %v %v", refs, err)
	}
	if n, err := lib.TrashFile(ctx, refs[0], set.Trash); n != library.Trashed || err != nil {
		t.Fatalf("let go: %v %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(src, "01.mp3")); err != nil {
		t.Fatal("the source's file went:", err)
	}
	if res, _ := im.ScanSource(ctx, set, st.ID, false); res.New+res.Changed != 0 {
		t.Fatalf("imported again: %+v", res)
	}
	future := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(src, "01.mp3"), future, future)
	if res, _ := im.ScanSource(ctx, set, st.ID, false); res.Changed != 1 {
		t.Fatalf("changed after being let go of: %+v", res)
	}
}

// A source's albums go in the category it names (#92), as it is set when they are imported; the
// library lists albums by where their files are.
func TestSourceCategory(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	set := withStores(t, im)
	startWorker(t, im)
	src := t.TempDir()
	song := func(rel, album string) {
		taggedMP3(t, filepath.Join(src, rel), map[string]string{"TIT2": "Song " + album, "TPE1": "Band", "TALB": album, "TRCK": "1"})
	}
	song("A/01.mp3", "Album A")
	c, err := lib.CreateCategory(ctx, "NAS 收藏")
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]string{"root": src})
	if _, err := set.Add(ctx, storage.NewStore{Kind: storage.KindLocal, Name: "Managed", Mode: storage.Managed, Config: json.RawMessage(`{"root":"` + t.TempDir() + `"}`), Category: c.ID}); err == nil {
		t.Fatal("a managed location with a category")
	}
	if _, err := set.Add(ctx, storage.NewStore{Kind: storage.KindLocal, Name: "NAS", Mode: storage.Source, Config: cfg, Category: 999}); err == nil {
		t.Fatal("a category that is not there")
	}
	st, err := set.Add(ctx, storage.NewStore{Kind: storage.KindLocal, Name: "NAS", Mode: storage.Source, Config: cfg, Category: c.ID})
	if err != nil || st.Category != c.ID {
		t.Fatalf("added %+v %v", st, err)
	}
	// In review, the encoding changed: the batch still knows its source.
	res, err := im.ScanSource(ctx, set, st.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, im, res.Batch, BatchReview)
	enc := "big5"
	if err := im.ApplyOp(ctx, res.Batch, PlanOp{Op: "encoding", Encoding: &enc}); err != nil {
		t.Fatal(err)
	}
	if got := im.options(ctx, res.Batch); got.Store != st.ID || got.Encoding != enc {
		t.Fatalf("options %+v", got)
	}
	waitState(t, im, res.Batch, BatchReview)
	if err := im.Start(ctx, res.Batch); err != nil {
		t.Fatal(err)
	}
	waitState(t, im, res.Batch, "done")
	in := func(q library.AlbumQuery) string {
		t.Helper()
		list, err := lib.AlbumsBy(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, a := range list {
			names = append(names, a.Title)
		}
		return strings.Join(names, ",")
	}
	if got := in(library.AlbumQuery{Limit: 10, Category: c.ID}); got != "Album A" {
		t.Fatalf("in the category: %q", got)
	}
	// No category any more: the next album stays out of it.
	if err := set.SetCategory(ctx, st.ID, 0); err != nil {
		t.Fatal(err)
	}
	song("B/01.mp3", "Album B")
	res, err = im.ScanSource(ctx, set, st.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, im, res.Batch, "done")
	if got := in(library.AlbumQuery{Limit: 10, Category: c.ID}); got != "Album A" {
		t.Fatalf("in the category: %q", got)
	}
	if got := in(library.AlbumQuery{Limit: 10, Place: "local:" + strconv.FormatInt(st.ID, 10)}); got != "Album A,Album B" {
		t.Fatalf("in the folder: %q", got)
	}
	if got := in(library.AlbumQuery{Limit: 10, Place: "drive"}) + in(library.AlbumQuery{Limit: 10, Place: "local:99"}); got != "" {
		t.Fatalf("elsewhere: %q", got)
	}
	// The category removed: the source puts albums in none.
	if err := set.SetCategory(ctx, st.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.DeleteCategory(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := set.Get(ctx, st.ID); st.Category != 0 {
		t.Fatalf("still %d", st.Category)
	}
}
