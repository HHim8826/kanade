package backup

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/gdrive"
)

type fakeDrive struct {
	mu     sync.Mutex
	files  map[string]gdrive.File
	data   map[string][]byte
	down   error
	nextID int
}

func (d *fakeDrive) Folder(_ context.Context, path string) (string, error) {
	if d.down != nil {
		return "", d.down
	}
	return "folder:" + path, nil
}

func (d *fakeDrive) Upload(_ context.Context, u gdrive.Upload) (gdrive.File, error) {
	b, err := os.ReadFile(u.Path)
	if err != nil {
		return gdrive.File{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nextID++
	f := gdrive.File{ID: fmt.Sprint("f", d.nextID), Name: u.Name, Parents: []string{u.ParentID}}
	d.files[f.ID], d.data[f.ID] = f, b
	return f, nil
}

func (d *fakeDrive) Children(_ context.Context, folder string) ([]gdrive.File, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []gdrive.File
	for _, f := range d.files {
		if f.Parents[0] == folder {
			out = append(out, f)
		}
	}
	return out, nil
}

func (d *fakeDrive) Trash(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	f := d.files[id]
	f.Trashed = true
	d.files[id] = f
	return nil
}

func (d *fakeDrive) add(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nextID++
	id := fmt.Sprint("f", d.nextID)
	d.files[id] = gdrive.File{ID: id, Name: name, Parents: []string{"folder:" + Folder}}
}

type counting struct {
	slog.Handler
	warns *atomic.Int32
}

func (c counting) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		c.warns.Add(1)
	}
	return nil
}

func setup(t *testing.T) (*Service, *fakeDrive, *atomic.Int32) {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := d.Exec(`INSERT INTO users (username, password_hash, created_at) VALUES ('admin', 'x', 1)`); err != nil {
		t.Fatal(err)
	}
	fd := &fakeDrive{files: map[string]gdrive.File{}, data: map[string][]byte{}}
	var warns atomic.Int32
	s := &Service{DB: d, Drive: fd, Dir: t.TempDir(), Keep: 14, Every: 24 * time.Hour,
		Log: slog.New(counting{slog.NewTextHandler(io.Discard, nil), &warns})}
	return s, fd, &warns
}

// A copy is the whole database, readable, in Kanade/backups; the 14 latest copies stay and older
// ones go to the trash, other files in the folder are left alone (review #161).
func TestCopyAndRotate(t *testing.T) {
	ctx := context.Background()
	s, fd, _ := setup(t)
	for i := range 16 {
		fd.add(fmt.Sprintf("kanade-db-202501%02d-000000.sqlite", i+1))
	}
	fd.add("notes.txt")
	if err := s.Now(ctx); err != nil {
		t.Fatal(err)
	}
	st := s.State(ctx)
	if st.Error != "" || st.Kept != 14 || st.Last == 0 || st.Name == "" {
		t.Fatalf("state %+v", st)
	}
	var copyID string
	kept, trashed := 0, 0
	for id, f := range fd.files {
		switch {
		case f.Name == st.Name:
			copyID = id
		case f.Name == "notes.txt" && f.Trashed:
			t.Fatal("another file went to the trash")
		}
		if f.Name != "notes.txt" {
			if f.Trashed {
				trashed++
			} else {
				kept++
			}
		}
	}
	if kept != 14 || trashed != 3 {
		t.Fatalf("kept %d, trashed %d", kept, trashed)
	}
	for _, f := range fd.files {
		if f.Trashed && f.Name > "kanade-db-20250103-000000.sqlite" {
			t.Fatalf("trashed %s, a newer one", f.Name)
		}
	}
	// The copy opens as the database it was.
	path := filepath.Join(t.TempDir(), "copy.sqlite")
	os.WriteFile(path, fd.data[copyID], 0o600)
	c, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var name string
	if err := c.QueryRow(`SELECT username FROM users`).Scan(&name); err != nil || name != "admin" {
		t.Fatalf("copy: %q %v", name, err)
	}
	if des, _ := os.ReadDir(s.Dir); len(des) != 0 {
		t.Fatalf("left %v", des)
	}
}

// Drive not connected is said, once in the log however often it is tried, and nothing counts as a
// copy; Run makes one when the last is a day old, not before.
func TestDriveDownAndRun(t *testing.T) {
	ctx := context.Background()
	s, fd, warns := setup(t)
	fd.down = gdrive.ErrNotConnected
	for range 3 {
		s.Now(ctx)
	}
	if st := s.State(ctx); st.Reason != "not_connected" || st.Last != 0 || warns.Load() != 1 {
		t.Fatalf("state %+v, %d warnings", st, warns.Load())
	}
	fd.down = nil

	old := firstRun
	firstRun = 10 * time.Millisecond
	defer func() { firstRun = old }()
	run := func() {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second) // room for a copy under the race detector
		defer cancel()
		s.Run(ctx)
	}
	run()
	first := s.State(ctx)
	if first.Last == 0 || first.Error != "" {
		t.Fatalf("no copy made: %+v", first)
	}
	time.Sleep(1100 * time.Millisecond) // a new name
	run()
	if s.State(ctx).Name != first.Name {
		t.Fatal("copied again within the day")
	}
}
