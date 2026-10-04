package downloader

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/importer"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/settings"
)

type rig struct {
	ctx     context.Context
	svc     *Service
	imp     *importer.Importer
	lib     *library.Store
	d       *sql.DB
	torrent []byte
	link    string      // where the torrent can be fetched again
	serve   atomic.Bool // whether link answers
	tmp     string
}

// newRig runs aria2, the importer and the downloader on the five test files of the rounds test,
// served by a local web seed; budget is the staging budget.
func newRig(t *testing.T, budget int64) *rig {
	return newRigWith(t, budget, nil, true)
}

// newRigWith is newRig with more files in the torrent (path -> content), and the importer started
// only when runImporter (else by the test).
func newRigWith(t *testing.T, budget int64, extra map[string]string, runImporter bool) *rig {
	bin := aria2Path(t)
	ctx, cancel := context.WithCancel(context.Background())
	r := &rig{ctx: ctx, tmp: t.TempDir()}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	content := filepath.Join(r.tmp, "web")
	files := map[string]string{"A/01 tone.flac": "tone.flac", "A/02 tone.mp3": "tone-cbr.mp3", "A/cover.png": "cover.png",
		"B/03 hires.flac": "tone-hires.flac", "C/04 tone.ogg": "tone.ogg"}
	var names []string
	for dst, src := range files {
		data, _ := os.ReadFile(filepath.Join("../media/testdata", src))
		os.MkdirAll(filepath.Dir(filepath.Join(content, "Box", dst)), 0o755)
		os.WriteFile(filepath.Join(content, "Box", dst), data, 0o644)
		names = append(names, dst)
	}
	for dst, body := range extra {
		os.WriteFile(filepath.Join(content, "Box", dst), []byte(body), 0o644)
		names = append(names, dst)
	}
	sort.Strings(names)
	web := httptest.NewServer(http.FileServer(http.Dir(content)))
	r.torrent = makeTorrent(t, content, "Box", web.URL+"/", names)
	r.serve.Store(true)
	links := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !r.serve.Load() {
			http.NotFound(w, nil)
			return
		}
		w.Write(r.torrent)
	}))
	r.link = links.URL + "/box.torrent"
	r.d, _ = db.Open(ctx, filepath.Join(r.tmp, "db.sqlite"))
	r.lib = library.New(r.d)
	for _, sub := range []string{"aria2", "downloads", "staging"} {
		os.MkdirAll(filepath.Join(r.tmp, sub), 0o700)
	}
	r.imp = importer.New(r.d, r.lib, &localDrive{}, filepath.Join(r.tmp, "staging"), log)
	aria, _ := NewAria2(bin, filepath.Join(r.tmp, "aria2"), filepath.Join(r.tmp, "downloads"), log)
	r.svc = NewService(r.d, aria, r.imp, filepath.Join(r.tmp, "downloads"), budget, 0, log)
	r.imp.Refetch = r.svc.Refetch
	if runImporter {
		go r.imp.Run(ctx)
	}
	ariaDone := make(chan struct{})
	go func() { aria.Run(ctx); close(ariaDone) }()
	t.Cleanup(func() { cancel(); <-ariaDone; web.Close(); links.Close(); r.d.Close() })
	go r.svc.Run(ctx)
	waitFor(t, "aria2", 10*time.Second, aria.Ready)
	return r
}

// add adds the torrent as if from its link, waits for the file list and selects every file.
func (r *rig) add(t *testing.T, prepare func(id int64)) int64 {
	id, err := r.svc.Add(r.ctx, r.link, r.torrent, false)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "file list", 20*time.Second, func() bool { v, _ := r.svc.Get(r.ctx, id); return v.State == StateSelecting })
	if prepare != nil {
		prepare(id)
	}
	v, _ := r.svc.Get(r.ctx, id)
	var all []int
	for _, f := range v.Files {
		all = append(all, f.Index)
	}
	if err := r.svc.Select(r.ctx, id, all); err != nil {
		t.Fatal(err)
	}
	return id
}

func (r *rig) finished(id int64) bool {
	v, _ := r.svc.Get(r.ctx, id)
	if (v.State != StateSeeding && v.State != StateCompleted) || v.ImportBatchID == 0 {
		return false
	}
	b, _ := r.imp.Batch(r.ctx, v.ImportBatchID)
	return b != nil && b.State == "done"
}

func (r *rig) tracks(t *testing.T) int {
	list, _ := r.lib.Tracks(r.ctx, 50, 0, "")
	return len(list)
}

// A download from before task.torrent has only the copy aria2 saved under the SHA-1 of the whole
// file; later rounds find it by its info hash (review #49).
func TestLegacyTaskFindsAria2Torrent(t *testing.T) {
	r := newRig(t, 105_000)
	id := r.add(t, func(id int64) {
		dir := filepath.Join(r.tmp, "downloads", fmt.Sprint(id))
		if err := os.Remove(filepath.Join(dir, taskTorrent)); err != nil {
			t.Fatal(err)
		}
		if m, _ := filepath.Glob(filepath.Join(dir, "*.torrent")); len(m) != 1 {
			t.Fatalf("aria2's own copy: %v", m)
		}
	})
	waitFor(t, "every round", 90*time.Second, func() bool {
		if v, _ := r.svc.Get(r.ctx, id); v.State == StateFailed {
			t.Fatalf("failed: %s", v.Error)
		}
		return r.finished(id)
	})
	if v, _ := r.svc.Get(r.ctx, id); v.Round != 3 || r.tracks(t) != 4 {
		t.Fatalf("rounds %d, tracks %d", v.Round, r.tracks(t))
	}
}

// With no torrent anywhere, the next round fails without losing anything: the files and the first
// round's songs stay, and a retry fetches the torrent from its link and finishes, without importing
// the first round again (review #49, #50).
func TestFailedRoundKeepsDataAndRetries(t *testing.T) {
	r := newRig(t, 105_000)
	r.serve.Store(false)
	id := r.add(t, func(id int64) {
		m, _ := filepath.Glob(filepath.Join(r.tmp, "downloads", fmt.Sprint(id), "*.torrent"))
		for _, p := range m {
			os.Remove(p)
		}
	})
	var v *View
	waitFor(t, "the second round to fail", 60*time.Second, func() bool {
		v, _ = r.svc.Get(r.ctx, id)
		return v.State == StateFailed
	})
	time.Sleep(3 * time.Second) // a few more polls: nothing may be cleaned up
	v, _ = r.svc.Get(r.ctx, id)
	dir := filepath.Join(r.tmp, "downloads", fmt.Sprint(id))
	first := r.tracks(t) // folder A, and C when it fits beside it
	if v.FilesRemoved || !v.CanRetry || v.Round != 1 || first < 2 || first > 3 {
		t.Fatalf("after the failure: removed %v, can retry %v, round %d, tracks %d (%s)", v.FilesRemoved, v.CanRetry, v.Round, r.tracks(t), v.Error)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the download folder went: %v", err)
	}
	if err := r.svc.Retry(r.ctx, id); err == nil {
		t.Fatal("retried without a torrent")
	}
	r.serve.Store(true)
	// The same torrent added again and under way: no second task for it.
	r.d.Exec(`INSERT INTO downloads (source, state, dir, info_hash, created_at, updated_at) SELECT 'again', 'downloading', '', info_hash, 0, 0
		FROM downloads WHERE id = ?`, id)
	if err := r.svc.Retry(r.ctx, id); err == nil {
		t.Fatal("retried beside another download of the same torrent")
	}
	r.d.Exec(`DELETE FROM downloads WHERE source = 'again'`)
	if err := r.svc.Retry(r.ctx, id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the remaining rounds", 90*time.Second, func() bool {
		if v, _ := r.svc.Get(r.ctx, id); v.State == StateFailed {
			t.Fatalf("failed again: %s", v.Error)
		}
		return r.finished(id)
	})
	if n := r.tracks(t); n != 4 {
		t.Fatalf("tracks %d", n)
	}
	var dups int
	r.d.QueryRow(`SELECT count(*) FROM import_items WHERE state = 'duplicate'`).Scan(&dups)
	if dups != 0 {
		t.Fatalf("the first round was imported again: %d duplicates", dups)
	}
}

// When the import cannot be set up, nothing moves on and nothing is removed; the hand-over is
// tried again and the download finishes (review #43): a middle round, then the last.
func TestImportAdmissionFailureKeepsDownload(t *testing.T) {
	defer func(d time.Duration) { importRetry = d }(importRetry)
	importRetry = 200 * time.Millisecond
	for _, tc := range []struct {
		name   string
		budget int64
	}{{"middle round", 105_000}, {"last round", 10 << 20}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, tc.budget)
			r.d.Exec(`CREATE TRIGGER reject_import BEFORE INSERT ON import_batches BEGIN SELECT RAISE(FAIL, 'simulated import admission failure'); END`)
			id := r.add(t, nil)
			waitFor(t, "a failed hand-over", 60*time.Second, func() bool {
				v, _ := r.svc.Get(r.ctx, id)
				return v.Error != ""
			})
			time.Sleep(3 * time.Second)
			v, _ := r.svc.Get(r.ctx, id)
			if v.State != StateDownloading || v.FilesRemoved || v.ImportBatchID != 0 {
				t.Fatalf("moved on without an import: %s removed %v batch %d", v.State, v.FilesRemoved, v.ImportBatchID)
			}
			if m, _ := filepath.Glob(filepath.Join(r.tmp, "downloads", fmt.Sprint(id), "Box", "A", "*.flac")); len(m) != 1 {
				t.Fatalf("files gone: %v", m)
			}
			r.d.Exec(`DROP TRIGGER reject_import`)
			waitFor(t, "the download to finish", 90*time.Second, func() bool { return r.finished(id) })
			if n := r.tracks(t); n != 4 {
				t.Fatalf("tracks %d", n)
			}
		})
	}
}

// Files lost after a round handed them over (review #57): the import fails for them; its retry has
// the download fetch them again from the saved torrent, and once a round has them the import that
// lost them is retried, so the rip log joins its album and the song is imported. Lost in an earlier
// round, a later round may have written part of them back (its pieces reach into them): they are
// fetched again all the same, checked against the torrent.
func TestLostFilesFetchedAgain(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget int64
	}{{"last round", 10 << 20}, {"earlier round", 105_000}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRigWith(t, tc.budget, map[string]string{"A/rip.log": "Exact Audio Copy V1.0 beta 3 from 29. August 2011\n"}, false)
			id := r.add(t, nil)
			var first int64
			waitFor(t, "the first round handed over", 30*time.Second, func() bool {
				v, _ := r.svc.Get(r.ctx, id)
				if v.State == StateFailed {
					t.Fatalf("failed: %s", v.Error)
				}
				first = v.ImportBatchID
				return first != 0 && v.State != StateDownloading
			})
			dir := filepath.Join(r.tmp, "downloads", fmt.Sprint(id), "Box", "A")
			for _, name := range []string{"rip.log", "02 tone.mp3"} {
				if err := os.Remove(filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
			}
			go r.imp.Run(r.ctx)
			waitFor(t, "every round", 90*time.Second, func() bool { return r.finished(id) })
			b, _ := r.imp.Batch(r.ctx, first)
			if b.Counts["failed"] != 2 {
				t.Fatalf("first batch %v", b.Counts)
			}
			tracks := r.tracks(t)
			rounds := func() int { v, _ := r.svc.Get(r.ctx, id); return v.Round }
			before := rounds()

			res, err := r.imp.Retry(r.ctx, first)
			if err != nil || res.Fetching != 2 || res.Requeued != 0 {
				t.Fatalf("retry %+v %v", res, err)
			}
			waitFor(t, "fetched again and imported", 60*time.Second, func() bool {
				b, _ := r.imp.Batch(r.ctx, first)
				return b.State == "done" && b.Counts["failed"] == 0 && b.Counts["pending"] == 0 && r.finished(id)
			})
			b, _ = r.imp.Batch(r.ctx, first)
			if b.Unsaved != 0 {
				t.Fatalf("first batch after the retry: %+v", b.Counts)
			}
			if got := r.tracks(t); got != tracks+1 {
				t.Fatalf("tracks %d, want %d", got, tracks+1)
			}
			var logs, batches int
			r.d.QueryRow(`SELECT count(*) FROM sidecars WHERE kind = 'log'`).Scan(&logs)
			r.d.QueryRow(`SELECT count(*) FROM import_batches`).Scan(&batches)
			if logs != 1 || batches != before {
				t.Fatalf("rip logs %d, batches %d for %d rounds: the files fetched again go to the import that lost them", logs, batches, before)
			}
			if rounds() != before+1 {
				t.Fatalf("rounds %d, before %d", rounds(), before)
			}
		})
	}
}

// Files fetched again stay marked for the import that lost them until it has them back in its
// queue: a hand-over that fails is done again later, and a service started afresh (a restart) does
// it too; the user does not have to retry again (review #66).
func TestFetchedAgainSurvivesFailedHandOver(t *testing.T) {
	old := importRetry
	importRetry = 300 * time.Millisecond
	t.Cleanup(func() { importRetry = old })
	r := newRigWith(t, 10<<20, map[string]string{"A/rip.log": "Exact Audio Copy V1.0 beta 3 from 29. August 2011\n"}, false)
	id := r.add(t, nil)
	var first int64
	waitFor(t, "handed over", 30*time.Second, func() bool {
		v, _ := r.svc.Get(r.ctx, id)
		first = v.ImportBatchID
		return first != 0 && v.State != StateDownloading
	})
	dir := filepath.Join(r.tmp, "downloads", fmt.Sprint(id), "Box", "A")
	for _, name := range []string{"rip.log", "02 tone.mp3"} {
		os.Remove(filepath.Join(dir, name))
	}
	go r.imp.Run(r.ctx)
	waitFor(t, "imported", 60*time.Second, func() bool { return r.finished(id) })
	if _, err := r.d.Exec(`CREATE TRIGGER deny_handoff BEFORE UPDATE OF error ON import_items WHEN NEW.error = 'fetched again'
		BEGIN SELECT RAISE(ABORT, 'simulated transient hand-over failure'); END`); err != nil {
		t.Fatal(err)
	}
	if res, err := r.imp.Retry(r.ctx, first); err != nil || res.Fetching != 2 {
		t.Fatalf("retry %+v %v", res, err)
	}
	marked := func() (again, fetched int) {
		row, _ := r.svc.load(r.ctx, id)
		for _, f := range row.files {
			if f.Again == first {
				again++
				if f.Fetched {
					fetched++
				}
			}
		}
		return
	}
	waitFor(t, "fetched again", 60*time.Second, func() bool { _, n := marked(); return n == 2 })
	time.Sleep(time.Second) // a few hand-overs, each refused
	if again, _ := marked(); again != 2 {
		t.Fatalf("marks after failed hand-overs: %d", again)
	}
	if b, _ := r.imp.Batch(r.ctx, first); b.State != "done" || b.Counts["failed"] != 2 {
		t.Fatalf("batch %s %v", b.State, b.Counts)
	}
	// A service started afresh on the same database (a restart) hands them over once it can; the
	// running one is held off, so the fresh one is what does it.
	r.svc.mu.Lock()
	r.svc.handWait[first] = time.Now().Add(time.Hour)
	r.svc.mu.Unlock()
	time.Sleep(500 * time.Millisecond) // a hand-over under way ends (refused)
	r.d.Exec(`DROP TRIGGER deny_handoff`)
	fresh := NewService(r.d, nil, r.imp, filepath.Join(r.tmp, "downloads"), 10<<20, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	row, _ := fresh.load(r.ctx, id)
	fresh.mu.Lock()
	fresh.handBack(r.ctx, row)
	fresh.mu.Unlock()
	waitFor(t, "imported again", 60*time.Second, func() bool {
		b, _ := r.imp.Batch(r.ctx, first)
		again, _ := marked()
		return b.State == "done" && b.Counts["failed"] == 0 && b.Counts["pending"] == 0 && again == 0
	})
	var logs int
	r.d.QueryRow(`SELECT count(*) FROM sidecars WHERE kind = 'log'`).Scan(&logs)
	if logs != 1 {
		t.Fatalf("rip logs %d", logs)
	}
}

// The download settings (review #75): aria2 gets the speed limits and connections; two downloads
// fetch at once when allowed; Kanade keeps seeding by the settings, seeding again when aria2 lost
// the task (as after a restart), and ends it when the settings say not to seed.
func TestDownloadSettings(t *testing.T) {
	r := newRig(t, 10<<20)
	ctx := r.ctx
	slow := settings.Downloads{DownKiB: 48, Concurrent: 2, MaxPeers: 7, Seed: true}
	r.svc.SetPolicy(ctx, slow)
	var g map[string]string
	if err := r.svc.aria.RPC.Call(ctx, "getGlobalOption", &g); err != nil {
		t.Fatal(err)
	}
	if g["max-overall-download-limit"] != "49152" || g["bt-max-peers"] != "7" || g["max-concurrent-downloads"] != "6" {
		t.Fatalf("aria2 options %v %v %v", g["max-overall-download-limit"], g["bt-max-peers"], g["max-concurrent-downloads"])
	}
	// A second torrent of its own.
	content := filepath.Join(r.tmp, "web2")
	var names []string
	for _, f := range []string{"tone.flac", "tone-hires.flac"} {
		data, _ := os.ReadFile(filepath.Join("../media/testdata", f))
		os.MkdirAll(filepath.Join(content, "Box2"), 0o755)
		os.WriteFile(filepath.Join(content, "Box2", f), data, 0o644)
		names = append(names, f)
	}
	web := httptest.NewServer(http.FileServer(http.Dir(content)))
	defer web.Close()
	second := makeTorrent(t, content, "Box2", web.URL+"/", names)

	one := r.add(t, nil)
	two, err := r.svc.Add(ctx, "", second, false)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "file list", 20*time.Second, func() bool { v, _ := r.svc.Get(ctx, two); return v.State == StateSelecting })
	if err := r.svc.Select(ctx, two, []int{1, 2}); err != nil {
		t.Fatal(err)
	}
	state := func(id int64) string { v, _ := r.svc.Get(ctx, id); return v.State }
	waitFor(t, "both downloading", 20*time.Second, func() bool { return state(one) == StateDownloading && state(two) == StateDownloading })

	fast := slow
	fast.DownKiB = 0
	r.svc.SetPolicy(ctx, fast)
	waitFor(t, "both seeding", 60*time.Second, func() bool { return state(one) == StateSeeding && state(two) == StateSeeding })

	// aria2 loses the task (it keeps no finished task across a restart): seeding goes on.
	row, _ := r.svc.load(ctx, one)
	r.svc.aria.RPC.Call(ctx, "forceRemove", nil, row.gid)
	time.Sleep(500 * time.Millisecond)
	r.svc.aria.RPC.Call(ctx, "removeDownloadResult", nil, row.gid)
	waitFor(t, "seeding again", 30*time.Second, func() bool {
		now, _ := r.svc.load(ctx, one)
		return now.gid != "" && now.gid != row.gid && now.State == StateSeeding
	})

	off := fast
	off.Seed = false
	r.svc.SetPolicy(ctx, off)
	go r.imp.Run(ctx)
	for _, id := range []int64{one, two} {
		waitFor(t, "seeding ended and files removed", 60*time.Second, func() bool {
			v, _ := r.svc.Get(ctx, id)
			return v.State == StateCompleted && v.FilesRemoved
		})
	}
	if v, _ := r.svc.Get(ctx, one); v.Note != "not seeding (settings)" {
		t.Fatalf("note %q", v.Note)
	}
}

func TestSeedingOver(t *testing.T) {
	s := &Service{}
	now := db.Now()
	for _, c := range []struct {
		p        settings.Downloads
		up, done int64
		want     bool
	}{
		{settings.Downloads{Seed: true, SeedRatio: 1, SeedHours: 72}, 50, now - 3600_000, false},
		{settings.Downloads{Seed: true, SeedRatio: 1, SeedHours: 72}, 100, now - 3600_000, true},
		{settings.Downloads{Seed: true, SeedRatio: 1, SeedHours: 72}, 0, now - 73*3600_000, true},
		{settings.Downloads{Seed: true}, 1000, now - 1000*3600_000, false},
		{settings.Downloads{Seed: false, SeedRatio: 5}, 0, now, true},
	} {
		s.policy = c.p
		r := &row{View: View{TotalBytes: 100, UploadedBytes: c.up, CompletedAt: c.done}}
		if got := s.seedingOver(r) != ""; got != c.want {
			t.Fatalf("%+v up %d: %v", c.p, c.up, got)
		}
	}
}

// aria2 started again (a crash, a restart) starts with the settings: they replace the defaults in
// its config (review #75).
func TestConfigCarriesSettings(t *testing.T) {
	a, err := NewAria2("", t.TempDir(), t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	a.SetGlobal(context.Background(), aria2Options(settings.Downloads{UpKiB: 100, Concurrent: 3, MaxPeers: 12, Seed: true, SeedRatio: 2}))
	path, err := a.writeConfig(1234)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	got := map[string][]string{}
	for _, l := range strings.Split(string(raw), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			got[k] = append(got[k], v)
		}
	}
	for k, want := range map[string]string{"bt-max-peers": "12", "seed-ratio": "2.00", "seed-time": "5256000",
		"max-overall-upload-limit": "100K", "max-concurrent-downloads": "7", "rpc-listen-port": "1234"} {
		if len(got[k]) != 1 || got[k][0] != want {
			t.Fatalf("%s = %v, want %s", k, got[k], want)
		}
	}
}

// Seeding tasks do not count against aria2's download slots: five torrents seeding without end and
// one download at a time still let the next download run, also after aria2 starts again and the
// seeds are added back (review #84).
func TestSeedersCannotBlockNextDownload(t *testing.T) {
	r := newRig(t, 10<<20)
	ctx := r.ctx
	r.svc.SetPolicy(ctx, settings.Downloads{Concurrent: 1, MaxPeers: 30, Seed: true}) // seed without end
	content := filepath.Join(r.tmp, "many")
	web := httptest.NewServer(http.FileServer(http.Dir(content)))
	defer web.Close()
	torrent := func(i int) []byte {
		name := fmt.Sprintf("Seed%d", i)
		os.MkdirAll(filepath.Join(content, name), 0o755)
		os.WriteFile(filepath.Join(content, name, "notes.txt"), []byte(strings.Repeat(name, 4000)), 0o644)
		return makeTorrent(t, content, name, web.URL+"/", []string{"notes.txt"})
	}
	state := func(id int64) string { v, _ := r.svc.Get(ctx, id); return v.State }
	start := func(i int) int64 {
		id, err := r.svc.Add(ctx, "", torrent(i), false)
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, "file list", 20*time.Second, func() bool { return state(id) == StateSelecting })
		if err := r.svc.Select(ctx, id, []int{1}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	seeding := func(ids ...int64) func() bool {
		return func() bool {
			for _, id := range ids {
				row, _ := r.svc.load(ctx, id)
				var st struct {
					Status string `json:"status"`
					Seeder string `json:"seeder"`
				}
				if row.State != StateSeeding || r.svc.aria.RPC.Call(ctx, "tellStatus", &st, row.gid, []string{"status", "seeder"}) != nil ||
					st.Status != "active" || st.Seeder != "true" {
					return false
				}
			}
			return true
		}
	}
	var seeds []int64
	for i := 1; i <= 5; i++ {
		seeds = append(seeds, start(i))
		waitFor(t, fmt.Sprintf("seed %d seeding", i), 30*time.Second, seeding(seeds[len(seeds)-1]))
	}
	next := start(6)
	waitFor(t, "the next download beside five seeds", 30*time.Second, seeding(append(seeds, next)...))

	// aria2 starts again without the finished tasks: they come back as seeds, and still leave room.
	r.svc.aria.RPC.Call(ctx, "shutdown", nil)
	waitFor(t, "seeds back after aria2 restarted", 60*time.Second, func() bool {
		return r.svc.aria.Ready() && seeding(append(seeds, next)...)()
	})
	last := start(7)
	waitFor(t, "a download after the restart", 30*time.Second, seeding(last))
}
