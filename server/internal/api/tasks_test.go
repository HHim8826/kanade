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

// Every finished task can be reached however many there are: the page keeps the latest ones up to
// date and lists the older ones 50 at a time, by ID; a task older than those kept up to date that
// is fetched again and finishes again still comes with the update (review #68).
func TestTaskHistoryReachesEveryRecord(t *testing.T) {
	s, h := newTestServer(t)
	token := loginToken(t, s, h)
	s.downloads = downloader.NewService(s.db, nil, s.importer, t.TempDir(), 2<<30, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	tx, _ := s.db.Begin()
	tx.Exec(`INSERT INTO downloads (source, name, state, dir, files_removed, created_at, updated_at) VALUES ('magnet:', 'x', 'selecting', '', 0, 0, 0)`)
	tx.Exec(`INSERT INTO import_batches (kind, source, state, created_at) VALUES ('upload', 'x', 'review', 0)`)
	for i := 0; i < 1100; i++ {
		tx.Exec(`INSERT INTO downloads (source, name, state, dir, files_removed, created_at, updated_at) VALUES ('magnet:', 'x', 'completed', '', 1, 0, 0)`)
		tx.Exec(`INSERT INTO import_batches (kind, source, state, created_at, finished_at) VALUES ('upload', 'x', 'done', 0, 0)`)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	type item struct {
		ID    int64
		State string
	}
	var first struct {
		Downloads, Imports []item
		MoreDownloads      bool `json:"more_downloads"`
		Now                int64
	}
	get := func(path string, v any) {
		t.Helper()
		rec := do(t, h, "GET", path, token, nil)
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), v) != nil {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
	get("/api/v1/tasks", &first)
	if len(first.Downloads) != 51 || first.Downloads[0].ID != 1101 || first.Downloads[50].ID != 1 || !first.MoreDownloads || len(first.Imports) != 51 {
		t.Fatalf("first page: %d, more %v", len(first.Downloads), first.MoreDownloads)
	}
	oldest := func(l []item) int64 { // the oldest finished one listed
		n := int64(0)
		for _, x := range l {
			if x.State == "completed" || x.State == "done" {
				n = x.ID
			}
		}
		return n
	}
	for kind, listed := range map[string][]item{"downloads": first.Downloads, "imports": first.Imports} {
		seen := map[int64]bool{}
		for _, d := range listed {
			seen[d.ID] = true
		}
		before := oldest(listed)
		if before != 1052 {
			t.Fatalf("%s: oldest listed %d", kind, before)
		}
		pages := 0
		for {
			var page struct {
				Items []item
				More  bool
			}
			get("/api/v1/tasks/older?kind="+kind+"&before="+strconv.FormatInt(before, 10), &page)
			for _, x := range page.Items {
				if seen[x.ID] || x.ID >= before {
					t.Fatalf("%s: %d twice or out of order", kind, x.ID)
				}
				seen[x.ID] = true
				before = x.ID
			}
			pages++
			if !page.More {
				break
			}
		}
		if len(seen) != 1101 || pages != 21 {
			t.Fatalf("%s: reached %d in %d pages", kind, len(seen), pages)
		}
	}
	// Kept up to date from the oldest of the first page on, with the unfinished ones; an older one
	// fetched again and finished again comes too.
	since := strconv.FormatInt(oldest(first.Downloads), 10)
	s.db.Exec(`UPDATE downloads SET updated_at = ? WHERE id = 300`, first.Now+5)
	s.db.Exec(`UPDATE import_batches SET finished_at = ? WHERE id = 301`, first.Now+5)
	var upd struct{ Downloads, Imports []item }
	get("/api/v1/tasks?since_downloads="+since+"&since_imports="+since+"&changed="+strconv.FormatInt(first.Now, 10), &upd)
	ids := func(l []item) map[int64]bool {
		m := map[int64]bool{}
		for _, x := range l {
			m[x.ID] = true
		}
		return m
	}
	d, b := ids(upd.Downloads), ids(upd.Imports)
	if len(upd.Downloads) != 52 || !d[1] || !d[300] || !d[1101] || len(upd.Imports) != 52 || !b[1] || !b[301] {
		t.Fatalf("kept up to date: %d downloads, %d imports", len(upd.Downloads), len(upd.Imports))
	}
}
