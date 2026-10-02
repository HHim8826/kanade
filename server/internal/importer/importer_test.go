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
	"sync"
	"testing"
	"time"

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

	batch, n, err := im.CreateBatch(ctx, "local", "test", src)
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
	batch, _, err := im.CreateBatch(ctx, "local", "", src)
	if err != nil {
		t.Fatal(err)
	}
	b := runUntilDone(t, im, batch)
	if b.Items[0].State != StateFailed || b.Items[0].Error == "" {
		t.Fatalf("item = %+v", b.Items[0])
	}
	if n, err := im.Retry(ctx, batch); err != nil || n != 1 {
		t.Fatalf("retry: %d %v", n, err)
	}
	b = runUntilDone(t, im, batch)
	if b.Items[0].State != StatePublished {
		t.Fatalf("after retry: %+v", b.Items[0])
	}
	if tracks, _ := lib.Tracks(ctx, 10, 0); len(tracks) != 1 {
		t.Fatalf("tracks = %d", len(tracks))
	}
}

func TestEntryInputFallbacks(t *testing.T) {
	in := entryInput("Some Album/CD2/07 - Hello World.mp3", &media.Info{})
	if in.Title != "Hello World" || in.TrackNo != 7 || in.DiscNo != 2 || in.Album != "" {
		t.Fatalf("got %+v", in)
	}
	in = entryInput("tri40.mp3", &media.Info{}) // the untagged radio files from the survey
	if in.Title != "tri40" || in.TrackNo != 0 {
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
	batch, _, err := im.CreateBatch(ctx, "local", "", src)
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
