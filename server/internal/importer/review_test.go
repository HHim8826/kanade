package importer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/ffmpeg"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/staging"
)

// Audio that did not make it into the library keeps its source; the rest of an upload is cleaned
// up, and the group goes once the user discards what is left (review #1).
func TestUnsavedAudioKeepsItsSource(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	var mu sync.Mutex
	var calls []int
	im.OnBatchDone = func(_ context.Context, kind, _ string, unsaved int) {
		mu.Lock()
		calls = append(calls, unsaved)
		mu.Unlock()
	}
	startWorker(t, im)
	src := filepath.Join(im.staging, "uploads", "g1")
	copyFixture(t, "tone.ogg", filepath.Join(src, "A/good.ogg"))
	os.MkdirAll(filepath.Join(src, "A"), 0o755)
	os.WriteFile(filepath.Join(src, "A/odd.mp3"), []byte("not really an mp3 at all, just some bytes"), 0o644)
	batch, _, err := im.CreateBatch(ctx, "upload", "g1", src, false)
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, im, batch, BatchDone)
	b, _ := im.Batch(ctx, batch)
	if st := states(b); st["A/good.ogg"] != StatePublished || st["A/odd.mp3"] != StateSkipped {
		t.Fatalf("states %v", st)
	}
	if _, err := os.Stat(filepath.Join(src, "A/good.ogg")); !os.IsNotExist(err) {
		t.Fatal("the imported file was kept")
	}
	if _, err := os.Stat(filepath.Join(src, "A/odd.mp3")); err != nil {
		t.Fatalf("the skipped file was deleted: %v", err)
	}
	if n, _ := im.Unsaved(ctx, batch); n != 1 {
		t.Fatalf("unsaved = %d", n)
	}
	mu.Lock()
	if len(calls) != 1 || calls[0] != 1 {
		t.Fatalf("batch done calls %v", calls)
	}
	mu.Unlock()
	if n, err := im.Discard(ctx, batch); err != nil || n != 1 {
		t.Fatalf("discard %d %v", n, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || calls[1] != 0 {
		t.Fatalf("after discard %v", calls)
	}
	if tracks, _ := lib.Tracks(ctx, 10, 0); len(tracks) != 1 {
		t.Fatalf("tracks %+v", tracks)
	}
}

// An archive's skipped file is its only copy once an uploaded archive is unpacked: the work folder
// stays.
func TestUnsavedFileFromArchiveKeepsWorkFolder(t *testing.T) {
	ctx := context.Background()
	im, _, _ := setup(t)
	startWorker(t, im)
	src := filepath.Join(im.staging, "uploads", "g2")
	os.MkdirAll(src, 0o755)
	tone, _ := os.ReadFile("../media/testdata/tone.ogg")
	writeZip(t, filepath.Join(src, "pack.zip"), []zipEntry{{name: "P/good.ogg", data: tone, utf8: true}, {name: "P/odd.mp3", data: []byte("not audio"), utf8: true}})
	batch, _, _ := im.CreateBatch(ctx, "upload", "g2", src, false)
	waitState(t, im, batch, BatchDone)
	b, _ := im.Batch(ctx, batch)
	if st := states(b); st["pack/P/odd.mp3"] != StateSkipped {
		t.Fatalf("states %v", st)
	}
	if _, err := os.Stat(filepath.Join(im.workDir(batch))); err != nil {
		t.Fatalf("work folder removed: %v", err)
	}
	im.Discard(ctx, batch)
	if _, err := os.Stat(filepath.Join(im.workDir(batch))); !os.IsNotExist(err) {
		t.Fatal("work folder kept after discarding")
	}
}

// A result that cannot be recorded keeps the source, and the item is done again after a restart
// without a second library entry (review #2).
func TestResultWriteFailureKeepsSource(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	src := filepath.Join(im.staging, "uploads", "g3")
	copyFixture(t, "tone.ogg", filepath.Join(src, "a.ogg"))
	batch, _, _ := im.CreateBatch(ctx, "upload", "g3", src, false)
	im.db.Exec(`CREATE TRIGGER fail_result BEFORE UPDATE OF sha256 ON import_items
		BEGIN SELECT RAISE(FAIL, 'simulated persistence failure'); END`)
	wctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { im.Run(wctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if tracks, _ := lib.Tracks(ctx, 10, 0); len(tracks) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never published")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(4 * time.Second) // the result write is tried three times
	if _, err := os.Stat(filepath.Join(src, "a.ogg")); err != nil {
		t.Fatalf("source deleted although the result was not recorded: %v", err)
	}
	stop()
	<-done
	im.db.Exec(`DROP TRIGGER fail_result`)
	startWorker(t, im) // a restart
	waitState(t, im, batch, BatchDone)
	if _, err := os.Stat(filepath.Join(src, "a.ogg")); !os.IsNotExist(err) {
		t.Fatal("source kept after the result was recorded")
	}
	if tracks, _ := lib.Tracks(ctx, 10, 0); len(tracks) != 1 {
		t.Fatalf("tracks %+v", tracks)
	}
}

// A conversion that failed for want of space converts on retry, instead of being skipped, and the
// upload is cleaned up only then (review #18, #1).
func TestRetryConvertsAfterSpaceFailure(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	withFFmpeg(t, im)
	var full sync.Mutex
	noRoom := true
	im.Budget = &staging.Budget{Limit: 1 << 30}
	im.Budget.Use(staging.OnDisk(func(context.Context) int64 { // something else holds the staging space at first
		full.Lock()
		defer full.Unlock()
		if noRoom {
			return 1 << 30
		}
		return 0
	}))
	var unsaved []int
	im.OnBatchDone = func(_ context.Context, _, _ string, n int) { unsaved = append(unsaved, n) }
	startWorker(t, im)
	src := filepath.Join(im.staging, "uploads", "g4")
	makeAudio(t, im, filepath.Join(src, "W/01.wav"), "-c:a", "pcm_s16le", "-metadata", "title=Wave", "-metadata", "album=W")
	batch, _, _ := im.CreateBatch(ctx, "upload", "g4", src, false)
	waitState(t, im, batch, BatchDone)
	if b, _ := im.Batch(ctx, batch); states(b)["W/01.wav"] != StateFailed {
		t.Fatalf("states %v", states(b))
	}
	if _, err := os.Stat(filepath.Join(src, "W/01.wav")); err != nil {
		t.Fatal("source deleted after a failure")
	}
	full.Lock()
	noRoom = false
	full.Unlock()
	if n, err := im.Retry(ctx, batch); err != nil || n != 1 {
		t.Fatalf("retry %d %v", n, err)
	}
	waitState(t, im, batch, BatchDone)
	b, _ := im.Batch(ctx, batch)
	if states(b)["W/01.wav"] != StatePublished {
		t.Fatalf("after retry %v", states(b))
	}
	if tracks, _ := lib.Tracks(ctx, 10, 0); len(tracks) != 1 || tracks[0].Asset.Format != "flac" {
		t.Fatalf("tracks %+v", tracks)
	}
	if len(unsaved) != 2 || unsaved[0] != 1 || unsaved[1] != 0 { // the upload group is cleared only after the retry
		t.Fatalf("batch done calls %v", unsaved)
	}
}

// A disc image whose CUE sheet was wrong is cut by the corrected sheet on retry, not imported
// whole; groups made before keep their albums (review #18).
func TestRetryReadsCorrectedCue(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	withFFmpeg(t, im)
	startWorker(t, im)
	src := t.TempDir()
	copyFixture(t, "tone.ogg", filepath.Join(src, "Other/x.ogg")) // a group that succeeds first
	makeAudio(t, im, filepath.Join(src, "Disc/image.flac"), "-c:a", "flac")
	bad := strings.Replace(splitCue, "INDEX 01 00:01:30", "INDEX 01 09:00:00", 1) // track 3 after the end
	os.WriteFile(filepath.Join(src, "Disc/image.cue"), []byte(bad), 0o644)
	batch, _, _ := im.CreateBatch(ctx, "local", "", src, false)
	waitState(t, im, batch, BatchDone)
	if b, _ := im.Batch(ctx, batch); states(b)["Disc/image.flac"] != StateFailed {
		t.Fatalf("states %v", states(b))
	}
	before, _ := lib.Albums(ctx, 10, 0, false)
	os.WriteFile(filepath.Join(src, "Disc/image.cue"), []byte(splitCue), 0o644)
	if _, err := im.Retry(ctx, batch); err != nil {
		t.Fatal(err)
	}
	waitState(t, im, batch, BatchDone)
	albums, _ := lib.Albums(ctx, 10, 0, false)
	if len(albums) != len(before)+1 {
		t.Fatalf("albums %+v (before %+v)", albums, before)
	}
	for _, a := range albums {
		d, _ := lib.Album(ctx, a.ID)
		if a.Title == "テスト盤" && (len(d.Entries) != 3 || len(d.Sidecars) != 1) {
			t.Fatalf("disc %+v", d)
		}
		if a.Title != "テスト盤" && len(d.Entries) != 1 {
			t.Fatalf("other album changed: %+v", d)
		}
	}
}

// Songs left out of a disc image are added by a later import of the same image; a converted file
// whose library copy went missing from Drive is imported again (review #19).
func TestPartialSourceCanBeCompleted(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	withFFmpeg(t, im)
	startWorker(t, im)
	src := t.TempDir()
	makeAudio(t, im, filepath.Join(src, "Disc/image.flac"), "-c:a", "flac")
	os.WriteFile(filepath.Join(src, "Disc/image.cue"), []byte(splitCue), 0o644)
	batch, _, _ := im.CreateBatch(ctx, "local", "", src, true)
	waitState(t, im, batch, BatchReview)
	p, _ := im.Preview(ctx, batch)
	g := group(t, p, "テスト盤")
	if err := im.ApplyOp(ctx, batch, PlanOp{Op: "exclude", Items: []int64{g.Items[1].ID, g.Items[2].ID}}); err != nil {
		t.Fatal(err)
	}
	im.Start(ctx, batch)
	waitState(t, im, batch, BatchDone)
	if tracks, _ := lib.Tracks(ctx, 10, 0); len(tracks) != 1 {
		t.Fatalf("first import: %+v", tracks)
	}
	again, _, _ := im.CreateBatch(ctx, "local", "", src, false)
	waitState(t, im, again, BatchDone)
	if tracks, _ := lib.Tracks(ctx, 10, 0); len(tracks) != 3 {
		t.Fatalf("after completing: %+v", tracks)
	}
	b, _ := im.Batch(ctx, again)
	if st := states(b); st["Disc/01 一曲目.flac"] != StateDuplicate || st["Disc/02 二曲目.flac"] != StatePublished ||
		st["Disc/03 三曲目.flac"] != StatePublished {
		t.Fatalf("second import %v", st)
	}
	// Complete now: a third import skips the image without cutting it.
	third, _, _ := im.CreateBatch(ctx, "local", "", src, false)
	waitState(t, im, third, BatchDone)
	if b, _ := im.Batch(ctx, third); states(b)["Disc/image.flac"] != StateDuplicate {
		t.Fatalf("third %v", states(b))
	}

	// A converted file whose FLAC is missing from Drive.
	wsrc := t.TempDir()
	makeAudio(t, im, filepath.Join(wsrc, "w.wav"), "-c:a", "pcm_s16le", "-metadata", "title=Wave")
	first, _, _ := im.CreateBatch(ctx, "local", "", wsrc, false)
	waitState(t, im, first, BatchDone)
	var asset int64
	var drive string
	im.db.QueryRow(`SELECT a.id, a.drive_file_id FROM assets a JOIN import_items i ON i.asset_id = a.id WHERE i.batch_id = ?`, first).Scan(&asset, &drive)
	lib.ObserveDriveFile(ctx, library.DriveObservation{ID: drive}) // gone from Drive
	again2, _, _ := im.CreateBatch(ctx, "local", "", wsrc, false)
	waitState(t, im, again2, BatchDone)
	var state string
	im.db.QueryRow(`SELECT state FROM assets WHERE id = ?`, asset).Scan(&state)
	if state != library.AssetVerified {
		b, _ := im.Batch(ctx, again2)
		t.Fatalf("missing converted file not restored: %s %v", state, states(b))
	}
}

// An inbox log that could not be fetched is fetched again on retry (review #18).
func TestRetryFetchesInboxSidecarAgain(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	dd := newInboxDrive()
	im.drive = dd
	inbox, _ := im.drive.Folder(ctx, InboxFolder)
	tmp := t.TempDir()
	taggedMP3(t, filepath.Join(tmp, "1.mp3"), map[string]string{"TIT2": "One", "TPE1": "A", "TALB": "Logged", "TRCK": "1"})
	one, _ := os.ReadFile(filepath.Join(tmp, "1.mp3"))
	album := dd.add(inbox, "A - Logged", nil)
	dd.add(album, "01.mp3", one)
	log := dd.add(album, "rip.log", []byte("Exact Audio Copy V1.0"))
	dd.failRange = map[string]bool{log: true}
	startWorker(t, im)
	im.ScanInbox(ctx)
	var batch int64
	im.db.QueryRow(`SELECT max(id) FROM import_batches`).Scan(&batch)
	waitState(t, im, batch, BatchDone)
	if st := states(mustBatch(t, im, batch)); st["A - Logged/rip.log"] != StateFailed || st["A - Logged/01.mp3"] != StatePublished {
		t.Fatalf("first %v", st)
	}
	if _, err := im.Retry(ctx, batch); err != nil {
		t.Fatal(err)
	}
	waitState(t, im, batch, BatchDone)
	if st := states(mustBatch(t, im, batch)); st["A - Logged/rip.log"] != StatePublished {
		t.Fatalf("after retry %v", st)
	}
	albums, _ := lib.Albums(ctx, 10, 0, false)
	if d, _ := lib.Album(ctx, albums[0].ID); len(d.Sidecars) != 1 {
		t.Fatalf("sidecars %+v", d.Sidecars)
	}
}

func mustBatch(t *testing.T, im *Importer, id int64) *BatchView {
	t.Helper()
	b, err := im.Batch(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// What a split may write is held as the songs' upper bound, not the compressed image's size, and
// the songs written stay within it; a budget below the bound fails the image instead of overrunning
// (review #4).
func TestSplitHoldsItsOutputBound(t *testing.T) {
	ctx := context.Background()
	im, _, _ := setup(t)
	withFFmpeg(t, im)
	src := t.TempDir()
	makeAudio(t, im, filepath.Join(src, "Disc/image.flac"), "-c:a", "flac")
	os.WriteFile(filepath.Join(src, "Disc/image.cue"), []byte(splitCue), 0o644)
	image, _ := os.Stat(filepath.Join(src, "Disc/image.flac"))
	s, _ := im.FFmpeg.Probe(ctx, filepath.Join(src, "Disc/image.flac"))
	bound := 3*(256<<10) + ffmpeg.MaxFLAC(s, 0, 0) - 256<<10 // three songs' share of the PCM, each with its overhead

	// Room for the image's size, not the bound, with something else in staging: it fails.
	limit := image.Size() * 2
	im.Budget = &staging.Budget{Limit: limit}
	im.Budget.Use(staging.OnDisk(im.WorkCommitted))
	im.Budget.Use(staging.OnDisk(func(context.Context) int64 { return limit / 2 }))
	startWorker(t, im)
	batch, _, _ := im.CreateBatch(ctx, "local", "", src, true)
	waitState(t, im, batch, BatchReview)
	if st := states(mustBatch(t, im, batch)); st["Disc/image.flac"] != StateFailed {
		t.Fatalf("split past the budget: %v", st)
	}
	// With nothing else in staging, output larger than the whole budget is made alone (review #46);
	// the image is in staging, but it is the split's own.
	im.Budget = &staging.Budget{Limit: limit, Dir: src}
	im.Budget.Use(staging.OnDisk(func(context.Context) int64 { return image.Size() }))
	im.Budget.Use(staging.OnDisk(im.WorkCommitted))
	alone, _, _ := im.CreateBatch(ctx, "local", "", src, true)
	waitState(t, im, alone, BatchReview)
	if st := states(mustBatch(t, im, alone)); st["Disc/image.flac"] != StateSplit {
		t.Fatalf("not split alone: %v", st)
	}
	im.Cancel(ctx, alone)

	im.Budget = &staging.Budget{Limit: bound + 1<<20}
	im.Budget.Use(staging.OnDisk(im.WorkCommitted))
	again, _, _ := im.CreateBatch(ctx, "local", "", src, true)
	waitState(t, im, again, BatchReview)
	if st := states(mustBatch(t, im, again)); st["Disc/image.flac"] != StateSplit {
		t.Fatalf("not split: %v", st)
	}
	if used := dirSize(im.workDir(again)); used > bound || im.Budget.Used(ctx) != used {
		t.Fatalf("songs take %d, bound %d, budget sees %d", used, bound, im.Budget.Used(ctx))
	}
}

// "New album" chosen in the preview for files imported before makes that album, even though the
// files are the same (review #21).
func TestPreviewNewAlbumOverridesImportIdentity(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	startWorker(t, im)
	src := t.TempDir()
	taggedMP3(t, filepath.Join(src, "A/1.mp3"), map[string]string{"TIT2": "One", "TPE1": "X", "TALB": "Original", "TRCK": "1"})
	taggedMP3(t, filepath.Join(src, "A/2.mp3"), map[string]string{"TIT2": "Two", "TPE1": "X", "TALB": "Original", "TRCK": "2"})
	first, _, _ := im.CreateBatch(ctx, "local", "", src, false)
	waitState(t, im, first, BatchDone)
	second, _, _ := im.CreateBatch(ctx, "local", "", src, true)
	waitState(t, im, second, BatchReview)
	p, _ := im.Preview(ctx, second)
	g := group(t, p, "Original")
	name, yes := "Another Edition", true
	if err := im.ApplyOp(ctx, second, PlanOp{Op: "group", Group: g.Key, Album: &name, NewAlbum: &yes}); err != nil {
		t.Fatal(err)
	}
	im.Start(ctx, second)
	waitState(t, im, second, BatchDone)
	albums, _ := lib.Albums(ctx, 10, 0, false)
	if len(albums) != 2 {
		t.Fatalf("albums %+v", albums)
	}
	for _, a := range albums {
		if d, _ := lib.Album(ctx, a.ID); len(d.Entries) != 2 {
			t.Fatalf("%s has %d entries", a.Title, len(d.Entries))
		}
	}
	if tracks, _ := lib.Tracks(ctx, 10, 0); len(tracks) != 2 { // the files are shared
		t.Fatalf("tracks %+v", tracks)
	}
}

// FFmpeg output short of space that others hold waits for it, saying so on the file, and goes on
// once it is free, instead of failing (review #46).
func TestConversionWaitsForSpace(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	withFFmpeg(t, im)
	var held atomic.Int64
	held.Store(1 << 30)
	im.Budget = &staging.Budget{Limit: 1 << 30}
	im.Budget.Use(staging.OnDisk(func(context.Context) int64 { return held.Load() })) // an upload, say
	im.SpaceWait = time.Minute
	startWorker(t, im)
	src := t.TempDir()
	makeAudio(t, im, filepath.Join(src, "W/01.wav"), "-c:a", "pcm_s16le", "-metadata", "title=Wave", "-metadata", "album=W")
	batch, _, _ := im.CreateBatch(ctx, "local", "", src, false)
	deadline := time.Now().Add(20 * time.Second)
	for {
		b, _ := im.Batch(ctx, batch)
		if len(b.Items) > 0 && strings.HasPrefix(b.Items[0].Error, "waiting for staging space") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not waiting: %+v", b.Items)
		}
		time.Sleep(100 * time.Millisecond)
	}
	held.Store(0)
	waitState(t, im, batch, BatchDone)
	b, _ := im.Batch(ctx, batch)
	if states(b)["W/01.wav"] != StatePublished || b.Items[0].Error != "" {
		t.Fatalf("after the wait %v %q", states(b), b.Items[0].Error)
	}
	if tracks, _ := lib.Tracks(ctx, 10, 0); len(tracks) != 1 {
		t.Fatalf("tracks %d", len(tracks))
	}
}
