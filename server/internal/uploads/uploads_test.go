package uploads

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/staging"
)

func newStore(t *testing.T, budget int64) *Store {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(context.Background(), filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return New(d, filepath.Join(dir, "uploads"), budget)
}

func TestCleanPathRejectsEscapes(t *testing.T) {
	for _, bad := range []string{"", "/etc/passwd", "../x", "a/../../x", "a//b", "a/./b", `..\x`, "a/\x00"} {
		if _, err := CleanPath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if p, err := CleanPath(`Album\Disc 1\01 曲.flac`); err != nil || p != "Album/Disc 1/01 曲.flac" {
		t.Fatalf("got %q %v", p, err)
	}
}

// shortReader fails partway, like a dropped connection.
type shortReader struct {
	r io.Reader
	n int
}

func (s *shortReader) Read(p []byte) (int, error) {
	if s.n <= 0 {
		return 0, errors.New("connection reset")
	}
	if len(p) > s.n {
		p = p[:s.n]
	}
	n, err := s.r.Read(p)
	s.n -= n
	return n, err
}

func TestChunkedUploadWithResume(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1<<30)
	data := bytes.Repeat([]byte("0123456789"), 1000) // 10 000 bytes
	sum := sha256.Sum256(data)
	u, err := s.Create(ctx, "group-0001", "Album/Disc 1/01.flac", int64(len(data)), hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, u.ID, 0, bytes.NewReader(data[:4000]), 4000); err != nil {
		t.Fatal(err)
	}
	// The next chunk drops after 1000 bytes: nothing of it may count.
	if _, err := s.Append(ctx, u.ID, 4000, &shortReader{r: bytes.NewReader(data[4000:8000]), n: 1000}, 4000); err == nil {
		t.Fatal("interrupted chunk accepted")
	}
	// A client that lost its place gets the server's count back.
	if cur, err := s.Append(ctx, u.ID, 8000, bytes.NewReader(data[8000:]), 2000); !errors.Is(err, ErrOffset) || cur.Received != 4000 {
		t.Fatalf("wrong offset: %v received=%d", err, cur.Received)
	}
	// Re-creating the same upload (app restarted) returns the existing one.
	again, err := s.Create(ctx, "group-0001", "Album/Disc 1/01.flac", int64(len(data)), "")
	if err != nil || again.ID != u.ID || again.Received != 4000 {
		t.Fatalf("resume lookup: %+v %v", again, err)
	}
	if _, err := s.Append(ctx, u.ID, 4000, bytes.NewReader(data[4000:]), 6000); err != nil {
		t.Fatal(err)
	}
	done, err := s.Complete(ctx, u.ID)
	if err != nil || done.State != StateComplete {
		t.Fatalf("complete: %+v %v", done, err)
	}
	got, err := os.ReadFile(filepath.Join(s.GroupDir("group-0001"), "Album", "Disc 1", "01.flac"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("staged file differs from what was sent")
	}
	if n, err := s.ReadyForImport(ctx, "group-0001"); err != nil || n != 1 {
		t.Fatalf("ready: %d %v", n, err)
	}
}

func TestChecksumMismatchResets(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1<<30)
	wrong := sha256.Sum256([]byte("something else"))
	u, _ := s.Create(ctx, "group-0002", "a.mp3", 5, hex.EncodeToString(wrong[:]))
	s.Append(ctx, u.ID, 0, bytes.NewReader([]byte("hello")), 5)
	if _, err := s.Complete(ctx, u.ID); err == nil {
		t.Fatal("bad checksum accepted")
	}
	if cur, _ := s.Get(ctx, u.ID); cur.Received != 0 || cur.State != StateReceiving {
		t.Fatalf("after mismatch: %+v", cur)
	}
	if _, err := s.ReadyForImport(ctx, "group-0002"); err == nil {
		t.Fatal("group with a receiving file reported ready")
	}
}

func TestBudgetIsShared(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1000)
	if _, err := s.Create(ctx, "group-0003", "a.flac", 2000, ""); !errors.Is(err, ErrOverBudget) {
		t.Fatalf("over budget: %v", err)
	}
	s.budget.Use(staging.OnDisk(func(context.Context) int64 { return 900 })) // downloads hold 900 bytes
	if _, err := s.Create(ctx, "group-0003", "b.flac", 200, ""); !errors.Is(err, ErrOverBudget) {
		t.Fatalf("shared budget ignored: %v", err)
	}
	if _, err := s.Create(ctx, "group-0003", "c.flac", 100, ""); err != nil {
		t.Fatalf("fits exactly: %v", err)
	}
	if _, err := s.Create(ctx, "bad group!", "x", 1, ""); err == nil {
		t.Fatal("bad group name accepted")
	}
}

func TestRemoveGroupClearsStagingAndRecords(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1<<30)
	u, _ := s.Create(ctx, "group-0004", "Scans/01.jpg", 3, "")
	s.Append(ctx, u.ID, 0, bytes.NewReader([]byte("jpg")), 3)
	s.Complete(ctx, u.ID)
	if err := s.RemoveGroup(ctx, "group-0004"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.GroupDir("group-0004")); !os.IsNotExist(err) {
		t.Fatal("group folder still exists")
	}
	if _, err := s.Get(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("upload record still exists")
	}
	if err := s.RemoveGroup(ctx, "../../etc"); err == nil {
		t.Fatal("path-like group accepted")
	}
}

// A stop between moving the finished file into place and recording it: Complete sent again (or the
// start-up recovery) finishes the same upload with the same bytes (review #5).
func TestCompleteRecoversAfterRename(t *testing.T) {
	ctx := context.Background()
	for _, viaRecover := range []bool{false, true} {
		s := newStore(t, 1<<30)
		sum := sha256.Sum256([]byte("hello"))
		u, _ := s.Create(ctx, "group-0009", "a.mp3", 5, hex.EncodeToString(sum[:]))
		s.Append(ctx, u.ID, 0, bytes.NewReader([]byte("hello")), 5)
		os.MkdirAll(filepath.Dir(s.finalPath(u)), 0o700)
		if err := os.Rename(s.partPath(u), s.finalPath(u)); err != nil { // the crash point
			t.Fatal(err)
		}
		again := New(s.db, s.root, 1<<30) // a restart
		if viaRecover {
			if n, err := again.Recover(ctx); err != nil || n != 1 {
				t.Fatalf("recover %d %v", n, err)
			}
		} else if got, err := again.Complete(ctx, u.ID); err != nil || got.State != StateComplete {
			t.Fatalf("complete again: %+v %v", got, err)
		}
		if n, err := again.ReadyForImport(ctx, "group-0009"); err != nil || n != 1 {
			t.Fatalf("ready %d %v", n, err)
		}
		data, _ := os.ReadFile(filepath.Join(again.GroupDir("group-0009"), "a.mp3"))
		if string(data) != "hello" {
			t.Fatalf("bytes %q", data)
		}
	}
	// A file in place with other bytes is not trusted: the upload starts over.
	s := newStore(t, 1<<30)
	sum := sha256.Sum256([]byte("hello"))
	u, _ := s.Create(ctx, "group-0010", "b.mp3", 5, hex.EncodeToString(sum[:]))
	s.Append(ctx, u.ID, 0, bytes.NewReader([]byte("hello")), 5)
	os.Remove(s.partPath(u))
	os.WriteFile(s.finalPath(u), []byte("jello"), 0o600)
	if _, err := s.Complete(ctx, u.ID); err == nil {
		t.Fatal("wrong bytes accepted")
	}
	if cur, _ := s.Get(ctx, u.ID); cur.Received != 0 || cur.State != StateReceiving {
		t.Fatalf("after mismatch %+v", cur)
	}
}

// Two uploads that each fit, started at once, do not both get in (review #4).
func TestConcurrentCreatesStayWithinBudget(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1000)
	var wg sync.WaitGroup
	var ok atomic.Int32
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.Create(ctx, "group-0011", fmt.Sprintf("f%d.flac", i), 700, ""); err == nil {
				ok.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if ok.Load() != 1 || s.Committed(ctx) != 700 {
		t.Fatalf("granted %d, committed %d", ok.Load(), s.Committed(ctx))
	}
}

// An unfinished selection is listed, can be cancelled (giving its reservation back) unless an import
// is at work on it, and expires when left alone (review #6).
func TestUnfinishedGroups(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1000)
	u, _ := s.Create(ctx, "group-0020", "a.flac", 700, "")
	s.Append(ctx, u.ID, 0, bytes.NewReader(make([]byte, 100)), 100)
	gs, err := s.Groups(ctx)
	if err != nil || len(gs) != 1 || gs[0].Group != "group-0020" || gs[0].Size != 700 || gs[0].Received != 100 || gs[0].Importing {
		t.Fatalf("groups %+v %v", gs, err)
	}
	// The same selection again reuses its reservation; another one does not fit beside it.
	if again, err := s.Create(ctx, "group-0020", "a.flac", 700, ""); err != nil || again.ID != u.ID {
		t.Fatalf("resume %+v %v", again, err)
	}
	if _, err := s.Create(ctx, "group-0021", "b.flac", 700, ""); !errors.Is(err, ErrOverBudget) {
		t.Fatalf("second reservation: %v", err)
	}
	// Being imported: kept.
	s.db.Exec(`INSERT INTO import_batches (kind, source, state, created_at) VALUES ('upload', 'group-0020', 'running', 0)`)
	if err := s.CancelGroup(ctx, "group-0020"); !errors.Is(err, ErrBusy) {
		t.Fatalf("cancel while importing: %v", err)
	}
	s.db.Exec(`UPDATE import_batches SET state = 'canceled'`)
	if err := s.CancelGroup(ctx, "group-0020"); err != nil {
		t.Fatal(err)
	}
	if s.Committed(ctx) != 0 {
		t.Fatalf("still committed %d", s.Committed(ctx))
	}
	if _, err := os.Stat(s.GroupDir("group-0020")); !os.IsNotExist(err) {
		t.Fatal("staging kept")
	}
	// Left alone for a week: expired.
	s.Create(ctx, "group-0022", "c.flac", 10, "")
	s.db.Exec(`UPDATE uploads SET updated_at = 0`)
	if n, err := s.Expire(ctx, 7*24*time.Hour); err != nil || n != 1 {
		t.Fatalf("expire %d %v", n, err)
	}
	if gs, _ := s.Groups(ctx); len(gs) != 0 {
		t.Fatalf("left %+v", gs)
	}
}

// send makes an upload of data at p in group, all sent: Complete is left to the caller.
func send(t *testing.T, s *Store, group, p string, data []byte) *Upload {
	t.Helper()
	ctx := context.Background()
	u, err := s.Create(ctx, group, p, int64(len(data)), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, u.ID, 0, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	return u
}

// Two uploads whose paths are a name and the name with ".part" each keep their bytes, whichever
// completes first, a restart between them or not (review #251): what one was received in was the
// other's place.
func TestUploadsReceivedApart(t *testing.T) {
	ctx := context.Background()
	for _, partFirst := range []bool{true, false} {
		for _, restart := range []bool{false, true} {
			s := newStore(t, 1<<30)
			song := send(t, s, "group-0030", "song.mp3", []byte("AAAA"))
			part := send(t, s, "group-0030", "song.mp3.part", []byte("BBBB"))
			first, second := song, part
			if partFirst {
				first, second = part, song
			}
			if _, err := s.Complete(ctx, first.ID); err != nil {
				t.Fatal(err)
			}
			if restart {
				s = New(s.db, s.root, 1<<30)
				if _, err := s.Recover(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := s.Complete(ctx, second.ID); err != nil || got.State != StateComplete {
				t.Fatalf("complete %+v %v", got, err)
			}
			for p, want := range map[string]string{"song.mp3": "AAAA", "song.mp3.part": "BBBB"} {
				if got, _ := os.ReadFile(filepath.Join(s.GroupDir("group-0030"), p)); string(got) != want {
					t.Fatalf("part first %v, restart %v: %s has %q", partFirst, restart, p, got)
				}
			}
			if list, _ := os.ReadDir(s.partsDir()); len(list) != 0 {
				t.Fatalf("received bytes left: %v", list)
			}
		}
	}
}

// What an older Kanade was receiving beside a file is taken up after the update and the upload
// goes on from where it was; one another upload's file overwrote starts over instead of taking
// that file's bytes, and received bytes no upload has go (review #251).
func TestOlderUploadsTakenUp(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1<<30)
	u, _ := s.Create(ctx, "group-0031", "Disc 1/01.flac", 8, "")
	s.Append(ctx, u.ID, 0, bytes.NewReader([]byte("0123")), 4)
	older := s.finalPath(u) + ".part" // where it was received
	os.MkdirAll(filepath.Dir(older), 0o700)
	if err := os.Rename(s.partPath(u), older); err != nil {
		t.Fatal(err)
	}
	// Another upload's file is where the next one's bytes were.
	v, _ := s.Create(ctx, "group-0031", "b.mp3", 4, "")
	s.Append(ctx, v.ID, 0, bytes.NewReader([]byte("vvvv")), 4)
	os.Remove(s.partPath(v))
	w := send(t, s, "group-0031", "b.mp3.part", []byte("wwww"))
	s.Complete(ctx, w.ID)
	os.WriteFile(filepath.Join(s.partsDir(), "999"), []byte("left"), 0o600)

	s = New(s.db, s.root, 1<<30) // the update
	if _, err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(older); !os.IsNotExist(err) {
		t.Fatal("the older bytes are still beside the file")
	}
	if _, err := s.Append(ctx, u.ID, 4, bytes.NewReader([]byte("4567")), 4); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Complete(ctx, u.ID); err != nil || got.State != StateComplete {
		t.Fatalf("complete %+v %v", got, err)
	}
	if got, _ := os.ReadFile(s.finalPath(u)); string(got) != "01234567" {
		t.Fatalf("taken up: %q", got)
	}
	if cur, err := s.Complete(ctx, v.ID); err == nil || cur.Received != 0 {
		t.Fatalf("v completed with another's bytes: %+v %v", cur, err)
	}
	if got, _ := os.ReadFile(filepath.Join(s.GroupDir("group-0031"), "b.mp3.part")); string(got) != "wwww" {
		t.Fatalf("w's file %q", got)
	}
	if _, err := os.Stat(filepath.Join(s.partsDir(), "999")); !os.IsNotExist(err) {
		t.Fatal("bytes of no upload kept")
	}
	// Bytes gone while being received: the client is sent back to the start, not given zeros.
	x, _ := s.Create(ctx, "group-0031", "c.mp3", 3, "")
	s.Append(ctx, x.ID, 0, bytes.NewReader([]byte("cc")), 2)
	os.Remove(s.partPath(x))
	if cur, err := s.Append(ctx, x.ID, 2, bytes.NewReader([]byte("c")), 1); !errors.Is(err, ErrOffset) || cur.Received != 0 {
		t.Fatalf("lost bytes: %+v %v", cur, err)
	}
}

// Making many uploads does not walk the staging folder for each (review #258): what the uploads
// hold is counted from their records, the folder measured again at most once a while for what no
// record accounts for.
func TestUsageFromRecords(t *testing.T) {
	ctx := context.Background()
	walks := 0
	measure = func(dir string) int64 { walks++; return dirSize(dir) }
	t.Cleanup(func() { measure = dirSize })
	s := newStore(t, 1<<30)
	for i := range 300 {
		u, err := s.Create(ctx, "group-0040", fmt.Sprintf("Scans/%03d.jpg", i), 10, "")
		if err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			s.Append(ctx, u.ID, 0, bytes.NewReader(make([]byte, 4)), 4)
		}
	}
	if walks > 1 {
		t.Fatalf("%d walks for 300 uploads", walks)
	}
	if u := s.Usage(ctx); u.Disk != 600 || u.Pending != 2400 {
		t.Fatalf("usage %+v", u)
	}
	// Bytes no upload has are counted once measured.
	os.WriteFile(filepath.Join(s.root, "left.bin"), make([]byte, 50), 0o600)
	s.extraAt = time.Now().Add(-extraEvery)
	if u := s.Usage(ctx); u.Disk != 650 || walks != 2 {
		t.Fatalf("with bytes of none: %+v, %d walks", u, walks)
	}
	if err := s.RemoveGroup(ctx, "group-0040"); err != nil {
		t.Fatal(err)
	}
	if u := s.Usage(ctx); u.Disk != 50 || u.Pending != 0 {
		t.Fatalf("after removal %+v", u)
	}
}
