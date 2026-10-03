package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/HHim8826/kanade/server/internal/downloader"
)

// The task center lists every task that is under way or waits for the user, however many finished
// ones are newer (#58); finished ones come a page at a time, and their records can be removed
// without touching anything else, unless they still hold files (#54).
func TestTasksKeepUnfinishedAndClearFinished(t *testing.T) {
	s, h := newTestServer(t)
	token := loginToken(t, s, h)
	root := t.TempDir()
	s.downloads = downloader.NewService(s.db, nil, s.importer, root, 2<<30, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	exec := func(q string, args ...any) int64 {
		t.Helper()
		r, err := s.db.Exec(q, args...)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := r.LastInsertId()
		return id
	}
	download := func(state string, removed bool) int64 {
		id := exec(`INSERT INTO downloads (source, name, state, dir, files_removed, created_at, updated_at) VALUES ('magnet:', 'x', ?, '', ?, 0, 0)`,
			state, removed)
		dir := filepath.Join(root, strconv.FormatInt(id, 10))
		exec(`UPDATE downloads SET dir = ? WHERE id = ?`, dir, id)
		return id
	}
	batch := func(state, item string) int64 {
		id := exec(`INSERT INTO import_batches (kind, source, state, created_at) VALUES ('upload', 'x', ?, 0)`, state)
		exec(`INSERT INTO import_items (batch_id, local_path, rel_path, state, role, updated_at) VALUES (?, '/x', 'a.flac', ?, 'audio', 0)`, id, item)
		return id
	}
	// The oldest tasks still need the user: a download to choose files for, a failed one still
	// holding its files, an import in review and one with a failed file.
	selecting := download(downloader.StateSelecting, false)
	failed := download(downloader.StateFailed, false)
	os.MkdirAll(filepath.Join(root, strconv.FormatInt(failed, 10)), 0o700)
	review := batch("review", "pending")
	unsaved := batch("done", "failed")
	var doneDownload, doneBatch int64
	for i := 0; i < 55; i++ {
		doneDownload = download(downloader.StateCompleted, true)
		doneBatch = batch("done", "published")
	}
	type list struct {
		Downloads []struct {
			ID        int64
			Clearable bool
		}
		Imports []struct {
			ID        int64
			Clearable bool
		}
		MoreDownloads bool `json:"more_downloads"`
		MoreImports   bool `json:"more_imports"`
	}
	tasks := func(query string) list {
		t.Helper()
		rec := do(t, h, "GET", "/api/v1/tasks"+query, token, nil)
		var l list
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &l) != nil {
			t.Fatalf("tasks %d %s", rec.Code, rec.Body)
		}
		return l
	}
	has := func(l list, download, batch int64) (bool, bool) {
		d, b := false, false
		for _, x := range l.Downloads {
			d = d || x.ID == download
		}
		for _, x := range l.Imports {
			b = b || x.ID == batch
		}
		return d, b
	}
	l := tasks("")
	if len(l.Downloads) != 52 || len(l.Imports) != 52 || !l.MoreDownloads || !l.MoreImports {
		t.Fatalf("first page: %d downloads, %d imports, more %v %v", len(l.Downloads), len(l.Imports), l.MoreDownloads, l.MoreImports)
	}
	for _, c := range [][2]int64{{selecting, review}, {failed, unsaved}} {
		if d, b := has(l, c[0], c[1]); !d || !b {
			t.Fatalf("unfinished %v missing: %v %v", c, d, b)
		}
	}
	if l := tasks("?history=100"); len(l.Downloads) != 57 || len(l.Imports) != 57 || l.MoreDownloads || l.MoreImports {
		t.Fatalf("all of it: %d %d %v %v", len(l.Downloads), len(l.Imports), l.MoreDownloads, l.MoreImports)
	}

	for _, path := range []string{"/api/v1/downloads/" + strconv.FormatInt(selecting, 10) + "/clear",
		"/api/v1/downloads/" + strconv.FormatInt(failed, 10) + "/clear", // its files are still there
		"/api/v1/imports/" + strconv.FormatInt(review, 10) + "/clear",
		"/api/v1/imports/" + strconv.FormatInt(unsaved, 10) + "/clear"} {
		if rec := do(t, h, "POST", path, token, nil); rec.Code != http.StatusConflict {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
	for _, path := range []string{"/api/v1/downloads/" + strconv.FormatInt(doneDownload, 10) + "/clear",
		"/api/v1/imports/" + strconv.FormatInt(doneBatch, 10) + "/clear"} {
		if rec := do(t, h, "POST", path, token, nil); rec.Code != http.StatusNoContent {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
	if d, b := has(tasks("?history=100"), doneDownload, doneBatch); d || b {
		t.Fatal("removed records still listed")
	}
	rec := do(t, h, "POST", "/api/v1/tasks/clear", token, nil)
	var n struct{ Downloads, Imports int }
	json.Unmarshal(rec.Body.Bytes(), &n)
	if rec.Code != 200 || n.Downloads != 54 || n.Imports != 54 {
		t.Fatalf("clear all: %d %s", rec.Code, rec.Body)
	}
	l = tasks("")
	if len(l.Downloads) != 2 || len(l.Imports) != 2 || l.MoreDownloads || l.MoreImports {
		t.Fatalf("after clearing: %+v", l)
	}
	// The records are still there for what depends on them.
	var kept int
	s.db.QueryRow(`SELECT count(*) FROM import_items i JOIN import_batches b ON b.id = i.batch_id WHERE b.cleared_at IS NOT NULL`).Scan(&kept)
	if kept != 55 {
		t.Fatalf("items of cleared batches: %d", kept)
	}
	// A failed download whose files are gone can go.
	os.RemoveAll(filepath.Join(root, strconv.FormatInt(failed, 10)))
	if rec := do(t, h, "POST", "/api/v1/downloads/"+strconv.FormatInt(failed, 10)+"/clear", token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("failed download without files: %d", rec.Code)
	}
}
