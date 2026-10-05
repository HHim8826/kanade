package loudness

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/ffmpeg"
	"github.com/HHim8826/kanade/server/internal/library"
)

func testdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "media", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// drive serves files by Drive ID; a file listed in broken stops halfway with an error.
type drive struct {
	files  map[string][]byte
	broken map[string]bool
}

type halfway struct{ r io.Reader }

func (h halfway) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	if err == io.EOF {
		return n, errors.New("connection reset")
	}
	return n, err
}

func (d *drive) OpenRange(_ context.Context, id string, _, _ int64) (*http.Response, error) {
	b, ok := d.files[id]
	if !ok {
		return nil, errors.New("no such file")
	}
	var body io.Reader = bytes.NewReader(b)
	if d.broken[id] {
		body = halfway{bytes.NewReader(b[:len(b)/2])}
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(body)}, nil
}

func setup(t *testing.T) (*Service, *library.Store, *drive, func(name, driveID string, data []byte) int64) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	ff := ffmpeg.Find(filepath.Join(filepath.Dir(file), "..", "..", "..", "var")) // tools/ is next to the data directory
	if ff == nil {
		t.Skip("no ffmpeg")
	}
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	lib := library.New(d)
	src := &drive{files: map[string][]byte{}, broken: map[string]bool{}}
	add := func(name, driveID string, data []byte) int64 {
		t.Helper()
		ctx := context.Background()
		a, err := lib.CreateAsset(ctx, library.Asset{SHA256: name, Size: int64(len(data)), Format: filepath.Ext(name)[1:], Codec: "x", DurationMS: 3000})
		if err != nil {
			t.Fatal(err)
		}
		if err := lib.MarkVerified(ctx, a.ID, driveID); err != nil {
			t.Fatal(err)
		}
		src.files[driveID] = data
		return a.ID
	}
	s := &Service{Lib: lib, DB: d, FF: ff, Source: src, Temp: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return s, lib, src, add
}

func measured(t *testing.T, lib *library.Store, ids ...int64) map[int64]library.Loudness {
	t.Helper()
	m, err := lib.AssetLoudness(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A scan measures every file not measured yet, streamed from Drive, or from the stream cache's
// copy when it has one. A file FFmpeg cannot read is recorded as failed (tried again only when
// asked); one Drive stopped sending is not recorded, so a later scan tries it again.
func TestScan(t *testing.T) {
	s, lib, src, add := setup(t)
	flac := add("a.flac", "d1", testdata(t, "tone.flac"))
	mp3 := add("b.mp3", "d2", testdata(t, "tone-cbr.mp3"))
	bad := add("c.flac", "d3", []byte("this is no audio file at all"))
	cut := add("d.flac", "d4", testdata(t, "tone.flac"))
	src.broken["d4"] = true
	cached := add("e.ogg", "d5", testdata(t, "tone.ogg"))
	delete(src.files, "d5") // only in the cache
	oggPath := filepath.Join("..", "media", "testdata", "tone.ogg")
	s.Hold = func(id string) (string, func(), bool) {
		if id == "d5" {
			return oggPath, func() {}, true
		}
		return "", nil, false
	}
	if err := s.Scan(false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for s.State().Running && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	st := s.State()
	if st.Running || st.Done != 3 || st.Failed != 2 || st.Error == "" { // the cut file is said to be tried again
		t.Fatalf("scan = %+v", st)
	}
	m := measured(t, lib, flac, mp3, bad, cut, cached)
	if len(m) != 3 || m[flac].LUFS > -10 || m[mp3].LUFS > -10 || m[cached].LUFS > -10 {
		t.Fatalf("measured = %+v", m)
	}
	if u, _ := lib.Unmeasured(context.Background(), 0, 10, false); len(u) != 1 || u[0].AssetID != cut {
		t.Fatalf("still to measure = %+v (only the cut one)", u)
	}
	if u, _ := lib.Unmeasured(context.Background(), 0, 10, true); len(u) != 2 {
		t.Fatalf("to measure again with failed = %+v", u)
	}
	// The connection is fine again: the next scan gets it.
	src.broken["d4"] = false
	s.Scan(false)
	for s.State().Running {
		time.Sleep(50 * time.Millisecond)
	}
	if m := measured(t, lib, cut); len(m) != 1 {
		t.Fatalf("after another scan: %+v", m)
	}
}

// A song is measured once playing has cached it whole, and as it is imported, unless it was
// measured already.
func TestCachedAndImported(t *testing.T) {
	s, lib, _, add := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	played := add("p.flac", "dp", testdata(t, "tone.flac"))
	path := filepath.Join("..", "media", "testdata", "tone.flac")
	s.Hold = func(id string) (string, func(), bool) { return path, func() {}, id == "dp" }
	s.Cached("dp")
	s.Cached("no-such-file")
	deadline := time.Now().Add(20 * time.Second)
	for len(measured(t, lib, played)) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if len(measured(t, lib, played)) != 1 {
		t.Fatal("a cached song was not measured")
	}
	imported := add("i.mp3", "di", testdata(t, "tone-vbr.mp3"))
	s.File(ctx, imported, filepath.Join("..", "media", "testdata", "tone-vbr.mp3"))
	if len(measured(t, lib, imported)) != 1 {
		t.Fatal("an imported song was not measured")
	}
	// Measured already: left as it is.
	if err := lib.SetLoudness(ctx, imported, -1, 0, "x"); err != nil {
		t.Fatal(err)
	}
	s.File(ctx, imported, filepath.Join("..", "media", "testdata", "tone-vbr.mp3"))
	if m := measured(t, lib, imported); m[imported].LUFS != -1 {
		t.Fatalf("measured again: %+v", m)
	}
}
