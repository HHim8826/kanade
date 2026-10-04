package downloader

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/importer"
	"github.com/HHim8826/kanade/server/internal/library"
)

// directEnv runs a service with aria2, an importer and a library in a temporary folder.
type directEnv struct {
	svc  *Service
	lib  *library.Store
	imp  *importer.Importer
	root string
}

func newDirectEnv(t *testing.T) (context.Context, *directEnv) {
	t.Helper()
	bin := aria2Path(t)
	ctx, cancel := context.WithCancel(context.Background())
	tmp := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d, err := db.Open(ctx, filepath.Join(tmp, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	lib := library.New(d)
	imp := importer.New(d, lib, &localDrive{}, filepath.Join(tmp, "staging"), log)
	for _, sub := range []string{"aria2", "downloads", "staging"} {
		os.MkdirAll(filepath.Join(tmp, sub), 0o700)
	}
	aria, err := NewAria2(bin, filepath.Join(tmp, "aria2"), filepath.Join(tmp, "downloads"), log)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(d, aria, imp, filepath.Join(tmp, "downloads"), 2<<30, 0, log)
	go imp.Run(ctx)
	ariaDone := make(chan struct{})
	go func() { aria.Run(ctx); close(ariaDone) }()
	t.Cleanup(func() { cancel(); <-ariaDone; d.Close() }) // reap aria2 before the test ends
	go svc.Run(ctx)
	waitFor(t, "aria2", 10*time.Second, aria.Ready)
	return ctx, &directEnv{svc: svc, lib: lib, imp: imp, root: filepath.Join(tmp, "downloads")}
}

// imported waits for a download to finish and its import to be done.
func (e *directEnv) imported(t *testing.T, ctx context.Context, id int64) *View {
	t.Helper()
	var v *View
	waitFor(t, "download and import", 60*time.Second, func() bool {
		v, _ = e.svc.Get(ctx, id)
		if v.State == StateFailed {
			t.Fatalf("download failed: %s", v.Error)
		}
		if v.ImportBatchID == 0 {
			return false
		}
		b, _ := e.imp.Batch(ctx, v.ImportBatchID)
		return b != nil && b.State == "done"
	})
	return v
}

func testdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("../media/testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func zipOf(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, data := range files {
		f, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		f.Write(data)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A web link to a file downloads it directly: queued at once, fetched by aria2, imported, and its
// files cleared once the library has them; nothing is seeded. The name is the server's, and a link
// already downloading is not added twice.
func TestDirectDownloadToLibrary(t *testing.T) {
	ctx, e := newDirectEnv(t)
	album := zipOf(t, map[string][]byte{"Album/01 tone.flac": testdata(t, "tone.flac"), "Album/02 tone.mp3": testdata(t, "tone-cbr.mp3")})
	song := testdata(t, "tone.ogg")
	mux := http.NewServeMux()
	mux.HandleFunc("/files/album.zip", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "album.zip", time.Time{}, bytes.NewReader(album))
	})
	mux.HandleFunc("/get", func(w http.ResponseWriter, r *http.Request) { // a name only the server gives
		w.Header().Set("Content-Type", "audio/ogg")
		w.Header().Set("Content-Disposition", `attachment; filename*=UTF-8''%E6%AD%8C.ogg`)
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(song))
	})
	web := httptest.NewServer(mux)
	defer web.Close()

	id, err := e.svc.Add(ctx, web.URL+"/files/album.zip", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := e.svc.Get(ctx, id); v.Kind != KindDirect || v.Name != "album.zip" || v.TotalBytes != int64(len(album)) ||
		len(v.Files) != 1 || !v.Files[0].Selected || (v.State != StateQueued && v.State != StateDownloading) {
		t.Fatalf("added: %+v", v)
	}
	if _, err := e.svc.Add(ctx, web.URL+"/files/album.zip", nil, false); err == nil || !strings.Contains(err.Error(), "already download") {
		t.Fatalf("the same link again: %v", err)
	}
	v := e.imported(t, ctx, id)
	if tracks, _ := e.lib.Tracks(ctx, 10, 0, ""); len(tracks) != 2 {
		t.Fatalf("library tracks = %d, want 2", len(tracks))
	}
	waitFor(t, "completed and cleared", 10*time.Second, func() bool {
		v, _ = e.svc.Get(ctx, id)
		return v.State == StateCompleted && v.FilesRemoved
	})

	id2, err := e.svc.Add(ctx, web.URL+"/get", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := e.svc.Get(ctx, id2); v.Name != "歌.ogg" {
		t.Fatalf("name = %q", v.Name)
	}
	e.imported(t, ctx, id2)
	if tracks, _ := e.lib.Tracks(ctx, 10, 0, ""); len(tracks) != 3 {
		t.Fatalf("library tracks = %d, want 3", len(tracks))
	}
}

// What a web link gives decides what is added: a .torrent stays a torrent download; a web page, a
// file Kanade does not import, or one of unknown size is refused with why, and nothing is recorded.
func TestDirectLinksRefused(t *testing.T) {
	ctx, e := newDirectEnv(t)
	content := filepath.Join(t.TempDir(), "web")
	os.MkdirAll(filepath.Join(content, "Album"), 0o755)
	os.WriteFile(filepath.Join(content, "Album", "01 tone.flac"), testdata(t, "tone.flac"), 0o644)
	mux := http.NewServeMux()
	mux.Handle("/seed/", http.StripPrefix("/seed", http.FileServer(http.Dir(content))))
	var torrent []byte
	mux.HandleFunc("/dl/42", func(w http.ResponseWriter, r *http.Request) { // a torrent link without .torrent
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(torrent)
	})
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, "<!doctype html><title>Download</title>")
	})
	mux.HandleFunc("/setup.exe", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(make([]byte, 1000))
	})
	mux.HandleFunc("/stream.flac", func(w http.ResponseWriter, r *http.Request) { // no Content-Length
		w.Write(testdata(t, "tone.flac")[:100])
		w.(http.Flusher).Flush()
		w.Write([]byte("more"))
	})
	mux.HandleFunc("/gone.zip", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	web := httptest.NewServer(mux)
	defer web.Close()
	torrent = makeTorrent(t, content, "Album", web.URL+"/seed/", []string{"01 tone.flac"})

	id, err := e.svc.Add(ctx, web.URL+"/dl/42", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "file list", 20*time.Second, func() bool {
		v, _ := e.svc.Get(ctx, id)
		return v.State == StateSelecting
	})
	if v, _ := e.svc.Get(ctx, id); v.Kind != KindTorrent || v.InfoHash == "" {
		t.Fatalf("torrent link: %+v", v)
	}
	for path, want := range map[string]string{
		"/page":        "web page",
		"/setup.exe":   "not a file Kanade imports",
		"/stream.flac": "does not say how large",
		"/gone.zip":    "HTTP 404",
	} {
		if _, err := e.svc.Add(ctx, web.URL+path, nil, false); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", path, err, want)
		}
	}
	var n int
	e.svc.db.QueryRow(`SELECT count(*) FROM downloads`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d downloads recorded, want only the torrent", n)
	}
}

// slowReader serves its bytes a little at a time, so a download is still under way when the test
// acts on it.
type slowReader struct{ *bytes.Reader }

func (s slowReader) Read(p []byte) (int, error) { // about 160 KB/s
	time.Sleep(50 * time.Millisecond)
	return s.Reader.Read(p[:min(len(p), 8<<10)])
}

// A direct download whose aria2 task is lost (a restart before aria2 saved its session) is added
// again from its link and continues from what it had; one that failed is retried the same way.
func TestDirectDownloadComesBack(t *testing.T) {
	ctx, e := newDirectEnv(t)
	filler := make([]byte, 2<<20)
	rand.Read(filler)
	album := zipOf(t, map[string][]byte{"Album/01 tone.mp3": testdata(t, "tone-vbr.mp3"), "Album/booklet.bin": filler})
	var ranged atomic.Int32
	fail := atomic.Bool{}
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		if strings.HasPrefix(r.Header.Get("Range"), "bytes=") && !strings.HasPrefix(r.Header.Get("Range"), "bytes=0-") {
			ranged.Add(1)
		}
		http.ServeContent(w, r, "album.zip", time.Time{}, slowReader{bytes.NewReader(album)})
	}))
	defer web.Close()

	id, err := e.svc.Add(ctx, web.URL+"/album.zip", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var v *View
	waitFor(t, "a start", 20*time.Second, func() bool {
		v, _ = e.svc.Get(ctx, id)
		return v.State == StateDownloading && v.DoneBytes > 64<<10
	})
	r, _ := e.svc.load(ctx, id)
	e.svc.aria.RPC.Call(ctx, "forceRemove", nil, r.gid) // lost: as if aria2 restarted without it
	waitFor(t, "the task gone", 10*time.Second, func() bool {
		var st Status
		err := e.svc.aria.RPC.Call(ctx, "tellStatus", &st, r.gid, []string{"status"})
		if err == nil && st.Status == "removed" {
			e.svc.aria.RPC.Call(ctx, "removeDownloadResult", nil, r.gid)
		}
		return IsNotFound(err)
	})
	waitFor(t, "added again", 20*time.Second, func() bool {
		r2, _ := e.svc.load(ctx, id)
		return r2.gid != "" && r2.gid != r.gid && r2.State == StateDownloading
	})
	e.imported(t, ctx, id)
	if ranged.Load() == 0 {
		t.Error("the file was fetched again from the start, not continued")
	}
	if tracks, _ := e.lib.Tracks(ctx, 10, 0, ""); len(tracks) != 1 {
		t.Fatalf("library tracks = %d, want 1", len(tracks))
	}

	// A failed one is retried from its link.
	fail.Store(true)
	album2 := zipOf(t, map[string][]byte{"B/01 tone.opus": testdata(t, "tone.opus")})
	web2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() && strings.HasPrefix(r.UserAgent(), "aria2") { // the link checks out; the download fails
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		http.ServeContent(w, r, "b.zip", time.Time{}, bytes.NewReader(album2))
	}))
	defer web2.Close()
	id2, err := e.svc.Add(ctx, web2.URL+"/b.zip", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a failure", 60*time.Second, func() bool {
		v, _ = e.svc.Get(ctx, id2)
		return v.State == StateFailed
	})
	if !v.CanRetry {
		t.Fatalf("failed direct download cannot be retried: %+v", v)
	}
	fail.Store(false)
	if err := e.svc.Retry(ctx, id2); err != nil {
		t.Fatal(err)
	}
	e.imported(t, ctx, id2)
}

// Names a server gives become one plain file name.
func TestDirectFileNames(t *testing.T) {
	for in, want := range map[string]string{
		"../../etc/passwd":                 "passwd",
		`C:\music\a.flac`:                  "a.flac",
		".hidden.zip":                      "hidden.zip",
		"  spaced .mp3 ":                   "spaced .mp3",
		"bad\x00\x07name.ogg":              "badname.ogg",
		"":                                 "download",
		strings.Repeat("長", 100) + ".flac": strings.Repeat("長", 65) + ".flac",
	} {
		if got := cleanName(in); got != want {
			t.Errorf("cleanName(%q) = %q, want %q", in, got, want)
		}
	}
	resp := func(u, ctype, disp string) *http.Response {
		req := httptest.NewRequest(http.MethodGet, u, nil)
		h := http.Header{}
		h.Set("Content-Disposition", disp)
		return &http.Response{Request: req, Header: h}
	}
	for _, c := range []struct{ url, ctype, disp, want string }{
		{"https://example.com/a/%E6%9B%B2.flac", "audio/flac", "", "曲.flac"},
		{"https://example.com/download?id=7", "application/zip", "", "download.zip"},
		{"https://example.com/x", "audio/mpeg", `attachment; filename="Song.mp3"`, "Song.mp3"},
		{"https://example.com/x.php", "audio/mpeg", "", "x.php.mp3"},
	} {
		if got := fileName(resp(c.url, c.ctype, c.disp), c.ctype); got != c.want {
			t.Errorf("fileName(%s) = %q, want %q", c.url, got, c.want)
		}
	}
}
