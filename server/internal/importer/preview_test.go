package importer

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// waitState waits for the worker to move a batch to state.
func waitState(t *testing.T, im *Importer, batch int64, state string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, err := im.Batch(context.Background(), batch)
		if err != nil {
			t.Fatal(err)
		}
		if b.State == state {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := im.Batch(context.Background(), batch)
	t.Fatalf("batch is %s, want %s", b.State, state)
}

func startWorker(t *testing.T, im *Importer) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go im.Run(ctx)
}

func group(t *testing.T, p *Preview, album string) PreviewGroup {
	t.Helper()
	for _, g := range p.Groups {
		if g.Album == album {
			return g
		}
	}
	t.Fatalf("no group %q in %+v", album, p.Groups)
	return PreviewGroup{}
}

func TestPreviewEditAndRun(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	startWorker(t, im)
	src := t.TempDir()
	taggedMP3(t, filepath.Join(src, "Live/01.mp3"), map[string]string{"TIT2": "Opening", "TPE1": "A", "TALB": "Live", "TRCK": "1"})
	taggedMP3(t, filepath.Join(src, "Live/02.mp3"), map[string]string{"TIT2": "Song", "TPE1": "A", "TALB": "Live", "TRCK": "2"})
	taggedMP3(t, filepath.Join(src, "Live/03 bonus.mp3"), map[string]string{"TIT2": "Bonus", "TPE1": "A", "TALB": "Live"})
	taggedMP3(t, filepath.Join(src, "loose.mp3"), map[string]string{"TIT2": "Loose", "TPE1": "B"})

	batch, n, err := im.CreateBatch(ctx, "local", "test", src, true)
	if err != nil || n != 4 {
		t.Fatalf("create: %d %v", n, err)
	}
	waitState(t, im, batch, BatchReview)
	p, err := im.Preview(ctx, batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Groups) != 1 || len(p.Standalone) != 1 {
		t.Fatalf("groups %+v standalone %+v", p.Groups, p.Standalone)
	}
	g := group(t, p, "Live")
	if g.AlbumArtist != "A" || len(g.Items) != 3 || g.Existing != nil {
		t.Fatalf("group %+v", g)
	}
	var bonus int64
	for _, it := range g.Items {
		if it.Plan.Title == "Bonus" {
			bonus = it.ID
			if it.Plan.Track != 3 { // from the file name
				t.Fatalf("bonus plan %+v", it.Plan)
			}
		}
	}

	// Nothing runs while the batch waits for review.
	time.Sleep(100 * time.Millisecond)
	if tracks, _ := lib.Tracks(ctx, 10, 0); len(tracks) != 0 {
		t.Fatal("files imported before the review was confirmed")
	}

	str := func(s string) *string { return &s }
	if err := im.ApplyOp(ctx, batch, PlanOp{Op: "group", Group: g.Key, Album: str("Live 2004"), Date: str("2004")}); err != nil {
		t.Fatal(err)
	}
	if err := im.ApplyOp(ctx, batch, PlanOp{Op: "move", Items: []int64{bonus}, Into: "new", Album: str("Bonus Disc"), AlbumArtist: str("A")}); err != nil {
		t.Fatal(err)
	}
	if err := im.ApplyOp(ctx, batch, PlanOp{Op: "items", Items: []int64{p.Standalone[0].ID}, Title: str("Loose (edit)")}); err != nil {
		t.Fatal(err)
	}
	if err := im.ApplyOp(ctx, batch, PlanOp{Op: "group", Group: g.Key, Album: str(" ")}); !errors.Is(err, ErrBadOp) {
		t.Fatalf("blank album: %v", err)
	}
	if err := im.ApplyOp(ctx, batch, PlanOp{Op: "nope"}); !errors.Is(err, ErrBadOp) {
		t.Fatalf("unknown op: %v", err)
	}
	p, _ = im.Preview(ctx, batch)
	if len(p.Groups) != 2 || len(group(t, p, "Bonus Disc").Items) != 1 || group(t, p, "Live 2004").Date != "2004" {
		t.Fatalf("after edits %+v", p.Groups)
	}

	if err := im.Start(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if err := im.Start(ctx, batch); !errors.Is(err, ErrNotInReview) {
		t.Fatalf("second start: %v", err)
	}
	waitState(t, im, batch, BatchDone)
	albums, _ := lib.Albums(ctx, 10, 0, false)
	if len(albums) != 2 {
		t.Fatalf("albums %+v", albums)
	}
	var live int64
	for _, a := range albums {
		if a.Title == "Live 2004" {
			live = a.ID
		}
	}
	d, _ := lib.Album(ctx, live)
	if d == nil || len(d.Entries) != 2 || d.Date != "2004" {
		t.Fatalf("Live 2004 = %+v", d)
	}
	if r, _ := lib.Search(ctx, "loose edit", 10); len(r.Tracks) != 1 {
		t.Fatal("edited title not imported")
	}

	// The same folder again, without preview: every file is recognized, no album is added even
	// though its tags still say "Live".
	again, _, _ := im.CreateBatch(ctx, "local", "test", src, false)
	waitState(t, im, again, BatchDone)
	b, _ := im.Batch(ctx, again)
	if b.Counts[StateDuplicate] != 4 {
		t.Fatalf("re-import counts %+v", b.Counts)
	}
	if albums, _ := lib.Albums(ctx, 10, 0, false); len(albums) != 2 {
		t.Fatalf("re-import made albums: %+v", albums)
	}
}

func TestPreviewFoldersExcludeCancel(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	startWorker(t, im)
	var done []string
	im.OnBatchDone = func(_ context.Context, kind, source string, failed int) { done = append(done, kind+":"+source) }
	src := t.TempDir()
	// Untagged files in two folders, and the same album tag in two folders.
	taggedMP3(t, filepath.Join(src, "Radio 01/a.mp3"), map[string]string{"TIT2": "a"})
	taggedMP3(t, filepath.Join(src, "Radio 01/b.mp3"), map[string]string{"TIT2": "b"})
	taggedMP3(t, filepath.Join(src, "Radio 02/c.mp3"), map[string]string{"TIT2": "c"})
	taggedMP3(t, filepath.Join(src, "Best/A/x.mp3"), map[string]string{"TIT2": "x", "TALB": "Best", "TPE2": "Z"})
	taggedMP3(t, filepath.Join(src, "Best/B/y.mp3"), map[string]string{"TIT2": "y", "TALB": "Best", "TPE2": "Z"})
	batch, _, _ := im.CreateBatch(ctx, "upload", "group-1", src, true)
	waitState(t, im, batch, BatchReview)
	p, _ := im.Preview(ctx, batch)
	if len(p.Groups) != 1 || len(p.Standalone) != 3 { // "Best" is one album by its tags
		t.Fatalf("before %+v / %+v", p.Groups, p.Standalone)
	}
	if err := im.ApplyOp(ctx, batch, PlanOp{Op: "folders"}); err != nil {
		t.Fatal(err)
	}
	p, _ = im.Preview(ctx, batch)
	if len(p.Groups) != 4 || len(group(t, p, "Radio 01").Items) != 2 {
		t.Fatalf("by folder %+v", p.Groups)
	}
	// The two "Best" folders keep their tag name; the second becomes an album of its own.
	var best []PreviewGroup
	for _, g := range p.Groups {
		if g.Album == "Best" {
			best = append(best, g)
		}
	}
	if len(best) != 2 || best[0].NewAlbum == best[1].NewAlbum {
		t.Fatalf("best groups %+v", best)
	}
	c := group(t, p, "Radio 02").Items[0].ID
	if err := im.ApplyOp(ctx, batch, PlanOp{Op: "exclude", Items: []int64{c}}); err != nil {
		t.Fatal(err)
	}
	p, _ = im.Preview(ctx, batch)
	if len(p.Groups) != 3 || len(p.Other) != 1 || p.Other[0].State != StateExcluded {
		t.Fatalf("after exclude %+v other %+v", p.Groups, p.Other)
	}
	if err := im.Start(ctx, batch); err != nil {
		t.Fatal(err)
	}
	waitState(t, im, batch, BatchDone)
	if albums, _ := lib.Albums(ctx, 10, 0, false); len(albums) != 3 {
		t.Fatalf("albums %+v", albums)
	}
	if len(done) != 1 || done[0] != "upload:group-1" {
		t.Fatalf("done callbacks %v", done)
	}

	// A batch can be dropped while it waits. (Uploads that went in were deleted: they are copies.)
	src2 := t.TempDir()
	taggedMP3(t, filepath.Join(src2, "d.mp3"), map[string]string{"TIT2": "d"})
	taggedMP3(t, filepath.Join(src2, "e.mp3"), map[string]string{"TIT2": "e"})
	b2, _, _ := im.CreateBatch(ctx, "upload", "group-2", src2, true)
	waitState(t, im, b2, BatchReview)
	if err := im.Cancel(ctx, b2); err != nil {
		t.Fatal(err)
	}
	if b, _ := im.Batch(ctx, b2); b.State != BatchCanceled || b.Counts[StateExcluded] != 2 {
		t.Fatalf("canceled %+v", b)
	}
	if len(done) != 2 || done[1] != "upload:group-2" {
		t.Fatalf("cancel did not release the upload: %v", done)
	}
}

func TestPreviewEncoding(t *testing.T) {
	ctx := context.Background()
	im, _, _ := setup(t)
	startWorker(t, im)
	src := t.TempDir()
	gbk, _ := simplifiedchinese.GBK.NewEncoder().Bytes([]byte("测试歌曲"))
	writeLatin1MP3(t, filepath.Join(src, "01.mp3"), gbk)
	batch, _, _ := im.CreateBatch(ctx, "local", "", src, true)
	waitState(t, im, batch, BatchReview)
	p, _ := im.Preview(ctx, batch)
	if p.Standalone[0].Plan.Title == "测试歌曲" || len(p.Detected) == 0 {
		t.Fatalf("auto guess %+v detected %v", p.Standalone[0].Plan, p.Detected)
	}
	gb := "gbk"
	if err := im.ApplyOp(ctx, batch, PlanOp{Op: "encoding", Encoding: &gb}); err != nil {
		t.Fatal(err)
	}
	p, _ = im.Preview(ctx, batch)
	if p.Encoding != "gbk" || p.Standalone[0].Plan.Title != "测试歌曲" {
		t.Fatalf("after gbk %+v", p.Standalone[0].Plan)
	}
}

// writeLatin1MP3 writes an MP3 whose title frame declares ISO-8859-1 but holds other bytes.
func writeLatin1MP3(t *testing.T, dst string, title []byte) {
	t.Helper()
	payload := append([]byte{0}, title...)
	n := len(payload)
	body := append([]byte{'T', 'I', 'T', '2', byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n), 0, 0}, payload...)
	m := len(body)
	tag := append([]byte{'I', 'D', '3', 3, 0, 0, byte(m >> 21 & 0x7f), byte(m >> 14 & 0x7f), byte(m >> 7 & 0x7f), byte(m & 0x7f)}, body...)
	audio, _ := os.ReadFile("../media/testdata/tone-notag.mp3")
	os.MkdirAll(filepath.Dir(dst), 0o755)
	if err := os.WriteFile(dst, append(tag, audio...), 0o644); err != nil {
		t.Fatal(err)
	}
}

type zipEntry struct {
	name   string // as stored
	data   []byte
	utf8   bool
	method uint16
}

func writeZip(t *testing.T, dst string, entries []zipEntry) {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: e.method, NonUTF8: !e.utf8}
		f, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		f.Write(e.data)
	}
	w.Close()
	os.MkdirAll(filepath.Dir(dst), 0o755)
	if err := os.WriteFile(dst, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestZipAndSidecars(t *testing.T) {
	ctx := context.Background()
	im, lib, fd := setup(t)
	startWorker(t, im)
	src := t.TempDir()
	tmp := t.TempDir()
	taggedMP3(t, filepath.Join(tmp, "1.mp3"), map[string]string{"TIT2": "One", "TPE1": "A", "TALB": "Zipped", "TRCK": "1"})
	taggedMP3(t, filepath.Join(tmp, "2.mp3"), map[string]string{"TIT2": "Two", "TPE1": "A", "TALB": "Zipped", "TRCK": "2"})
	one, _ := os.ReadFile(filepath.Join(tmp, "1.mp3"))
	two, _ := os.ReadFile(filepath.Join(tmp, "2.mp3"))
	cover, _ := os.ReadFile("../media/testdata/cover.png")
	sjisName, _ := japanese.ShiftJIS.NewEncoder().Bytes([]byte("アルバム/02 二曲目.mp3"))
	writeZip(t, filepath.Join(src, "album.zip"), []zipEntry{
		{name: "アルバム/01 one.mp3", data: one, utf8: true, method: zip.Store},
		{name: string(sjisName), data: two, method: zip.Deflate}, // CP932 name without the UTF-8 flag
		{name: "アルバム/cover.png", data: cover, utf8: true, method: zip.Store},
		{name: "アルバム/album.cue", data: []byte("TITLE \"Zipped\"\r\n"), utf8: true, method: zip.Deflate},
		{name: "__MACOSX/アルバム/._01 one.mp3", data: []byte("junk"), utf8: true, method: zip.Store},
		{name: "アルバム/notes.txt", data: []byte("not imported"), utf8: true, method: zip.Store},
	})
	batch, n, err := im.CreateBatch(ctx, "local", "", src, true)
	if err != nil || n != 1 {
		t.Fatalf("create: %d %v", n, err)
	}
	waitState(t, im, batch, BatchReview)
	p, _ := im.Preview(ctx, batch)
	g := group(t, p, "Zipped")
	paths := []string{}
	for _, it := range g.Items {
		paths = append(paths, it.Path)
	}
	if len(g.Items) != 2 || !strings.Contains(strings.Join(paths, "|"), "album/アルバム/02 二曲目.mp3") {
		t.Fatalf("zip items %v", paths)
	}
	im.Start(ctx, batch)
	waitState(t, im, batch, BatchDone)
	b, _ := im.Batch(ctx, batch)
	if b.Counts[StatePublished] != 3 || b.Counts[StateExpanded] != 1 { // two songs and the cue sheet
		t.Fatalf("counts %+v", b.Counts)
	}
	albums, _ := lib.Albums(ctx, 10, 0, false)
	d, _ := lib.Album(ctx, albums[0].ID)
	if d.CoverID == 0 || len(d.Sidecars) != 1 || d.Sidecars[0].Name != "album.cue" {
		t.Fatalf("album %+v", d)
	}
	if _, ok := fd.files[d.Sidecars[0].DriveFileID]; !ok {
		t.Fatal("cue sheet not uploaded")
	}
	if _, err := os.Stat(im.workDir(batch)); !os.IsNotExist(err) {
		t.Fatal("extracted files not cleaned up")
	}
	if _, err := os.Stat(filepath.Join(src, "album.zip")); err != nil {
		t.Fatal("a ZIP from a server folder must stay")
	}
}

func TestZipChecks(t *testing.T) {
	ctx := context.Background()
	im, _, _ := setup(t)
	dir := t.TempDir()
	cases := map[string][]zipEntry{
		"outside it":  {{name: "../evil.mp3", data: []byte("x"), utf8: true}},
		"expands":     {{name: "bomb.flac", data: make([]byte, 1<<20), utf8: true, method: zip.Deflate}},
		"no audio":    {{name: "readme.txt", data: []byte("hi"), utf8: true}},
		"budget is 1": {{name: "a.mp3", data: []byte("0123456789"), utf8: true}},
	}
	im.Space = func(_ context.Context, need int64) error {
		if need > 5 {
			return errors.New("budget is 1 MB")
		}
		return nil
	}
	for want, entries := range cases {
		zp := filepath.Join(dir, strings.ReplaceAll(want, " ", "_")+".zip")
		writeZip(t, zp, entries)
		files, err := im.expandZip(ctx, zp, filepath.Join(dir, "out"))
		if err == nil || !strings.Contains(err.Error(), want) || len(files) != 0 {
			t.Errorf("%s: %v %v", want, files, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "evil.mp3")); !os.IsNotExist(err) {
		t.Fatal("a file escaped the archive")
	}
}
