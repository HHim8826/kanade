package downloader

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/importer"
	"github.com/HHim8826/kanade/server/internal/library"
)

// bencode encodes the subset of types a .torrent needs.
func bencode(w *bytes.Buffer, v any) {
	switch x := v.(type) {
	case int:
		fmt.Fprintf(w, "i%de", x)
	case string:
		fmt.Fprintf(w, "%d:%s", len(x), x)
	case []byte:
		fmt.Fprintf(w, "%d:", len(x))
		w.Write(x)
	case []any:
		w.WriteByte('l')
		for _, e := range x {
			bencode(w, e)
		}
		w.WriteByte('e')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		w.WriteByte('d')
		for _, k := range keys {
			bencode(w, k)
			bencode(w, x[k])
		}
		w.WriteByte('e')
	}
}

// makeTorrent builds a multi-file torrent of dir/name whose only source is a web seed (BEP 19).
func makeTorrent(t *testing.T, dir, name, webseed string, files []string) []byte {
	t.Helper()
	const pieceLen = 32 << 10
	var all bytes.Buffer
	var list []any
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(dir, name, f))
		if err != nil {
			t.Fatal(err)
		}
		all.Write(data)
		var parts []any
		for _, p := range strings.Split(f, "/") {
			parts = append(parts, p)
		}
		list = append(list, map[string]any{"length": len(data), "path": parts})
	}
	var pieces bytes.Buffer
	for b := all.Bytes(); len(b) > 0; {
		n := min(pieceLen, len(b))
		sum := sha1.Sum(b[:n])
		pieces.Write(sum[:])
		b = b[n:]
	}
	var out bytes.Buffer
	bencode(&out, map[string]any{
		"url-list": webseed,
		"info":     map[string]any{"name": name, "piece length": pieceLen, "pieces": pieces.Bytes(), "files": list},
	})
	return out.Bytes()
}

type localDrive struct{ uploads int }

func (l *localDrive) Folder(_ context.Context, p string) (string, error) { return p, nil }
func (l *localDrive) Upload(_ context.Context, u gdrive.Upload) (gdrive.File, error) {
	data, err := os.ReadFile(u.Path)
	if err != nil {
		return gdrive.File{}, err
	}
	l.uploads++
	sum := sha256.Sum256(data)
	return gdrive.File{ID: fmt.Sprintf("f%d", l.uploads), SHA256Checksum: hex.EncodeToString(sum[:])}, nil
}

func aria2Path(t *testing.T) string {
	p := os.Getenv("KANADE_ARIA2")
	if p == "" {
		p = os.Getenv("SER1KA_ARIA2")
	}
	if p == "" {
		p = "/data/music-platform/tools/aria2/aria2c"
	}
	if _, err := os.Stat(p); err != nil {
		t.Skip("aria2c not available")
	}
	return p
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestTorrentToLibrary(t *testing.T) {
	bin := aria2Path(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tmp := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Content served over HTTP as the web seed.
	content := filepath.Join(tmp, "web")
	album := filepath.Join(content, "Album")
	os.MkdirAll(album, 0o755)
	for src, dst := range map[string]string{"tone.flac": "01 tone.flac", "tone-cbr.mp3": "02 tone.mp3", "cover.png": "cover.png"} {
		data, err := os.ReadFile(filepath.Join("../media/testdata", src))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(album, dst), data, 0o644)
	}
	os.WriteFile(filepath.Join(album, "bonus.mkv"), bytes.Repeat([]byte{7}, 100<<10), 0o644)
	web := httptest.NewServer(http.FileServer(http.Dir(content)))
	defer web.Close()
	torrent := makeTorrent(t, content, "Album", web.URL+"/", []string{"01 tone.flac", "02 tone.mp3", "bonus.mkv", "cover.png"})

	d, err := db.Open(ctx, filepath.Join(tmp, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	lib := library.New(d)
	drive := &localDrive{}
	imp := importer.New(d, lib, drive, filepath.Join(tmp, "staging"), log)
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
	defer func() { cancel(); <-ariaDone }() // reap aria2 before the test ends
	go svc.Run(ctx)
	waitFor(t, "aria2", 10*time.Second, aria.Ready)

	id, err := svc.Add(ctx, "", torrent, false)
	if err != nil {
		t.Fatal(err)
	}
	var v *View
	waitFor(t, "file list", 20*time.Second, func() bool {
		v, _ = svc.Get(ctx, id)
		return v != nil && v.State == StateSelecting
	})
	if v.Name != "Album" || len(v.Files) != 4 {
		t.Fatalf("download = %+v", v)
	}
	var pick []int
	for _, f := range v.Files {
		if (f.Path == "Album/bonus.mkv") == f.Suggested {
			t.Fatalf("suggestion for %s = %v", f.Path, f.Suggested)
		}
		if f.Suggested {
			pick = append(pick, f.Index)
		}
	}

	if err := svc.Select(ctx, id, pick); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "download and import", 60*time.Second, func() bool {
		v, _ = svc.Get(ctx, id)
		if v.State == StateFailed {
			t.Fatalf("download failed: %s", v.Error)
		}
		if v.ImportBatchID == 0 {
			return false
		}
		b, _ := imp.Batch(ctx, v.ImportBatchID)
		return b != nil && b.State == "done"
	})
	if v.State != StateSeeding && v.State != StateCompleted {
		t.Fatalf("state after download = %s", v.State)
	}
	// An unselected file can still receive the pieces it shares with selected neighbours
	// (P0 §4). It must stay a sparse fragment, never a full download.
	if st, err := os.Stat(filepath.Join(tmp, "downloads", fmt.Sprint(id), "Album", "bonus.mkv")); err == nil {
		onDisk := st.Sys().(*syscall.Stat_t).Blocks * 512
		t.Logf("unselected bonus.mkv: %d of %d bytes on disk", onDisk, st.Size())
		if onDisk > 2*(32<<10) {
			t.Fatalf("unselected video occupies %d bytes, more than its two boundary pieces", onDisk)
		}
	}
	if tracks, _ := lib.Tracks(ctx, 10, 0, ""); len(tracks) != 2 {
		t.Fatalf("library tracks = %d, want 2", len(tracks))
	}

	if err := svc.Cancel(ctx, id); err != nil && v.State == StateSeeding {
		t.Fatal(err)
	}
	waitFor(t, "files removed after seeding stopped", 10*time.Second, func() bool {
		v, _ = svc.Get(ctx, id)
		return v.FilesRemoved
	})
	if _, err := os.Stat(filepath.Join(tmp, "downloads", fmt.Sprint(id))); !os.IsNotExist(err) {
		t.Fatal("download folder still exists")
	}
}

func TestSuggestRules(t *testing.T) {
	files := []FileView{
		{Path: "A/01.flac", Length: 30 << 20},
		{Path: "A/a.cue", Length: 1 << 10},
		{Path: "A/BD/menu.m2ts", Length: 2 << 30},
		{Path: "A/info.nfo", Length: 4 << 10},
		{Path: "A/huge-readme.pdf", Length: 5 << 20},
		{Path: "A/BK/VTCL-1_01.png", Length: 200 << 20},
		{Path: "A/BK/VTCL-1_02.png", Length: 200 << 20},
	}
	suggest(files)
	want := []bool{true, true, false, true, false, true, false} // scans over 300 MB: only the _01 cover
	for i, f := range files {
		if f.Suggested != want[i] {
			t.Errorf("%s: suggested %v, want %v", f.Path, f.Suggested, want[i])
		}
	}
}

// Found in the real end-to-end run: a task waiting for file selection must survive an aria2
// restart with its GID, so that selecting files afterwards still works.
func TestSelectionSurvivesAria2Restart(t *testing.T) {
	bin := aria2Path(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tmp := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	content := filepath.Join(tmp, "web")
	os.MkdirAll(filepath.Join(content, "One"), 0o755)
	data, _ := os.ReadFile("../media/testdata/tone.flac")
	os.WriteFile(filepath.Join(content, "One", "a.flac"), data, 0o644)
	web := httptest.NewServer(http.FileServer(http.Dir(content)))
	defer web.Close()
	torrent := makeTorrent(t, content, "One", web.URL+"/", []string{"a.flac"})

	d, _ := db.Open(ctx, filepath.Join(tmp, "db.sqlite"))
	defer d.Close()
	for _, sub := range []string{"aria2", "downloads", "staging"} {
		os.MkdirAll(filepath.Join(tmp, sub), 0o700)
	}
	imp := importer.New(d, library.New(d), &localDrive{}, filepath.Join(tmp, "staging"), log)
	aria, _ := NewAria2(bin, filepath.Join(tmp, "aria2"), filepath.Join(tmp, "downloads"), log)
	svc := NewService(d, aria, imp, filepath.Join(tmp, "downloads"), 2<<30, 0, log)
	go imp.Run(ctx)
	go svc.Run(ctx)

	ariaCtx, stopAria := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { aria.Run(ariaCtx); close(done) }()
	waitFor(t, "aria2", 10*time.Second, aria.Ready)
	id, err := svc.Add(ctx, "", torrent, false)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "file list", 20*time.Second, func() bool { v, _ := svc.Get(ctx, id); return v.State == StateSelecting })

	stopAria() // aria2 saves its session and exits
	<-done
	done2 := make(chan struct{})
	go func() { aria.Run(ctx); close(done2) }()
	defer func() { cancel(); <-done2 }()
	waitFor(t, "aria2 again", 10*time.Second, aria.Ready)

	if err := svc.Select(ctx, id, []int{1}); err != nil {
		t.Fatalf("select after restart: %v", err)
	}
	waitFor(t, "download after restart", 30*time.Second, func() bool {
		v, _ := svc.Get(ctx, id)
		if v.State == StateFailed {
			t.Fatalf("failed: %s", v.Error)
		}
		return v.State == StateSeeding || v.State == StateCompleted
	})
}

func TestAutoSelectTakesSuggestedFiles(t *testing.T) {
	bin := aria2Path(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tmp := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	content := filepath.Join(tmp, "web")
	os.MkdirAll(filepath.Join(content, "Auto"), 0o755)
	data, _ := os.ReadFile("../media/testdata/tone.flac")
	os.WriteFile(filepath.Join(content, "Auto", "a.flac"), data, 0o644)
	os.WriteFile(filepath.Join(content, "Auto", "extra.mkv"), []byte("not music"), 0o644)
	web := httptest.NewServer(http.FileServer(http.Dir(content)))
	defer web.Close()
	torrent := makeTorrent(t, content, "Auto", web.URL+"/", []string{"a.flac", "extra.mkv"})

	d, _ := db.Open(ctx, filepath.Join(tmp, "db.sqlite"))
	defer d.Close()
	for _, sub := range []string{"aria2", "downloads", "staging"} {
		os.MkdirAll(filepath.Join(tmp, sub), 0o700)
	}
	imp := importer.New(d, library.New(d), &localDrive{}, filepath.Join(tmp, "staging"), log)
	aria, _ := NewAria2(bin, filepath.Join(tmp, "aria2"), filepath.Join(tmp, "downloads"), log)
	svc := NewService(d, aria, imp, filepath.Join(tmp, "downloads"), 2<<30, 0, log)
	go imp.Run(ctx)
	done := make(chan struct{})
	go func() { aria.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	go svc.Run(ctx)
	waitFor(t, "aria2", 10*time.Second, aria.Ready)

	id, err := svc.Add(ctx, "https://example.org/auto.torrent", torrent, true)
	if err != nil {
		t.Fatal(err)
	}
	var v *View
	waitFor(t, "download without a choice", 30*time.Second, func() bool {
		v, _ = svc.Get(ctx, id)
		if v.State == StateFailed {
			t.Fatalf("failed: %s", v.Error)
		}
		return v.State == StateSeeding || v.State == StateCompleted
	})
	if !v.AutoSelect || v.Source != "https://example.org/auto.torrent" {
		t.Fatalf("download %+v", v)
	}
	for _, f := range v.Files {
		if f.Selected != f.Suggested || (f.Path == "Auto/extra.mkv" && f.Selected) {
			t.Fatalf("file %+v", f)
		}
	}
}

// A pause for low disk that cannot be written is reported, not claimed, and while space is low the
// scheduler starts nothing, even a download whose pause was never recorded.
func TestDiskPauseWriteFailure(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d, _ := db.Open(ctx, filepath.Join(tmp, "db.sqlite"))
	defer d.Close()
	aria := &Aria2{RPC: &RPC{url: "http://127.0.0.1:1/jsonrpc", http: http.DefaultClient}} // unreachable: any call fails
	svc := NewService(d, aria, nil, filepath.Join(tmp, "downloads"), 2<<30, 0, log)
	if _, err := d.Exec(`INSERT INTO downloads (source, name, state, gid, dir, files, created_at, updated_at)
		VALUES ('test', 'q', 'queued', 'g1', ?, '[]', 0, 0)`, tmp); err != nil {
		t.Fatal(err)
	}
	d.Exec(`CREATE TRIGGER no_pause BEFORE UPDATE OF state ON downloads WHEN NEW.state = 'paused'
		BEGIN SELECT RAISE(FAIL, 'simulated write failure'); END`)
	svc.SetLowDisk(true)
	if n, err := svc.PauseForDisk(ctx); err == nil || n != 0 {
		t.Fatalf("pause reported %d, %v", n, err)
	}
	svc.tick(ctx)
	var state string
	d.QueryRow(`SELECT state FROM downloads`).Scan(&state)
	if state != StateQueued {
		t.Fatalf("started while the disk is low: %s", state)
	}
	// Once writes work again the pause is recorded, and resuming finds it.
	d.Exec(`DROP TRIGGER no_pause`)
	if n, err := svc.PauseForDisk(ctx); err != nil || n != 1 {
		t.Fatalf("pause: %d %v", n, err)
	}
	if n, _ := svc.DiskPaused(ctx); n != 1 {
		t.Fatalf("disk paused = %d", n)
	}
	svc.SetLowDisk(false)
	if n, err := svc.ResumeAfterDisk(ctx); err != nil || n != 1 {
		t.Fatalf("resume: %d %v", n, err)
	}
}

// A selection larger than the staging budget downloads in rounds: each is fetched, imported and
// cleared before the next; a file larger than the whole budget gets a round of its own (review #28).
func TestLargeSelectionDownloadsInRounds(t *testing.T) {
	bin := aria2Path(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tmp := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	content := filepath.Join(tmp, "web")
	files := map[string]string{"A/01 tone.flac": "tone.flac", "A/02 tone.mp3": "tone-cbr.mp3", "A/cover.png": "cover.png",
		"B/03 hires.flac": "tone-hires.flac", "C/04 tone.ogg": "tone.ogg"}
	var names []string
	for dst, src := range files {
		data, _ := os.ReadFile(filepath.Join("../media/testdata", src))
		os.MkdirAll(filepath.Dir(filepath.Join(content, "Box", dst)), 0o755)
		os.WriteFile(filepath.Join(content, "Box", dst), data, 0o644)
		names = append(names, dst)
	}
	sort.Strings(names)
	web := httptest.NewServer(http.FileServer(http.Dir(content)))
	defer web.Close()
	torrent := makeTorrent(t, content, "Box", web.URL+"/", names)

	d, _ := db.Open(ctx, filepath.Join(tmp, "db.sqlite"))
	defer d.Close()
	lib := library.New(d)
	for _, sub := range []string{"aria2", "downloads", "staging"} {
		os.MkdirAll(filepath.Join(tmp, sub), 0o700)
	}
	imp := importer.New(d, lib, &localDrive{}, filepath.Join(tmp, "staging"), log)
	aria, _ := NewAria2(bin, filepath.Join(tmp, "aria2"), filepath.Join(tmp, "downloads"), log)
	svc := NewService(d, aria, imp, filepath.Join(tmp, "downloads"), 105_000, 0, log) // A fits (beside the saved torrents); A and C do not; B is bigger than the budget
	go imp.Run(ctx)
	ariaDone := make(chan struct{})
	go func() { aria.Run(ctx); close(ariaDone) }()
	defer func() { cancel(); <-ariaDone }()
	go svc.Run(ctx)
	waitFor(t, "aria2", 10*time.Second, aria.Ready)

	id, err := svc.Add(ctx, "", torrent, false)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "file list", 20*time.Second, func() bool { v, _ := svc.Get(ctx, id); return v.State == StateSelecting })
	v, _ := svc.Get(ctx, id)
	var all []int
	for _, f := range v.Files {
		all = append(all, f.Index)
	}
	if err := svc.Select(ctx, id, all); err != nil {
		t.Fatalf("a selection over the budget was refused: %v", err)
	}
	dir := filepath.Join(tmp, "downloads", fmt.Sprint(id), "Box")
	var sawRound1Cleared bool
	waitFor(t, "every round", 90*time.Second, func() bool {
		v, _ = svc.Get(ctx, id)
		if v.State == StateFailed {
			t.Fatalf("failed: %s", v.Error)
		}
		if v.Round >= 2 {
			if _, err := os.Stat(filepath.Join(dir, "A/01 tone.flac")); os.IsNotExist(err) {
				sawRound1Cleared = true
			}
		}
		return (v.State == StateSeeding || v.State == StateCompleted) && v.ImportBatchID != 0 && func() bool {
			b, _ := imp.Batch(ctx, v.ImportBatchID)
			return b != nil && b.State == "done"
		}()
	})
	if v.Round != 3 {
		t.Fatalf("rounds = %d, files %+v", v.Round, v.Files)
	}
	if !sawRound1Cleared {
		t.Fatal("the first round's files were not cleared before the last round")
	}
	if tracks, _ := lib.Tracks(ctx, 10, 0, ""); len(tracks) != 4 {
		t.Fatalf("library tracks = %d, want 4", len(tracks))
	}
	albums, _ := lib.Albums(ctx, 10, 0, false)
	for _, a := range albums {
		if a.Title == "" {
			continue
		}
	}
	svc.Cancel(ctx, id)
	waitFor(t, "files removed", 10*time.Second, func() bool { v, _ = svc.Get(ctx, id); return v.FilesRemoved })
}
