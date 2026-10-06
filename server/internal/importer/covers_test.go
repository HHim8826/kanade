package importer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// pngOf is a small PNG of one color: each color is another image.
func pngOf(t *testing.T, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := range 4 {
		for y := range 4 {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// albumCover is the content hash of an album's cover ("" without one).
func albumCover(t *testing.T, im *Importer, title string) string {
	t.Helper()
	var sha string
	im.db.QueryRow(`SELECT coalesce(c.sha256, '') FROM albums a LEFT JOIN covers c ON c.id = a.cover_id WHERE a.title = ?`, title).Scan(&sha)
	return sha
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func shaOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// A cover that could not be stored is not taken for no cover (review #150): the next song of the
// folder stores it.
func TestCoverFailureIsTriedAgain(t *testing.T) {
	ctx := context.Background()
	im, _, fd := setup(t)
	src := t.TempDir()
	taggedMP3(t, filepath.Join(src, "Album", "01.mp3"), map[string]string{"TIT2": "One", "TPE1": "A", "TALB": "Album", "TRCK": "1"})
	taggedMP3(t, filepath.Join(src, "Album", "02.mp3"), map[string]string{"TIT2": "Two", "TPE1": "A", "TALB": "Album", "TRCK": "2"})
	art := pngOf(t, color.RGBA{200, 0, 0, 255})
	os.WriteFile(filepath.Join(src, "Album", "cover.png"), art, 0o644)
	fd.failCovers = 1
	batch, _, err := im.CreateBatch(ctx, "local", "", src, false)
	if err != nil {
		t.Fatal(err)
	}
	runUntilDone(t, im, batch)
	if got := albumCover(t, im, "Album"); got != shaOf(art) {
		t.Fatalf("cover %q after one failed upload", got)
	}
}

// Songs dropped straight into the inbox all have the inbox for a folder: each batch looks at what
// is there then, not at what an earlier one found (review #150).
func TestInboxRootCoverIsPerBatch(t *testing.T) {
	ctx := context.Background()
	im, _, _ := setup(t)
	dd := newInboxDrive()
	im.drive = dd
	startWorker(t, im)
	inbox, _ := im.drive.Folder(ctx, InboxFolder)
	tmp := t.TempDir()
	drop := func(name, album string, art []byte) int64 {
		t.Helper()
		p := filepath.Join(tmp, name)
		taggedMP3(t, p, map[string]string{"TIT2": name, "TPE1": "A", "TALB": album, "TRCK": "1"})
		data, _ := os.ReadFile(p)
		dd.add(inbox, name, data)
		dd.add(inbox, "cover.png", art)
		if n, err := im.ScanInbox(ctx); err != nil || n != 1 {
			t.Fatalf("scan: %d %v", n, err)
		}
		var batch int64
		im.db.QueryRow(`SELECT max(id) FROM import_batches WHERE kind = 'inbox'`).Scan(&batch)
		waitState(t, im, batch, BatchDone)
		return batch
	}
	red, blue := pngOf(t, color.RGBA{200, 0, 0, 255}), pngOf(t, color.RGBA{0, 0, 200, 255})
	drop("first.mp3", "First", red)
	// The first image is gone from the inbox by then (moved to 已處理).
	done := dd.add(inbox, "done", nil)
	for _, f := range must(dd.Children(ctx, inbox)) {
		if f.Name == "cover.png" {
			dd.Move(ctx, f.ID, done, []string{inbox})
		}
	}
	drop("second.mp3", "Second", blue)
	if got := albumCover(t, im, "First"); got != shaOf(red) {
		t.Fatalf("first album: %q", got)
	}
	if got := albumCover(t, im, "Second"); got != shaOf(blue) {
		t.Fatalf("second album took %q, want its own", got)
	}
	// A batch is marked done before what it leaves is cleared up.
	left := func() int {
		im.mu.Lock()
		defer im.mu.Unlock()
		return len(im.covers)
	}
	for deadline := time.Now().Add(5 * time.Second); left() != 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if n := left(); n != 0 {
		t.Fatalf("finished batches left %d covers", n)
	}
}
