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

	// The same bytes dropped in again are set aside, not deleted and not imported twice.
	again := dd.add(inbox, "copy.mp3", one)
	im.ScanInbox(ctx)
	im.db.QueryRow(`SELECT max(id) FROM import_batches WHERE kind = 'inbox'`).Scan(&batch)
	waitState(t, im, batch, BatchDone)
	if p := dd.parentOf(again); !strings.HasSuffix(p, "inbox/"+inboxDuplicates) {
		t.Fatalf("duplicate went to %q", p)
	}
	if tracks, _ := lib.Tracks(ctx, 10, 0); len(tracks) != 2 {
		t.Fatalf("tracks %+v", tracks)
	}
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
