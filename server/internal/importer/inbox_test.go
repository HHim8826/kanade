package importer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/library"
)

// inboxDrive is a fake Drive with folders: files have parents and can be moved and read by range.
type inboxDrive struct {
	fakeDrive
	mu2    sync.Mutex
	meta   map[string]gdrive.File // ID -> file or folder
	ranges int                    // range requests served
	moves  int
	nextID int
	// failRange makes the first range request for these files fail.
	failRange map[string]bool
}

func newInboxDrive() *inboxDrive {
	return &inboxDrive{fakeDrive: fakeDrive{files: map[string][]byte{}}, meta: map[string]gdrive.File{}}
}

func (d *inboxDrive) add(parent, name string, data []byte) string {
	d.mu2.Lock()
	defer d.mu2.Unlock()
	d.nextID++
	id := fmt.Sprintf("in%d", d.nextID)
	f := gdrive.File{ID: id, Name: name, Parents: []string{parent}, Size: fmt.Sprint(len(data))}
	if data == nil {
		f.MimeType = gdrive.FolderMime
	} else {
		sum := sha256.Sum256(data)
		f.SHA256Checksum = hex.EncodeToString(sum[:])
		d.files[id] = data
	}
	d.meta[id] = f
	return id
}

func (d *inboxDrive) Children(_ context.Context, folder string) ([]gdrive.File, error) {
	d.mu2.Lock()
	defer d.mu2.Unlock()
	var out []gdrive.File
	for _, f := range d.meta {
		if len(f.Parents) > 0 && f.Parents[0] == folder {
			out = append(out, f)
		}
	}
	return out, nil
}

func (d *inboxDrive) Move(_ context.Context, id, to string, from []string) error {
	d.mu2.Lock()
	defer d.mu2.Unlock()
	f, ok := d.meta[id]
	if !ok || (len(from) > 0 && f.Parents[0] != from[0]) {
		return fmt.Errorf("move %s: not in %v", id, from)
	}
	f.Parents = []string{to}
	d.meta[id] = f
	d.moves++
	return nil
}

func (d *inboxDrive) OpenRange(_ context.Context, id string, start, end int64) (*http.Response, error) {
	d.mu2.Lock()
	defer d.mu2.Unlock()
	if d.failRange[id] {
		delete(d.failRange, id)
		return nil, fmt.Errorf("drive is unavailable")
	}
	data := d.files[id]
	if end < 0 || end >= int64(len(data)) {
		end = int64(len(data)) - 1
	}
	d.ranges++
	return &http.Response{StatusCode: http.StatusPartialContent, Body: io.NopCloser(bytes.NewReader(data[start : end+1]))}, nil
}

func (d *inboxDrive) GetFile(_ context.Context, id, _ string) (gdrive.File, error) {
	d.mu2.Lock()
	defer d.mu2.Unlock()
	return d.meta[id], nil
}

func (d *inboxDrive) parentOf(id string) string {
	d.mu2.Lock()
	defer d.mu2.Unlock()
	return d.meta[id].Parents[0]
}

func TestInboxImportsInPlace(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	dd := newInboxDrive()
	im.drive = dd
	startWorker(t, im)
	inbox, _ := im.drive.Folder(ctx, InboxFolder) // "folder:inbox"

	tmp := t.TempDir()
	taggedMP3(t, filepath.Join(tmp, "1.mp3"), map[string]string{"TIT2": "Dropped", "TPE1": "A", "TALB": "Inbox Album", "TRCK": "1"})
	taggedMP3(t, filepath.Join(tmp, "2.mp3"), map[string]string{"TIT2": "Second", "TPE1": "A", "TALB": "Inbox Album", "TRCK": "2"})
	one, _ := os.ReadFile(filepath.Join(tmp, "1.mp3"))
	two, _ := os.ReadFile(filepath.Join(tmp, "2.mp3"))
	cover, _ := os.ReadFile("../media/testdata/cover.png")
	album := dd.add(inbox, "A - Inbox Album", nil)
	f1 := dd.add(album, "01.mp3", one)
	f2 := dd.add(album, "02.mp3", two)
	dd.add(album, "02.lrc", []byte("[00:01.00]words"))
	dd.add(album, "cover.png", cover)
	dd.add(album, "notes.txt", []byte("ignored"))

	n, err := im.ScanInbox(ctx)
	if err != nil || n != 2 {
		t.Fatalf("scan: %d %v", n, err)
	}
	if n, _ := im.ScanInbox(ctx); n != 0 {
		t.Fatal("the same files were queued twice")
	}
	var batch int64
	im.db.QueryRow(`SELECT max(id) FROM import_batches WHERE kind = 'inbox'`).Scan(&batch)
	waitState(t, im, batch, BatchDone)
	b, _ := im.Batch(ctx, batch)
	if b.Counts[StatePublished] != 2 {
		t.Fatalf("batch %+v", b.Items)
	}
	if dd.uploads != 1 { // only the cover was uploaded; the songs were moved
		t.Fatalf("uploads = %d", dd.uploads)
	}
	if p := dd.parentOf(f1); p != "folder:library/A/Inbox Album" {
		t.Fatalf("moved to %q", p)
	}
	albums, _ := lib.Albums(ctx, 10, 0, false)
	if len(albums) != 1 || albums[0].CoverID == 0 || albums[0].Tracks != 2 {
		t.Fatalf("albums %+v", albums)
	}
	d, _ := lib.Album(ctx, albums[0].ID)
	if l, _ := lib.Lyrics(ctx, d.Entries[1].TrackID); l == nil || !l.Synced {
		t.Fatalf("lyrics %+v", l)
	}
	if a, _ := lib.AssetByHash(ctx, sha(one), int64(len(one))); a == nil || a.DriveFileID != f1 || a.State != library.AssetVerified {
		t.Fatalf("asset %+v", a)
	}
	_ = f2
	// What the batch left (lyrics, cover, notes) went to inbox/已處理 with its folder.
	if p := movedTo(dd, album, "folder:inbox/"+inboxProcessed); p != "folder:inbox/"+inboxProcessed {
		t.Fatalf("finished folder is in %q", p)
	}

	// A folder still receiving files waits for the next scan.
	late := dd.add(inbox, "Still Copying", nil)
	fresh := dd.add(late, "01.mp3", two)
	dd.mu2.Lock()
	m := dd.meta[fresh]
	m.CreatedTime = time.Now().UTC().Format(time.RFC3339)
	dd.meta[fresh] = m
	dd.mu2.Unlock()
	if n, _ := im.ScanInbox(ctx); n != 0 || im.InboxWaiting() != 1 {
		t.Fatalf("queued %d, waiting %d", n, im.InboxWaiting())
	}
	dd.mu2.Lock()
	m.CreatedTime = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	dd.meta[fresh] = m
	dd.mu2.Unlock()
	if n, _ := im.ScanInbox(ctx); n != 1 || im.InboxWaiting() != 0 {
		t.Fatalf("settled folder: queued %d", n)
	}
	im.db.QueryRow(`SELECT max(id) FROM import_batches WHERE kind = 'inbox'`).Scan(&batch)
	waitState(t, im, batch, BatchDone)
	// Its file was a duplicate (set aside in inbox/重複), and the empty folder is tidied away.
	if p := movedTo(dd, late, "folder:inbox/"+inboxProcessed); p != "folder:inbox/"+inboxProcessed {
		t.Fatalf("settled folder is in %q", p)
	}

	// The same bytes dropped in again are set aside, not deleted and not imported twice.
	again := dd.add(inbox, "copy.mp3", one)
	im.ScanInbox(ctx)
	im.db.QueryRow(`SELECT max(id) FROM import_batches WHERE kind = 'inbox'`).Scan(&batch)
	waitState(t, im, batch, BatchDone)
	if p := dd.parentOf(again); !strings.HasSuffix(p, "inbox/"+inboxDuplicates) {
		t.Fatalf("duplicate went to %q", p)
	}
	if tracks, _ := lib.Tracks(ctx, 10, 0, ""); len(tracks) != 2 {
		t.Fatalf("tracks %+v", tracks)
	}
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Covers, scans and files already handled do not use up the scan's limits.
func TestInboxLimitCountsOnlyNewImportableFiles(t *testing.T) {
	ctx := context.Background()
	im, _, _ := setup(t)
	dd := newInboxDrive()
	im.drive = dd
	inbox, _ := im.drive.Folder(ctx, InboxFolder)
	scans := dd.add(inbox, "Scans", nil)
	for i := 0; i < inboxBatchMax+100; i++ {
		dd.add(scans, fmt.Sprintf("%04d.jpg", i), []byte{byte(i)})
	}
	tmp := t.TempDir()
	taggedMP3(t, filepath.Join(tmp, "1.mp3"), map[string]string{"TIT2": "Late", "TPE1": "A", "TALB": "B"})
	one, _ := os.ReadFile(filepath.Join(tmp, "1.mp3"))
	dd.add(inbox, "late.mp3", one)
	if n, err := im.ScanInbox(ctx); err != nil || n != 1 {
		t.Fatalf("queued %d, %v", n, err)
	}
}

// A file replaced after the scan is imported as what it is now, never under the scanned checksum.
func TestInboxUsesCurrentContent(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	dd := newInboxDrive()
	im.drive = dd
	inbox, _ := im.drive.Folder(ctx, InboxFolder)
	tmp := t.TempDir()
	taggedMP3(t, filepath.Join(tmp, "old.mp3"), map[string]string{"TIT2": "Old", "TPE1": "A", "TALB": "B"})
	taggedMP3(t, filepath.Join(tmp, "new.mp3"), map[string]string{"TIT2": "New", "TPE1": "A", "TALB": "B"})
	old, _ := os.ReadFile(filepath.Join(tmp, "old.mp3"))
	cur, _ := os.ReadFile(filepath.Join(tmp, "new.mp3"))
	id := dd.add(inbox, "song.mp3", old)
	if n, _ := im.ScanInbox(ctx); n != 1 {
		t.Fatal("not queued")
	}
	dd.mu2.Lock() // replaced in Drive before the import gets to it
	dd.files[id] = cur
	m := dd.meta[id]
	m.SHA256Checksum, m.Size = sha(cur), fmt.Sprint(len(cur))
	dd.meta[id] = m
	dd.mu2.Unlock()
	startWorker(t, im)
	var batch int64
	im.db.QueryRow(`SELECT max(id) FROM import_batches`).Scan(&batch)
	waitState(t, im, batch, BatchDone)
	if a, _ := lib.AssetByHash(ctx, sha(old), int64(len(old))); a != nil {
		t.Fatalf("published under the old checksum: %+v", a)
	}
	a, _ := lib.AssetByHash(ctx, sha(cur), int64(len(cur)))
	if a == nil || a.State != library.AssetVerified || a.DriveFileID != id {
		t.Fatalf("asset %+v", a)
	}
	if tracks, _ := lib.Tracks(ctx, 10, 0, ""); len(tracks) != 1 || tracks[0].Title != "New" {
		t.Fatalf("tracks %+v", tracks)
	}
}

// movedTo waits a little for a file to be moved to want: the batch is done just before its inbox
// folder is tidied. It returns where the file is.
func movedTo(dd *inboxDrive, id, want string) string {
	deadline := time.Now().Add(5 * time.Second)
	for dd.parentOf(id) != want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return dd.parentOf(id)
}

// The inbox wait is a setting; no wait at all is one too (review #77).
func TestInboxSettleSetting(t *testing.T) {
	im, _, _ := setup(t)
	for _, c := range []struct{ set, want time.Duration }{{-2, 0}, {0, 0}, {7 * time.Minute, 7 * time.Minute}} {
		im.SetInboxSettle(c.set)
		if got := im.inboxSettle(); got != c.want {
			t.Fatalf("set %v: %v", c.set, got)
		}
	}
	fresh, _, _ := setup(t)
	if fresh.inboxSettle() != defaultInboxSettle {
		t.Fatal("default")
	}
}
