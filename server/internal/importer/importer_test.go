package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/text/encoding/japanese"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/media"
)

type fakeDrive struct {
	mu       sync.Mutex
	files    map[string][]byte // drive ID -> content
	uploads  int
	failNext error
}

func (f *fakeDrive) Folder(_ context.Context, path string) (string, error) {
	return "folder:" + path, nil
}

func (f *fakeDrive) Upload(_ context.Context, u gdrive.Upload) (gdrive.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return gdrive.File{}, err
	}
	data, err := os.ReadFile(u.Path)
	if err != nil {
		return gdrive.File{}, err
	}
	sum := sha256.Sum256(data)
	f.uploads++
	id := "file" + hex.EncodeToString(sum[:4])
	f.files[id] = data
	return gdrive.File{ID: id, Name: u.Name, SHA256Checksum: hex.EncodeToString(sum[:])}, nil
}

func setup(t *testing.T) (*Importer, *library.Store, *fakeDrive) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(context.Background(), filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	lib := library.New(d)
	fd := &fakeDrive{files: map[string][]byte{}}
	return New(d, lib, fd, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil))), lib, fd
}

func copyFixture(t *testing.T, name, dst string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("../media/testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func runUntilDone(t *testing.T, im *Importer, batch int64) *BatchView {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go im.Run(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, err := im.Batch(context.Background(), batch)
		if err != nil {
			t.Fatal(err)
		}
		if b.State == "done" {
			return b
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("batch did not finish")
	return nil
}

func states(b *BatchView) map[string]string {
	out := map[string]string{}
	for _, it := range b.Items {
		out[it.RelPath] = it.State
	}
	return out
}

func TestBatchImportsSkipsAndDedupes(t *testing.T) {
	ctx := context.Background()
	im, lib, fd := setup(t)
	src := t.TempDir()
	copyFixture(t, "tone.flac", filepath.Join(src, "Album", "Disc 2", "01 first.flac"))
	copyFixture(t, "tone-cbr.mp3", filepath.Join(src, "Album", "02 second.mp3"))
	copyFixture(t, "tone.flac", filepath.Join(src, "Elsewhere", "same bytes.flac"))
	copyFixture(t, "tone.wav", filepath.Join(src, "raw.wav"))
	copyFixture(t, "tone-alac.m4a", filepath.Join(src, "lossless.m4a"))
	os.WriteFile(filepath.Join(src, "notes.txt"), []byte("not audio"), 0o644)

	batch, n, err := im.CreateBatch(ctx, "local", "test", src, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("queued %d files, want 5 (the .txt is ignored)", n)
	}
	b := runUntilDone(t, im, batch)
	got := states(b)
	want := map[string]string{
		"Album/Disc 2/01 first.flac": StatePublished,
		"Album/02 second.mp3":        StatePublished,
		"Elsewhere/same bytes.flac":  StateDuplicate, // identical bytes, identical tags
		"raw.wav":                    StateSkipped,
		"lossless.m4a":               StateSkipped, // ALAC: converted in P2
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %s, want %s", k, got[k], v)
		}
	}
	if fd.uploads != 3 { // two audio files plus one embedded cover; the duplicate is not uploaded again
		t.Fatalf("uploads = %d, want 3", fd.uploads)
	}
	albums, _ := lib.Albums(ctx, 10, 0, false)
	if len(albums) != 1 || albums[0].Title != "アルバム" || albums[0].CoverID == 0 {
		t.Fatalf("albums = %+v", albums)
	}
	d, _ := lib.Album(ctx, albums[0].ID)
	discs := map[int]bool{}
	for _, e := range d.Entries {
		discs[e.DiscNo] = true
	}
	if !discs[1] { // both fixtures carry DISCNUMBER=1, which beats the "Disc 2" folder
		t.Fatalf("entries = %+v", d.Entries)
	}
}

func TestFailedUploadCanBeRetried(t *testing.T) {
	ctx := context.Background()
	im, lib, fd := setup(t)
	src := t.TempDir()
	copyFixture(t, "tone.ogg", filepath.Join(src, "a.ogg"))
	fd.failNext = errors.New("network down")
	batch, _, err := im.CreateBatch(ctx, "local", "", src, false)
	if err != nil {
		t.Fatal(err)
	}
	b := runUntilDone(t, im, batch)
	if b.Items[0].State != StateFailed || b.Items[0].Error == "" {
		t.Fatalf("item = %+v", b.Items[0])
	}
	if n, err := im.Retry(ctx, batch); err != nil || n.Requeued != 1 {
		t.Fatalf("retry: %d %v", n, err)
	}
	b = runUntilDone(t, im, batch)
	if b.Items[0].State != StatePublished {
		t.Fatalf("after retry: %+v", b.Items[0])
	}
	if tracks, _ := lib.Tracks(ctx, 10, 0, ""); len(tracks) != 1 {
		t.Fatalf("tracks = %d", len(tracks))
	}
}

func TestFolderAlbum(t *testing.T) {
	for root, want := range map[string]string{
		"ARIA/Drama CD/ARIA The STATION Due COUR.1":                                        "ARIA The STATION Due COUR.1",
		"[2024.09.30] ATLUS Sound Team - PERSONA3 RELOAD OST [CD FLAC - 44.1 kHz, 16-bit]": "ATLUS Sound Team - PERSONA3 RELOAD OST",
		"Album (2019) [MP3 320K] (WEB)":                                                    "Album (2019)",
		"【2025.06.25】 ベスト [FLAC 96kHz／24bit]":                                              "ベスト",
		".": "", "": "", "Music": "", "x/新しいフォルダー": "", "Live (Disc 2 of 2)": "Live (Disc 2 of 2)",
	} {
		if got := FolderAlbum(root); got != want {
			t.Errorf("FolderAlbum(%q) = %q, want %q", root, got, want)
		}
	}
}

// Files whose tags name no album: an album folder without album tags is one album named after it,
// with the artist its files share; in a folder whose other files share one album they join it; in a
// folder with several albums, and at the top of the batch, they stay standalone.
func TestDefaultPlansFolderAlbums(t *testing.T) {
	probe := func(id int64, rel string, tags media.Tags) probed {
		return probed{id: id, rel: rel, info: media.Info{Tags: tags}}
	}
	plans := defaultPlans([]probed{
		probe(1, "ARIA/Due COUR.9/Disc1/DUE01.mp3", media.Tags{}),
		probe(2, "ARIA/Due COUR.9/Disc1/DUE02.mp3", media.Tags{}),
		probe(3, "Singles/a.mp3", media.Tags{Title: "A", Artist: "X"}),
		probe(4, "Singles/b.mp3", media.Tags{Title: "B", Artist: "Y"}),
		probe(5, "ARIA/Due COUR.1/Disc2/01 Track01.flac", media.Tags{Title: "T", Album: "ARIA The STATION Due COUR.1", AlbumArtist: "Hosts"}),
		probe(6, "ARIA/Due COUR.1/Disc1/DUE01.mp3", media.Tags{}),
		probe(7, "Box/a.mp3", media.Tags{Album: "Disc A"}),
		probe(8, "Box/b.mp3", media.Tags{Album: "Disc B"}),
		probe(9, "Box/c.mp3", media.Tags{}),
		probe(10, "loose.mp3", media.Tags{}),
		// Loose files of an upload, one with an album tag: the others are not on it (#53).
		probe(11, "01 Album A.mp3", media.Tags{Title: "Tagged song A", Album: "Album A", AlbumArtist: "Artist A"}),
		probe(12, "02 Loose song B.mp3", media.Tags{Title: "Unrelated song B", Artist: "Artist B"}),
		probe(13, "Music/x.mp3", media.Tags{Album: "Album X"}),
		probe(14, "Music/y.mp3", media.Tags{Title: "y", Artist: "Q"}),
	})
	byID := map[int64]Plan{}
	for _, p := range plans {
		byID[p.id] = p.plan
	}
	if p := byID[1]; p.Album != "Due COUR.9" || p.AlbumArtist != "" || p.Group == "" || p.Group != byID[2].Group ||
		p.Disc != 1 || p.Track != 1 || byID[2].Track != 2 || p.Tagged.Album != "Due COUR.9" {
		t.Fatalf("drama folder: %+v / %+v", p, byID[2])
	}
	if p := byID[3]; p.Album != "Singles" || p.AlbumArtist != "Various Artists" || p.Artist != "X" || p.Group != byID[4].Group {
		t.Fatalf("singles folder: %+v", p)
	}
	if p := byID[6]; p.Album != "ARIA The STATION Due COUR.1" || p.AlbumArtist != "Hosts" || p.Artist != "Hosts" || p.Group != byID[5].Group ||
		p.Disc != 1 || p.Track != 1 || byID[5].Disc != 2 || p.Tagged.Album != p.Album {
		t.Fatalf("joined the tagged disc: %+v / %+v", p, byID[5])
	}
	if byID[9].Album != "" || byID[9].Group != "" || byID[7].Group == byID[8].Group {
		t.Fatalf("folder of several albums: %+v", byID[9])
	}
	for _, id := range []int64{10, 12, 14} {
		if p := byID[id]; p.Album != "" || p.Group != "" || p.Tagged.Album != "" {
			t.Fatalf("loose file %d: %+v", id, p)
		}
	}
	if p := byID[12]; p.Artist != "Artist B" || p.Title != "Unrelated song B" || byID[11].Album != "Album A" || byID[11].Group == "" {
		t.Fatalf("loose files keep their own tags: %+v / %+v", p, byID[11])
	}
}

func TestEntryInputFallbacks(t *testing.T) {
	in := entryInput("Some Album/CD2/07 - Hello World.mp3", &media.Info{})
	if in.Title != "Hello World" || in.TrackNo != 7 || in.DiscNo != 2 || in.Album != "" {
		t.Fatalf("got %+v", in)
	}
	in = entryInput("tri40.mp3", &media.Info{}) // the untagged radio files from the survey: numbered by name
	if in.Title != "tri40" || in.TrackNo != 40 {
		t.Fatalf("got %+v", in)
	}
	in = entryInput("x.flac", &media.Info{Tags: media.Tags{Title: "T", AlbumArtist: "AA", Album: "Al"}})
	if in.Artist != "AA" || in.AlbumArtist != "AA" {
		t.Fatalf("artist fallback: %+v", in)
	}
	if p := drivePath(library.EntryInput{Album: "a/b", AlbumArtist: ".."}); p != "library/Unknown Artist/a／b" {
		t.Fatalf("drivePath = %q", p)
	}
}

func TestFindCoverFilePriority(t *testing.T) {
	root := t.TempDir()
	album := filepath.Join(root, "Album")
	touch := func(p string) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
	}
	touch(filepath.Join(album, "Scans", "02.jpg"))
	touch(filepath.Join(album, "Scans", "VICL-12345_01.jpg"))
	if got := findCoverFile(filepath.Join(album, "Disc 1")); filepath.Base(got) != "VICL-12345_01.jpg" {
		t.Fatalf("scans: %s", got)
	}
	touch(filepath.Join(album, "folder.png"))
	if got := findCoverFile(filepath.Join(album, "Disc 1")); filepath.Base(got) != "folder.png" {
		t.Fatalf("album folder image should win over scans: %s", got)
	}
}

// taggedMP3 prepends an ID3v2.3 tag (UTF-8 text frames) to the untagged fixture.
func taggedMP3(t *testing.T, dst string, frames map[string]string) {
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
	if err := os.WriteFile(dst, append(tag, audio...), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAlbumWithoutAlbumArtistStaysOneAlbum(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	src := t.TempDir()
	// The Rainbow single: vocals credit the singer, instrumentals the composer, no album artist.
	taggedMP3(t, filepath.Join(src, "Rainbow", "01.mp3"), map[string]string{"TIT2": "Rainbow", "TPE1": "ROUND TABLE", "TALB": "Rainbow", "TRCK": "1"})
	taggedMP3(t, filepath.Join(src, "Rainbow", "03.mp3"), map[string]string{"TIT2": "Rainbow (inst)", "TPE1": "Kitagawa", "TALB": "Rainbow", "TRCK": "3"})
	// A solo album keeps its one performer as album artist.
	taggedMP3(t, filepath.Join(src, "Solo", "Disc 1", "01.mp3"), map[string]string{"TIT2": "A", "TPE1": "Makino Yui", "TALB": "Solo", "TRCK": "1"})
	taggedMP3(t, filepath.Join(src, "Solo", "Disc 2", "01.mp3"), map[string]string{"TIT2": "B", "TPE1": "Makino Yui", "TALB": "Solo", "TRCK": "1"})
	batch, _, err := im.CreateBatch(ctx, "local", "", src, false)
	if err != nil {
		t.Fatal(err)
	}
	runUntilDone(t, im, batch)
	albums, _ := lib.Albums(ctx, 10, 0, false)
	got := map[string]string{}
	for _, a := range albums {
		got[a.Title] = fmt.Sprintf("%s/%d", a.AlbumArtist, a.Tracks)
	}
	if len(albums) != 2 || got["Rainbow"] != "Various Artists/2" || got["Solo"] != "Makino Yui/2" {
		t.Fatalf("albums = %v", got)
	}
}

func TestSpokenWordDetection(t *testing.T) {
	cases := []struct {
		rel  string
		tags media.Tags
		kind string
	}{
		{"ARIA/Drama CD/ARIA The NATURAL Drama CD I/01 - Navigation01.flac", media.Tags{Title: "x"}, "spoken"},
		{"Spice and Wolf - Wolf's Spicy-Radio 1 (Radio Drama DJCD)/CD 1/01.flac", media.Tags{Title: "x"}, "spoken"},
		{"x/01.flac", media.Tags{Title: "x", Genre: "Spoken"}, "spoken"},
		{"x/01.flac", media.Tags{Title: "x", Album: "みらくるあどばんすドラマCD 第1話"}, "spoken"},
		{"ARIA/OST/ARIA The ANIMATION Original Soundtrack/01.flac", media.Tags{Title: "x", Genre: "Anime"}, ""},
		{"x/01.flac", media.Tags{Title: "Radiohead tribute", Genre: "Rock"}, ""}, // a title is not evidence
		{"Radiohead/OK Computer/01 Airbag.flac", media.Tags{Title: "Airbag", Album: "OK Computer"}, ""},
		{"x/01.flac", media.Tags{Title: "x", Album: "Radioactive"}, ""},
	}
	for _, c := range cases {
		if got := entryInput(c.rel, &media.Info{Tags: c.tags}).Kind; got != c.kind {
			t.Errorf("%s (%+v): kind %q, want %q", c.rel, c.tags, got, c.kind)
		}
	}
}

func TestLyricsFromLRCFile(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	src := t.TempDir()
	taggedMP3(t, filepath.Join(src, "01 song.mp3"), map[string]string{"TIT2": "Song", "TPE1": "A"})
	sjis, _ := japanese.ShiftJIS.NewEncoder().Bytes([]byte("[00:01.00]一行目\r\n[00:02.00]二行目\r\n"))
	os.WriteFile(filepath.Join(src, "01 song.lrc"), sjis, 0o644)
	batch, _, err := im.CreateBatch(ctx, "local", "", src, false)
	if err != nil {
		t.Fatal(err)
	}
	b := runUntilDone(t, im, batch)
	if len(b.Items) != 1 || b.Items[0].State != StatePublished { // the .lrc is not an import item
		t.Fatalf("items %+v", b.Items)
	}
	tracks, _ := lib.Tracks(ctx, 10, 0, "")
	l, err := lib.Lyrics(ctx, tracks[0].ID)
	if err != nil || l == nil || !l.Synced || l.Source != library.LyricsLRC || l.Text != "[00:01.00]一行目\n[00:02.00]二行目" {
		t.Fatalf("lyrics %+v %v", l, err)
	}
}

func TestOriginalTagsRestoreAfterEdit(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	src := filepath.Join(t.TempDir(), "Album")
	os.MkdirAll(src, 0o755)
	taggedMP3(t, filepath.Join(src, "03 x.mp3"), map[string]string{"TIT2": "Song", "TPE1": "A", "TALB": "Al", "TRCK": "3"})
	batch, _, _ := im.CreateBatch(ctx, "local", "", src, false)
	runUntilDone(t, im, batch)
	albums, _ := lib.Albums(ctx, 10, 0, false)
	if len(albums) != 1 {
		t.Fatalf("albums %+v", albums)
	}
	d, _ := lib.Album(ctx, albums[0].ID)
	e := d.Entries[0]
	if !d.Original {
		t.Fatal("imported album not marked original")
	}
	lib.EditAlbum(ctx, d.ID, library.AlbumEdit{Title: library.Str("Renamed"),
		Entries: []library.EntryEdit{{EntryID: e.EntryID, TrackEdit: library.TrackEdit{Title: library.Str("Changed"), Artist: library.Str("B")}}}})
	o, err := im.Original(ctx, e.TrackID)
	if err != nil || o == nil || o.Title != "Song" || o.Artist != "A" || o.Album != "Al" || o.TrackNo != 3 {
		t.Fatalf("original = %+v %v", o, err)
	}
	if _, err := lib.RestoreAlbum(ctx, d.ID, im.Original); err != nil {
		t.Fatal(err)
	}
	d, _ = lib.Album(ctx, d.ID)
	if d.Title != "Al" || d.Entries[0].Title != "Song" || d.Entries[0].Artist != "A" {
		t.Fatalf("restored %+v", d)
	}
	// Importing the folder again finds the same entry.
	batch, _, _ = im.CreateBatch(ctx, "local", "", src, false)
	if b := runUntilDone(t, im, batch); b.Items[0].State != StateDuplicate {
		t.Fatalf("re-import %+v", b.Items)
	}
}

// A retry looks at each failed file first (review #57): one still there is imported again; one
// gone that another import has in the library (a later round brought an earlier round's CUE sheet
// again) is a duplicate; one gone from a download is fetched again by it; one gone for good stays
// failed and says why.
func TestRetryTellsGoneFiles(t *testing.T) {
	ctx := context.Background()
	im, _, _ := setup(t)
	dir := t.TempDir()
	exec := func(q string, args ...any) int64 {
		t.Helper()
		r, err := im.db.Exec(q, args...)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := r.LastInsertId()
		return id
	}
	batch := func(kind string) int64 {
		return exec(`INSERT INTO import_batches (kind, source, state, created_at) VALUES (?, 'x', 'done', 0)`, kind)
	}
	item := func(batch int64, name, state, why string) int64 {
		p := filepath.Join(dir, name)
		return exec(`INSERT INTO import_items (batch_id, local_path, rel_path, state, role, error, updated_at) VALUES (?, ?, ?, ?, 'sidecar', ?, 0)`,
			batch, p, name, state, why)
	}
	gone := "open " + filepath.Join(dir, "x") + ": no such file or directory"
	os.WriteFile(filepath.Join(dir, "there.log"), []byte("log"), 0o600)
	os.WriteFile(filepath.Join(dir, "back.cue"), []byte("fragment"), 0o600) // missing at import, back since

	earlier, later := batch("download"), batch("download")
	item(earlier, "CD1/album.cue", "published", "")
	dup := item(later, "CD1/album.cue", "failed", gone)
	there := item(later, "there.log", "failed", "upload failed: EOF")
	back := item(later, "back.cue", "failed", gone)
	lost := item(later, "CD2/album.log", "failed", gone)
	var asked []string
	im.Refetch = func(_ context.Context, b int64, paths []string) error {
		if b != later {
			t.Fatalf("batch %d", b)
		}
		asked = paths
		return nil
	}
	res, err := im.Retry(ctx, later)
	if err != nil || res != (RetryResult{Requeued: 1, Saved: 1, Fetching: 2}) {
		t.Fatalf("retry %+v %v", res, err)
	}
	if len(asked) != 2 || filepath.Base(asked[0]) != "back.cue" || filepath.Base(asked[1]) != "album.log" {
		t.Fatalf("fetched again: %v", asked)
	}
	state := func(id int64) (s, why string) {
		im.db.QueryRow(`SELECT state, error FROM import_items WHERE id = ?`, id).Scan(&s, &why)
		return
	}
	if s, _ := state(dup); s != StateDuplicate {
		t.Fatalf("saved elsewhere: %s", s)
	}
	if s, _ := state(there); s != "pending" {
		t.Fatalf("still there: %s", s)
	}
	for _, id := range []int64{back, lost} {
		if s, why := state(id); s != "failed" || !strings.Contains(why, "fetching it again") {
			t.Fatalf("fetched again: %s %q", s, why)
		}
	}

	// Without a download to fetch it, a file gone stays failed, saying why.
	im.db.Exec(`UPDATE import_batches SET state = 'done'`)
	up := batch("upload")
	gonePerm := item(up, "CD3/album.log", "failed", gone)
	res, err = im.Retry(ctx, up)
	if err != nil || res != (RetryResult{Lost: 1}) {
		t.Fatalf("upload retry %+v %v", res, err)
	}
	if s, why := state(gonePerm); s != "failed" || !strings.Contains(why, "import it again, or discard it") {
		t.Fatalf("gone for good: %s %q", s, why)
	}
	im.Refetch = func(context.Context, int64, []string) error { return errors.New("its download was canceled") }
	res, _ = im.Retry(ctx, later)
	if s, why := state(lost); res.Lost != 2 || s != "failed" || !strings.Contains(why, "its download was canceled") {
		t.Fatalf("download cannot: %+v %s %q", res, s, why)
	}
}
