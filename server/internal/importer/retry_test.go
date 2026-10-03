package importer

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/HHim8826/kanade/server/internal/library"
)

func itemsOf(t *testing.T, im *Importer, batch int64) map[string]struct {
	id                  int64
	state, path, source string
} {
	t.Helper()
	rows, err := im.db.Query(`SELECT id, rel_path, state, local_path, source_path FROM import_items WHERE batch_id = ?`, batch)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]struct {
		id                  int64
		state, path, source string
	}{}
	for rows.Next() {
		var rel string
		var v struct {
			id                  int64
			state, path, source string
		}
		rows.Scan(&v.id, &rel, &v.state, &v.path, &v.source)
		out[rel] = v
	}
	return out
}

// A converted file whose FLAC and original are both gone is fetched again by its original, the file
// of the download, then converted again; its plan stays (review #65).
func TestLostConvertedFileIsFetchedByItsOriginal(t *testing.T) {
	ctx := context.Background()
	im, lib, fd := setup(t)
	withFFmpeg(t, im)
	startWorker(t, im)
	src := t.TempDir()
	wav := filepath.Join(src, "Box/Album/01.wav")
	makeAudio(t, im, wav, "-c:a", "pcm_s16le", "-metadata", "title=Wave", "-metadata", "album=W")
	fd.failNext = errors.New("upload failed")
	batch, _, _ := im.CreateBatch(ctx, "download", "Box", src, false)
	waitState(t, im, batch, BatchDone)
	it := itemsOf(t, im, batch)["Box/Album/01.wav"]
	if it.state != StateFailed || it.source != wav || filepath.Ext(it.path) != ".flac" {
		t.Fatalf("after the failed upload: %+v", it)
	}
	im.db.Exec(`UPDATE import_items SET plan = json_set(plan, '$.title', 'Kept title') WHERE id = ?`, it.id)
	os.Remove(it.path)
	data, _ := os.ReadFile(wav)
	os.Remove(wav)

	var asked []string
	im.Refetch = func(_ context.Context, b int64, paths []string) error {
		asked = paths
		return nil
	}
	res, err := im.Retry(ctx, batch)
	if err != nil || res != (RetryResult{Fetching: 1}) || !slices.Equal(asked, []string{wav}) {
		t.Fatalf("retry %+v %v, fetched %v", res, err, asked)
	}
	// The download has it again.
	os.WriteFile(wav, data, 0o644)
	res, err = im.RetryFetched(ctx, batch, []string{wav})
	if err != nil || res.Requeued != 1 {
		t.Fatalf("after fetching: %+v %v", res, err)
	}
	waitState(t, im, batch, BatchDone)
	if it := itemsOf(t, im, batch)["Box/Album/01.wav"]; it.state != StatePublished {
		t.Fatalf("after converting again: %+v", it)
	}
	tracks, _ := lib.Tracks(ctx, 10, 0, "")
	if len(tracks) != 1 || tracks[0].Asset.Format != "flac" || tracks[0].Title != "Kept title" {
		t.Fatalf("tracks %+v", tracks)
	}
}

// A song cut from a disc image is cut again when it is lost, the image still there: the other
// songs, in the library, are not imported again and no item is added; with the image gone too, the
// image is what is fetched again (review #65).
func TestLostCutSongIsCutAgain(t *testing.T) {
	ctx := context.Background()
	im, lib, fd := setup(t)
	withFFmpeg(t, im)
	startWorker(t, im)
	src := t.TempDir()
	image := filepath.Join(src, "Disc/image.flac")
	makeAudio(t, im, image, "-c:a", "flac")
	os.WriteFile(filepath.Join(src, "Disc/image.cue"), []byte(splitCue), 0o644)
	fd.failNext = errors.New("upload failed")
	batch, _, _ := im.CreateBatch(ctx, "download", "Disc", src, false)
	waitState(t, im, batch, BatchDone)
	before := itemsOf(t, im, batch)
	first := before["Disc/01 一曲目.flac"]
	if first.state != StateFailed || before["Disc/02 二曲目.flac"].state != StatePublished || before["Disc/image.cue"].state != StatePublished {
		t.Fatalf("first import %+v", before)
	}
	os.Remove(first.path)
	res, err := im.Retry(ctx, batch)
	if err != nil || res.Requeued != 1 {
		t.Fatalf("retry %+v %v", res, err)
	}
	waitState(t, im, batch, BatchDone)
	after := itemsOf(t, im, batch)
	if len(after) != len(before) || after["Disc/01 一曲目.flac"].state != StatePublished || after["Disc/01 一曲目.flac"].id != first.id ||
		after["Disc/image.flac"].state != StateSplit {
		t.Fatalf("after cutting again %+v", after)
	}
	if tracks, _ := lib.Tracks(ctx, 10, 0, ""); len(tracks) != 3 {
		t.Fatalf("tracks %d", len(tracks))
	}

	// Lost with its image: the image is fetched again.
	fd.failNext = errors.New("upload failed")
	other := t.TempDir()
	image2 := filepath.Join(other, "Disc2/image.flac")
	makeAudio(t, im, image2, "-c:a", "flac", "-af", "volume=0.5")
	os.WriteFile(filepath.Join(other, "Disc2/image.cue"), []byte(splitCue), 0o644)
	b2, _, _ := im.CreateBatch(ctx, "download", "Disc2", other, false)
	waitState(t, im, b2, BatchDone)
	lost := itemsOf(t, im, b2)["Disc2/01 一曲目.flac"]
	os.Remove(lost.path)
	os.Remove(image2)
	var asked []string
	im.Refetch = func(_ context.Context, b int64, paths []string) error {
		asked = paths
		return nil
	}
	if res, err := im.Retry(ctx, b2); err != nil || res != (RetryResult{Fetching: 1}) || !slices.Equal(asked, []string{image2}) {
		t.Fatalf("retry without the image %+v %v %v", res, err, asked)
	}
}

// A file unpacked from an archive is unpacked again when it is lost; with the archive gone too,
// the archive is what is fetched again (review #65).
func TestLostUnpackedFileIsUnpackedAgain(t *testing.T) {
	ctx := context.Background()
	im, lib, fd := setup(t)
	startWorker(t, im)
	src := t.TempDir()
	tmp := t.TempDir()
	taggedMP3(t, filepath.Join(tmp, "1.mp3"), map[string]string{"TIT2": "One", "TPE1": "A", "TALB": "Zipped", "TRCK": "1"})
	taggedMP3(t, filepath.Join(tmp, "2.mp3"), map[string]string{"TIT2": "Two", "TPE1": "A", "TALB": "Zipped", "TRCK": "2"})
	one, _ := os.ReadFile(filepath.Join(tmp, "1.mp3"))
	two, _ := os.ReadFile(filepath.Join(tmp, "2.mp3"))
	archive := filepath.Join(src, "album.zip")
	writeZip(t, archive, []zipEntry{
		{name: "a/01 one.mp3", data: one, utf8: true, method: zip.Store},
		{name: "a/02 two.mp3", data: two, utf8: true, method: zip.Store},
	})
	fd.failNext = errors.New("upload failed")
	batch, _, _ := im.CreateBatch(ctx, "download", "Zip", src, false)
	waitState(t, im, batch, BatchDone)
	before := itemsOf(t, im, batch)
	lost := before["album/a/01 one.mp3"]
	if lost.state != StateFailed || before["album/a/02 two.mp3"].state != StatePublished {
		t.Fatalf("first import %+v", before)
	}
	os.Remove(lost.path)
	if res, err := im.Retry(ctx, batch); err != nil || res.Requeued != 1 {
		t.Fatalf("retry %+v %v", res, err)
	}
	waitState(t, im, batch, BatchDone)
	after := itemsOf(t, im, batch)
	if len(after) != len(before) || after["album/a/01 one.mp3"].state != StatePublished || after["album.zip"].state != StateExpanded {
		t.Fatalf("after unpacking again %+v", after)
	}
	if tracks, _ := lib.Tracks(ctx, 10, 0, ""); len(tracks) != 2 {
		t.Fatalf("tracks %d", len(tracks))
	}

	fd.failNext = errors.New("upload failed")
	other := t.TempDir()
	archive2 := filepath.Join(other, "again.zip")
	taggedMP3(t, filepath.Join(tmp, "3.mp3"), map[string]string{"TIT2": "Three", "TPE1": "A", "TALB": "Zipped", "TRCK": "3"})
	three, _ := os.ReadFile(filepath.Join(tmp, "3.mp3"))
	writeZip(t, archive2, []zipEntry{{name: "a/03 three.mp3", data: three, utf8: true, method: zip.Store}})
	b2, _, _ := im.CreateBatch(ctx, "download", "Zip2", other, false)
	waitState(t, im, b2, BatchDone)
	gone := itemsOf(t, im, b2)["again/a/03 three.mp3"]
	os.Remove(gone.path)
	os.Remove(archive2)
	var asked []string
	im.Refetch = func(_ context.Context, b int64, paths []string) error {
		asked = paths
		return nil
	}
	if res, err := im.Retry(ctx, b2); err != nil || res != (RetryResult{Fetching: 1}) || !slices.Equal(asked, []string{archive2}) {
		t.Fatalf("retry without the archive %+v %v %v", res, err, asked)
	}
}

// "Saved" means the library has the file now, not that an import once put it there: after the
// song and its asset are deleted, a lost file of a later import of the same path is fetched again,
// not marked a duplicate (review #67).
func TestDeletedSongIsNotSavedElsewhere(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	startWorker(t, im)
	src := t.TempDir()
	p := filepath.Join(src, "A/01.flac")
	copyFixture(t, "tone.flac", p)
	first, _, _ := im.CreateBatch(ctx, "download", "A", src, false)
	waitState(t, im, first, BatchDone)
	tracks, _ := lib.Tracks(ctx, 10, 0, "")
	if len(tracks) != 1 {
		t.Fatalf("tracks %d", len(tracks))
	}
	again := func() int64 { // a later import of the same path, whose file was gone
		t.Helper()
		r, _ := im.db.Exec(`INSERT INTO import_batches (kind, source, state, created_at) VALUES ('download', 'A', 'done', 0)`)
		b, _ := r.LastInsertId()
		im.db.Exec(`INSERT INTO import_items (batch_id, local_path, rel_path, state, role, error, updated_at)
			VALUES (?, ?, 'A/01.flac', 'failed', 'audio', 'open: no such file or directory', 0)`, b, p)
		return b
	}
	fetched := 0
	im.Refetch = func(context.Context, int64, []string) error { fetched++; return nil }
	os.Rename(p, p+".away")
	// Still in the library: a duplicate.
	if res, err := im.Retry(ctx, again()); err != nil || res != (RetryResult{Saved: 1}) || fetched != 0 {
		t.Fatalf("in the library: %+v %v (fetched %d)", res, err, fetched)
	}
	if _, err := lib.DeleteTrack(ctx, tracks[0].ID); err != nil {
		t.Fatal(err)
	}
	if res, err := im.Retry(ctx, again()); err != nil || res != (RetryResult{Fetching: 1}) || fetched != 1 {
		t.Fatalf("deleted from the library: %+v %v (fetched %d)", res, err, fetched)
	}
	// A copy whose Drive file went missing is no copy either.
	os.Rename(p+".away", p)
	second, _, _ := im.CreateBatch(ctx, "download", "A", src, false)
	waitState(t, im, second, BatchDone)
	os.Rename(p, p+".away")
	var drive string
	im.db.QueryRow(`SELECT a.drive_file_id FROM assets a JOIN import_items i ON i.asset_id = a.id WHERE i.batch_id = ?`, second).Scan(&drive)
	lib.ObserveDriveFile(ctx, library.DriveObservation{ID: drive})
	if res, err := im.Retry(ctx, again()); err != nil || res != (RetryResult{Fetching: 1}) {
		t.Fatalf("missing in Drive: %+v %v", res, err)
	}
}
