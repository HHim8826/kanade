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
	"slices"
	"strings"
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

// collectionRig imports the first round of a download of three folders (Alpha, Beta and, for a later
// round, Gamma and Delta), each its own album by its tags.
type collectionRig struct {
	ctx  context.Context
	d    *sql.DB
	lib  *library.Store
	imp  *importer.Importer
	svc  *Service
	dir  string
	path map[string]string // album -> its song, relative to dir
}

func newCollectionRig(t *testing.T) *collectionRig {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	tmp := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(tmp, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := &collectionRig{ctx: ctx, d: d, lib: library.New(d), dir: filepath.Join(tmp, "downloads", "1"), path: map[string]string{}}
	r.imp = importer.New(d, r.lib, &localDrive{}, filepath.Join(tmp, "staging"), log)
	go r.imp.Run(ctx)
	r.svc = NewService(d, nil, r.imp, filepath.Join(tmp, "downloads"), 1<<30, 0, log)
	var files []FileView
	for i, album := range []string{"Alpha", "Beta", "Gamma", "Delta"} {
		rel := "Coll/" + album + "/01.mp3"
		id3MP3(t, filepath.Join(r.dir, filepath.FromSlash(rel)), map[string]string{"TIT2": album + " song", "TPE1": "A", "TALB": album, "TRCK": "1"})
		r.path[album] = rel
		files = append(files, FileView{Index: i + 1, Path: rel, Selected: true})
	}
	raw, _ := json.Marshal(files)
	if _, err := d.Exec(`INSERT INTO downloads (id, source, name, state, dir, files, round, created_at, updated_at)
		VALUES (1, 'magnet:', 'Coll', 'downloading', ?, ?, 1, 0, 0)`, r.dir, string(raw)); err != nil {
		t.Fatal(err)
	}
	r.round(t, "Alpha", "Beta")
	return r
}

// round imports the albums' songs as the download's next round, with the grouping it has now.
func (r *collectionRig) round(t *testing.T, albums ...string) {
	t.Helper()
	row, _ := r.svc.load(r.ctx, 1)
	var paths []string
	for _, a := range albums {
		paths = append(paths, filepath.Join(r.dir, filepath.FromSlash(r.path[a])))
	}
	opts, _ := json.Marshal(map[string]any{"grouping": row.batchGrouping(paths)})
	b, _, err := r.imp.CreateBatchLinked(r.ctx, "download", "Coll", r.dir, paths, false, func(tx *sql.Tx, b int64) error {
		_, err := tx.Exec(`UPDATE import_batches SET options = ? WHERE id = ?`, string(opts), b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "import", 20*time.Second, func() bool { v, _ := r.imp.Batch(r.ctx, b); return v.State == importer.BatchDone })
	// The round's files are the download's from now on.
	r.d.Exec(`UPDATE downloads SET files = (SELECT json_group_array(CASE WHEN json_extract(f.value, '$.path') IN (SELECT value FROM json_each(?))
		THEN json_set(f.value, '$.batch', ?) ELSE json(f.value) END) FROM json_each(downloads.files) f) WHERE id = 1`,
		fmt.Sprint(`["`, strings.Join(func() []string {
			var out []string
			for _, a := range albums {
				out = append(out, r.path[a])
			}
			return out
		}(), `","`), `"]`), b)
}

func (r *collectionRig) albums() []string {
	list, _ := r.lib.Albums(r.ctx, 50, 0, false)
	var out []string
	for _, a := range list {
		out = append(out, fmt.Sprintf("%s %d", a.Title, a.Tracks))
	}
	slices.Sort(out)
	return out
}

func (r *collectionRig) rules() (grouping string, scopes int) {
	r.d.QueryRow(`SELECT grouping FROM downloads WHERE id = 1`).Scan(&grouping)
	r.d.QueryRow(`SELECT count(*) FROM album_scopes WHERE scope LIKE '%' || char(31) || char(31) || '%'`).Scan(&scopes)
	return
}

// Making a collection is one write: when saving the grouping or the scope fails, or the request
// ends, nothing of it stays and the error is told (review #87).
func TestCollectionIsSavedWhole(t *testing.T) {
	r := newCollectionRig(t)
	before := fmt.Sprint(r.albums())
	for _, trigger := range []string{
		`CREATE TRIGGER fail BEFORE UPDATE OF grouping ON downloads BEGIN SELECT RAISE(ABORT, 'injected'); END`,
		`CREATE TRIGGER fail BEFORE INSERT ON album_scopes BEGIN SELECT RAISE(ABORT, 'injected'); END`,
	} {
		if _, err := r.d.Exec(trigger); err != nil {
			t.Fatal(err)
		}
		if _, _, err := r.svc.MakeCollection(r.ctx, r.lib, 1, "The Collection", ""); err == nil || !strings.Contains(err.Error(), "injected") {
			t.Fatalf("%s: %v", trigger, err)
		}
		r.d.Exec(`DROP TRIGGER fail`)
		if g, n := r.rules(); fmt.Sprint(r.albums()) != before || g != "" || n != 0 {
			t.Fatalf("after a failure: %v %q %d", r.albums(), g, n)
		}
	}
	ended, cancel := context.WithCancel(r.ctx)
	cancel()
	if _, _, err := r.svc.MakeCollection(ended, r.lib, 1, "The Collection", ""); err == nil {
		t.Fatal("made with the request ended")
	}
	if g, n := r.rules(); fmt.Sprint(r.albums()) != before || g != "" || n != 0 {
		t.Fatalf("after the request ended: %v %q %d", r.albums(), g, n)
	}
	if _, _, err := r.svc.MakeCollection(r.ctx, r.lib, 1, "The Collection", ""); err != nil {
		t.Fatal(err)
	}
	if g, n := r.rules(); fmt.Sprint(r.albums()) != "[The Collection 2]" || !strings.Contains(g, `"collection"`) || n != 1 {
		t.Fatalf("made: %v %q %d", r.albums(), g, n)
	}
}

// Undoing a collection takes back the grouping later rounds follow and the scope that found it with
// the songs: the next round is imported by its tags again. Redoing it brings them back, and the round
// after that joins the collection (review #88).
func TestCollectionUndoRestoresLaterRounds(t *testing.T) {
	r := newCollectionRig(t)
	_, group, err := r.svc.MakeCollection(r.ctx, r.lib, 1, "The Collection", "")
	if err != nil {
		t.Fatal(err)
	}
	undo, conflicts, err := r.lib.Undo(r.ctx, group)
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("undo: %v %+v", err, conflicts)
	}
	if g, n := r.rules(); fmt.Sprint(r.albums()) != "[Alpha 1 Beta 1]" || g != "" || n != 0 {
		t.Fatalf("after undo: %v %q %d", r.albums(), g, n)
	}
	r.round(t, "Gamma")
	if got := fmt.Sprint(r.albums()); got != "[Alpha 1 Beta 1 Gamma 1]" {
		t.Fatalf("the next round after undo: %s", got)
	}
	if _, conflicts, err := r.lib.Undo(r.ctx, undo); err != nil || len(conflicts) != 0 { // redo
		t.Fatalf("redo: %v %+v", err, conflicts)
	}
	if g, n := r.rules(); fmt.Sprint(r.albums()) != "[Gamma 1 The Collection 2]" || g == "" || n != 1 {
		t.Fatalf("after redo: %v %q %d", r.albums(), g, n)
	}
	r.round(t, "Delta")
	if got := fmt.Sprint(r.albums()); got != "[Gamma 1 The Collection 3]" {
		t.Fatalf("the round after redo: %s", got)
	}
}
