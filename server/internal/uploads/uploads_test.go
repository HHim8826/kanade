package uploads

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/HHim8826/kanade/server/internal/db"
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
	s.Other = func(context.Context) int64 { return 900 } // downloads hold 900 bytes
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
