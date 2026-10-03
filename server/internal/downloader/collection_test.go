package downloader

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/importer"
	"github.com/HHim8826/kanade/server/internal/library"
)

// id3MP3 writes the test tone with ID3 frames.
func id3MP3(t *testing.T, dst string, frames map[string]string) {
	t.Helper()
	var body []byte
	for id, v := range frames {
		payload := append([]byte{3}, v...)
		n := len(payload)
		body = append(body, id[0], id[1], id[2], id[3], byte(n>>24), byte(n>>16), byte(n>>8), byte(n), 0, 0)
		body = append(body, payload...)
	}
	n := len(body)
	tag := append([]byte{'I', 'D', '3', 3, 0, 0, byte(n >> 21 & 0x7f), byte(n >> 14 & 0x7f), byte(n >> 7 & 0x7f), byte(n & 0x7f)}, body...)
	audio, err := os.ReadFile("../media/testdata/tone-notag.mp3")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(dst), 0o755)
	os.WriteFile(dst, append(tag, audio...), 0o644)
}

// A download already imported by its tags (each original album apart) becomes one collection of
// named sections in one edit, which undo takes back; its later rounds then join the collection
// (review #82).
func TestImportedDownloadBecomesCollection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tmp := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(tmp, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	lib := library.New(d)
	imp := importer.New(d, lib, &localDrive{}, filepath.Join(tmp, "staging"), log)
	go imp.Run(ctx)
	svc := NewService(d, nil, imp, filepath.Join(tmp, "downloads"), 1<<30, 0, log)
	dir := filepath.Join(tmp, "downloads", "1")
	songs := []struct{ path, album, artist string }{
		{"Coll/Episode 1/Red/01.mp3", "musicbox Red", "A"}, {"Coll/Episode 1/Red/02.mp3", "musicbox Red", "B"},
		{"Coll/Episode 2/Gold/01.mp3", "Golden", "D"}, {"Coll/Episode 2/Gold/02.mp3", "Golden", "E"},
	}
	var files []FileView
	var first []string
	for i, s := range songs {
		p := filepath.Join(dir, filepath.FromSlash(s.path))
		id3MP3(t, p, map[string]string{"TIT2": fmt.Sprintf("Song %d", i), "TPE1": s.artist, "TALB": s.album, "TPE2": s.album + " AA",
			"TRCK": fmt.Sprint(i%2 + 1)})
		files = append(files, FileView{Index: i + 1, Path: s.path, Selected: true, Round: 1})
		if i < 3 {
			first = append(first, p)
		}
	}
	done := func(b int64) {
		t.Helper()
		waitFor(t, "import", 20*time.Second, func() bool { v, _ := imp.Batch(ctx, b); return v.State == importer.BatchDone })
	}
	b1, _, err := imp.CreateBatchFiles(ctx, "download", "Coll", dir, first, false)
	if err != nil {
		t.Fatal(err)
	}
	done(b1)
	for i := 0; i < 3; i++ {
		files[i].Batch = b1
	}
	files[3].Round = 0 // a later round's
	raw, _ := json.Marshal(files)
	if _, err := d.Exec(`INSERT INTO downloads (id, source, name, state, dir, files, round, created_at, updated_at)
		VALUES (1, 'magnet:', 'Coll', 'downloading', ?, ?, 1, 0, 0)`, dir, string(raw)); err != nil {
		t.Fatal(err)
	}
	albums := func() map[string]int {
		list, _ := lib.Albums(ctx, 50, 0, false)
		out := map[string]int{}
		for _, a := range list {
			out[a.Title] = a.Tracks
		}
		return out
	}
	if got := albums(); len(got) != 2 {
		t.Fatalf("by tags: %v", got)
	}

	p, err := svc.CollectionPlan(ctx, 1, "The Collection", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := lib.CheckPlan(ctx, p); err != nil {
		t.Fatal(err)
	}
	if p.Target.ID != 0 || len(p.Moves) != 3 || len(p.Emptied) != 2 || p.Sections[1] != "Episode 1" || p.Sections[2] != "Episode 2" ||
		p.Moves[2].Disc != 2 || p.Moves[2].Track != 1 {
		t.Fatalf("plan %+v", p)
	}
	album, g, err := lib.Arrange(ctx, p, "collection")
	if err != nil {
		t.Fatal(err)
	}
	if got := albums(); len(got) != 1 || got["The Collection"] != 3 {
		t.Fatalf("collection: %v", got)
	}
	if _, _, err := lib.Undo(ctx, g); err != nil {
		t.Fatal(err)
	}
	if got := albums(); len(got) != 2 || got["musicbox Red"] != 2 {
		t.Fatalf("after undo: %v", got)
	}
	if album, _, err = lib.Arrange(ctx, p, "collection"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetGrouping(ctx, 1, &importer.Grouping{Mode: importer.GroupCollection, Title: "The Collection"}); err != nil {
		t.Fatal(err)
	}
	if err := imp.SetScopeAlbum(ctx, importer.CollectionScope(dir, "The Collection"), album, "Various Artists"); err != nil {
		t.Fatal(err)
	}

	// The next round brings Episode 2's second song: it joins the collection, in its place.
	r, _ := svc.load(ctx, 1)
	last := []string{filepath.Join(dir, filepath.FromSlash(songs[3].path))}
	g2 := r.batchGrouping(last)
	if g2 == nil || g2.Slots[songs[3].path] != [2]int{2, 2} {
		t.Fatalf("round's grouping %+v", g2)
	}
	opts, _ := json.Marshal(map[string]any{"grouping": g2})
	b2, _, err := imp.CreateBatchLinked(ctx, "download", "Coll", dir, last, false, func(tx *sql.Tx, b int64) error {
		_, err := tx.Exec(`UPDATE import_batches SET options = ? WHERE id = ?`, string(opts), b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	done(b2)
	if got := albums(); len(got) != 1 || got["The Collection"] != 4 {
		t.Fatalf("after the next round: %v", got)
	}
	dd, _ := lib.Album(ctx, album)
	for _, e := range dd.Entries {
		if e.Title == "Song 3" && (e.DiscNo != 2 || e.TrackNo != 2 || e.Artist != "E") {
			t.Fatalf("the next round's song: %+v", e)
		}
	}
}

// Songs whose albums were merged and then removed are on no album: the collection still gathers
// every one of them, each in its section and place, and a second look finds nothing left to add.
func TestCollectionGathersSongsOnNoAlbum(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tmp := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(tmp, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	lib := library.New(d)
	imp := importer.New(d, lib, &localDrive{}, filepath.Join(tmp, "staging"), log)
	go imp.Run(ctx)
	svc := NewService(d, nil, imp, filepath.Join(tmp, "downloads"), 1<<30, 0, log)
	dir := filepath.Join(tmp, "downloads", "1")
	songs := []struct{ path, album string }{
		{"Coll/Episode 1/01.mp3", "Red"}, {"Coll/Episode 1/02.mp3", "Red"}, {"Coll/Episode 2/01.mp3", "Gold"},
	}
	var files []FileView
	var paths []string
	for i, s := range songs {
		p := filepath.Join(dir, filepath.FromSlash(s.path))
		id3MP3(t, p, map[string]string{"TIT2": fmt.Sprintf("Song %d", i), "TPE1": "A", "TALB": s.album, "TRCK": fmt.Sprint(i + 1)})
		paths = append(paths, p)
		files = append(files, FileView{Index: i + 1, Path: s.path, Selected: true, Round: 1})
	}
	b, _, err := imp.CreateBatchFiles(ctx, "download", "Coll", dir, paths, false)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "import", 20*time.Second, func() bool { v, _ := imp.Batch(ctx, b); return v.State == importer.BatchDone })
	for i := range files {
		files[i].Batch = b
	}
	raw, _ := json.Marshal(files)
	if _, err := d.Exec(`INSERT INTO downloads (id, source, name, state, dir, files, round, created_at, updated_at)
		VALUES (1, 'magnet:', 'Coll', 'completed', ?, ?, 1, 0, 0)`, dir, string(raw)); err != nil {
		t.Fatal(err)
	}
	list, _ := lib.Albums(ctx, 50, 0, false)
	var ids []int64
	for _, a := range list {
		ids = append(ids, a.ID)
	}
	if len(ids) != 2 {
		t.Fatalf("by tags: %+v", list)
	}
	merged, _, err := lib.MergeAlbums(ctx, library.MergeRequest{Albums: ids, Title: "Odd title", AlbumArtist: "A"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lib.RemoveAlbums(ctx, []int64{merged}); err != nil {
		t.Fatal(err)
	}
	if list, _ := lib.Albums(ctx, 50, 0, false); len(list) != 0 {
		t.Fatalf("after removing: %+v", list)
	}

	p, err := svc.CollectionPlan(ctx, 1, "The Collection", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := lib.CheckPlan(ctx, p); err != nil {
		t.Fatal(err)
	}
	if len(p.Moves) != 0 || len(p.Adds) != 3 || len(p.Emptied) != 0 || p.Sections[1] != "Episode 1" || p.Sections[2] != "Episode 2" {
		t.Fatalf("plan %+v", p)
	}
	for _, a := range p.Adds {
		if !a.Loose {
			t.Fatalf("not told apart as on no album: %+v", a)
		}
	}
	album, _, err := lib.Arrange(ctx, p, "collection")
	if err != nil {
		t.Fatal(err)
	}
	if err := imp.SetScopeAlbum(ctx, importer.CollectionScope(dir, "The Collection"), album, "Various Artists"); err != nil {
		t.Fatal(err)
	}
	dd, _ := lib.Album(ctx, album)
	want := map[string][2]int{"Song 0": {1, 1}, "Song 1": {1, 2}, "Song 2": {2, 1}}
	if len(dd.Entries) != 3 {
		t.Fatalf("collection: %+v", dd.Entries)
	}
	for _, e := range dd.Entries {
		if w := want[e.Title]; e.DiscNo != w[0] || e.TrackNo != w[1] {
			t.Fatalf("%s at %d-%d, want %v", e.Title, e.DiscNo, e.TrackNo, w)
		}
	}
	if again, err := svc.CollectionPlan(ctx, 1, "The Collection", ""); err != nil || len(again.Adds)+len(again.Moves) != 0 || again.Target.ID != album {
		t.Fatalf("second look: %+v %v", again, err)
	}
}
