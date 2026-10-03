package importer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/HHim8826/kanade/server/internal/ffmpeg"
)

func withFFmpeg(t *testing.T, im *Importer) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	tl := ffmpeg.Find(filepath.Join(filepath.Dir(file), "..", "..", "..", "var"))
	if tl == nil {
		t.Skip("no ffmpeg")
	}
	im.FFmpeg = tl
}

// makeAudio writes a two-second tone with FFmpeg.
func makeAudio(t *testing.T, im *Importer, dst string, args ...string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(dst), 0o755)
	cmd := exec.Command(im.FFmpeg.Path(), append(append([]string{"-v", "error", "-y", "-f", "lavfi", "-i",
		"sine=frequency=440:duration=2", "-ac", "2"}, args...), dst)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
}

func TestConvertLosslessToFLAC(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	withFFmpeg(t, im)
	startWorker(t, im)
	src := t.TempDir()
	tags := func(title string, n string) []string {
		return []string{"-metadata", "title=" + title, "-metadata", "artist=A", "-metadata", "album=Lossless", "-metadata", "track=" + n}
	}
	makeAudio(t, im, filepath.Join(src, "Lossless/01.wav"), append([]string{"-c:a", "pcm_s24le"}, tags("Wave", "1")...)...)
	makeAudio(t, im, filepath.Join(src, "Lossless/02.wv"), append([]string{"-c:a", "wavpack"}, tags("Pack", "2")...)...)
	makeAudio(t, im, filepath.Join(src, "Lossless/03.m4a"), append([]string{"-c:a", "alac"}, tags("Apple", "3")...)...)
	makeAudio(t, im, filepath.Join(src, "Lossless/04.wav"), "-c:a", "pcm_f32le")
	copyFixture(t, "cover.png", filepath.Join(src, "Lossless/cover.png"))

	batch, _, err := im.CreateBatch(ctx, "local", "", src, false)
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, im, batch, BatchDone)
	b, _ := im.Batch(ctx, batch)
	st := states(b)
	if st["Lossless/01.wav"] != StatePublished || st["Lossless/02.wv"] != StatePublished || st["Lossless/03.m4a"] != StatePublished ||
		st["Lossless/04.wav"] != StateSkipped {
		t.Fatalf("states %v %+v", st, b.Items)
	}
	albums, _ := lib.Albums(ctx, 10, 0, false)
	if len(albums) != 1 || albums[0].Title != "Lossless" || albums[0].CoverID == 0 { // the cover beside the originals
		t.Fatalf("albums %+v", albums)
	}
	d, _ := lib.Album(ctx, albums[0].ID)
	if len(d.Entries) != 3 || d.Entries[0].Title != "Wave" || d.Entries[0].Asset.Format != "flac" || d.Entries[0].Asset.BitDepth != 24 {
		t.Fatalf("entries %+v", d.Entries)
	}
	if _, err := os.Stat(im.workDir(batch)); !os.IsNotExist(err) {
		t.Fatal("converted files not cleaned up")
	}

	// The same originals again are recognized before converting.
	again, _, _ := im.CreateBatch(ctx, "local", "", src, false)
	waitState(t, im, again, BatchDone)
	b, _ = im.Batch(ctx, again)
	if b.Counts[StateDuplicate] != 3 {
		t.Fatalf("re-import %+v", b.Counts)
	}
}

const splitCue = "PERFORMER \"牧野由依\"\r\nTITLE \"テスト盤\"\r\nREM DATE 2006\r\nFILE \"image.wav\" WAVE\r\n" +
	"  TRACK 01 AUDIO\r\n    TITLE \"一曲目\"\r\n    INDEX 01 00:00:00\r\n" +
	"  TRACK 02 AUDIO\r\n    TITLE \"二曲目\"\r\n    INDEX 00 00:00:50\r\n    INDEX 01 00:00:60\r\n" +
	"  TRACK 03 AUDIO\r\n    TITLE \"三曲目\"\r\n    PERFORMER \"Guest\"\r\n    INDEX 01 00:01:30\r\n"

func TestSplitDiscImageByCue(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	withFFmpeg(t, im)
	startWorker(t, im)
	src := t.TempDir()
	// EAC style: the sheet names image.wav, the file is image.flac.
	makeAudio(t, im, filepath.Join(src, "Disc/image.flac"), "-c:a", "flac")
	os.WriteFile(filepath.Join(src, "Disc/image.cue"), []byte(splitCue), 0o644)
	copyFixture(t, "cover.png", filepath.Join(src, "Disc/folder.png"))

	batch, _, _ := im.CreateBatch(ctx, "local", "", src, true)
	waitState(t, im, batch, BatchReview)
	p, _ := im.Preview(ctx, batch)
	g := group(t, p, "テスト盤")
	if len(g.Items) != 3 || g.AlbumArtist != "牧野由依" || g.Items[2].Plan.Artist != "Guest" || g.Items[1].Plan.Title != "二曲目" {
		t.Fatalf("group %+v", g)
	}
	var image PreviewItem
	for _, it := range p.Other {
		if it.Path == "Disc/image.flac" {
			image = it
		}
	}
	if image.State != StateSplit {
		t.Fatalf("image %+v", image)
	}
	im.Start(ctx, batch)
	waitState(t, im, batch, BatchDone)
	albums, _ := lib.Albums(ctx, 10, 0, false)
	if len(albums) != 1 || albums[0].CoverID == 0 || albums[0].Date != "2006" {
		t.Fatalf("albums %+v", albums)
	}
	d, _ := lib.Album(ctx, albums[0].ID)
	// 2 s at 44.1 kHz: track 2 starts at 0.8 s (60 frames; its pregap stays with track 1), track 3 at 1.4 s.
	durs := []int64{d.Entries[0].Asset.DurationMS, d.Entries[1].Asset.DurationMS, d.Entries[2].Asset.DurationMS}
	if len(d.Entries) != 3 || durs[0] != 800 || durs[1] != 600 || durs[2] != 600 || len(d.Sidecars) != 1 {
		t.Fatalf("entries %v %+v", durs, d)
	}

	again, _, _ := im.CreateBatch(ctx, "local", "", src, false)
	waitState(t, im, again, BatchDone)
	if b, _ := im.Batch(ctx, again); states(b)["Disc/image.flac"] != StateDuplicate {
		t.Fatalf("re-import %+v", b.Items)
	}
}

func TestBadCueFailsTheImage(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	withFFmpeg(t, im)
	startWorker(t, im)
	src := t.TempDir()
	makeAudio(t, im, filepath.Join(src, "image.flac"), "-c:a", "flac", "-metadata", "album=Whole")
	os.WriteFile(filepath.Join(src, "image.cue"), []byte("FILE \"image.flac\" WAVE\r\n  TRACK 01 AUDIO\r\n    INDEX 01 00:00:00\r\n"+
		"  TRACK 02 AUDIO\r\n    INDEX 01 05:00:00\r\n"), 0o644) // track 2 starts after the end
	batch, _, _ := im.CreateBatch(ctx, "local", "", src, false)
	waitState(t, im, batch, BatchDone)
	b, _ := im.Batch(ctx, batch)
	if st := states(b); st["image.flac"] != StateFailed {
		t.Fatalf("states %v", st)
	}
	if tracks, _ := lib.Tracks(ctx, 10, 0, ""); len(tracks) != 0 {
		t.Fatal("the whole image was imported as one song")
	}
}
