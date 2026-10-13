package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/config"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/storage"
	"github.com/HHim8826/kanade/server/internal/storage/storagetest"
	"github.com/HHim8826/kanade/server/internal/storesync"
	"github.com/HHim8826/kanade/server/internal/stream"
)

// storageServer is a server whose library files may be in folders (#99), read where they are.
func storageServer(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	s, h := newTestServer(t, func(d *Deps) {
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		set := &storage.Set{DB: d.DB, Log: log, DataDir: d.Config.DataDir}
		set.Forget = d.Importer.ForgetSource
		cache, err := stream.NewCache(set, d.Config.Path(config.DirCache), 64<<20, log)
		if err != nil {
			t.Fatal(err)
		}
		cache.Explain = storage.Explain
		d.Files, d.Cache = set, cache
		d.Importer.Stores = set
		d.StoreSync = &storesync.Syncer{Set: set, Lib: d.Library, Imp: d.Importer, Log: log}
	})
	return s, h, loginToken(t, s, h)
}

func decode[T any](t *testing.T, body io.Reader) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

type storageView struct {
	Default string      `json:"default"`
	Stores  []storeInfo `json:"stores"`
}

// Folders as storage locations: added and checked, the default for imports, songs there streamed
// where they are (ranges, a folder not there said as such), and kept from being removed while the
// library has files there.
func TestStorageFolders(t *testing.T) {
	s, h, token := storageServer(t)
	ctx := context.Background()
	root := t.TempDir()
	add := func(root, mode string) *httptest.ResponseRecorder {
		cfg, _ := json.Marshal(map[string]string{"root": root})
		return do(t, h, "POST", "/api/v1/storage", token, map[string]any{"kind": "local", "name": "Library", "mode": mode, "config": json.RawMessage(cfg)})
	}
	os.MkdirAll(filepath.Join(root, "inner"), 0o755)
	rec := add(root, "managed")
	if rec.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	if st := decode[storeInfo](t, rec.Body); st.State != storage.StateOK || st.Location != root {
		t.Fatalf("added %+v", st)
	}
	if rec := add(filepath.Join(root, "inner"), "source"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "overlaps") {
		t.Fatalf("an overlapping folder: %d %s", rec.Code, rec.Body)
	}
	if rec := add(s.cfg.Path(config.DirStaging), "source"); rec.Code != http.StatusBadRequest {
		t.Fatalf("the staging folder: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "PUT", "/api/v1/storage/default", token, map[string]any{"id": 1}); rec.Code != http.StatusNoContent {
		t.Fatalf("default: %d %s", rec.Code, rec.Body)
	}
	if v := decode[storageView](t, do(t, h, "GET", "/api/v1/storage", token, nil).Body); v.Default != "1" || len(v.Stores) != 1 {
		t.Fatalf("storage %+v", v)
	}

	data := "fLaC" + strings.Repeat("0123456789", 100)
	os.MkdirAll(filepath.Join(root, "library/A"), 0o755)
	os.WriteFile(filepath.Join(root, "library/A/01.flac"), []byte(data), 0o644)
	a, err := s.lib.CreateAsset(ctx, library.Asset{SHA256: "s1", Size: int64(len(data)), Format: "flac", Codec: "flac"})
	if err != nil {
		t.Fatal(err)
	}
	s.lib.MarkVerified(ctx, a.ID, "local:1:library/A/01.flac")
	get := func(rng string) (int, http.Header, string) {
		req, _ := http.NewRequest("GET", fmt.Sprintf("/api/v1/stream/%d", a.ID), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Header(), rec.Body.String()
	}
	if code, hd, body := get("bytes=4-13"); code != http.StatusPartialContent || body != "0123456789" ||
		hd.Get("Content-Range") != fmt.Sprintf("bytes 4-13/%d", len(data)) {
		t.Fatalf("range: %d %v %q", code, hd, body)
	}
	if code, _, body := get(""); code != 200 || body != data {
		t.Fatalf("whole: %d %d bytes", code, len(body))
	}
	if entries, _ := os.ReadDir(s.cfg.Path(config.DirCache)); len(entries) != 0 {
		t.Fatalf("copied into the cache: %v", entries)
	}
	away := root + ".away"
	os.Rename(root, away)
	os.Mkdir(root, 0o755)
	if code, hd, body := get(""); code != http.StatusServiceUnavailable || !strings.Contains(body, `"reason":"offline"`) || hd.Get("Retry-After") == "" {
		t.Fatalf("not mounted: %d %s", code, body)
	}
	os.Remove(root)
	os.Rename(away, root)
	os.WriteFile(filepath.Join(root, "library/A/01.flac"), []byte("other"), 0o644)
	if code, _, body := get(""); code != http.StatusServiceUnavailable || !strings.Contains(body, `"reason":"gone"`) {
		t.Fatalf("other bytes there: %d %s", code, body)
	}
	if rec := do(t, h, "DELETE", "/api/v1/storage/1", token, nil); rec.Code != http.StatusConflict {
		t.Fatalf("removing a location in use: %d %s", rec.Code, rec.Body)
	}
}

// A source: scanned in the background, its songs imported where they are; removed, they leave the
// library and its files stay.
func TestStorageSource(t *testing.T) {
	s, h, token := storageServer(t)
	src := t.TempDir()
	data, err := os.ReadFile("../media/testdata/tone-notag.mp3")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(src, "Album"), 0o755)
	os.WriteFile(filepath.Join(src, "Album", "01 Song.mp3"), data, 0o644)
	cfg, _ := json.Marshal(map[string]string{"root": src})
	if rec := do(t, h, "POST", "/api/v1/storage", token, map[string]any{"kind": "local", "name": "NAS", "mode": "source", "config": json.RawMessage(cfg)}); rec.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "PUT", "/api/v1/storage/default", token, map[string]any{"id": "1"}); rec.Code != http.StatusConflict {
		t.Fatalf("a source as the default: %d %s", rec.Code, rec.Body)
	}
	cat := decode[struct{ ID int64 }](t, do(t, h, "POST", "/api/v1/categories", token, map[string]string{"name": "NAS"}).Body)
	if rec := do(t, h, "PATCH", "/api/v1/storage/1", token, map[string]any{"category": cat.ID + 1}); rec.Code != http.StatusBadRequest {
		t.Fatalf("a category that is not there: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "PATCH", "/api/v1/storage/1", token, map[string]any{"category": cat.ID}); rec.Code >= 300 {
		t.Fatalf("category: %d %s", rec.Code, rec.Body)
	}
	go s.importer.Run(t.Context())
	if rec := do(t, h, "POST", "/api/v1/storage/1/scan", token, nil); rec.Code != http.StatusAccepted {
		t.Fatalf("scan: %d %s", rec.Code, rec.Body)
	}
	var st storeInfo
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		v := decode[storageView](t, do(t, h, "GET", "/api/v1/storage", token, nil).Body)
		if st = v.Stores[0]; !st.Running && st.Use.Songs == 1 {
			break
		}
	}
	var scan struct{ Files, New int }
	json.Unmarshal(st.Scan, &scan)
	if st.Use.Songs != 1 || scan.Files != 1 || scan.New != 1 || st.Category != cat.ID {
		t.Fatalf("scanned: %+v %s", st, st.Scan)
	}
	// The album is in the source's category, and listed by where its files are.
	albums := func(query string) string {
		rec := do(t, h, "GET", "/api/v1/albums?"+query, token, nil)
		if rec.Code != 200 {
			return fmt.Sprint(rec.Code)
		}
		return fmt.Sprint(len(decode[[]library.AlbumSummary](t, rec.Body)))
	}
	for query, want := range map[string]string{"": "1", "place=1": "1", "place=drive": "0", "category=" + fmt.Sprint(cat.ID): "1",
		"place=2": "400", "place=local:1": "400"} {
		if got := albums(query); got != want {
			t.Errorf("albums?%s: %s, want %s", query, got, want)
		}
	}
	rec := do(t, h, "DELETE", "/api/v1/storage/1", token, nil)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"songs":1}` {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
	var tracks int
	s.db.QueryRow(`SELECT count(*) FROM tracks`).Scan(&tracks)
	if tracks != 0 {
		t.Fatalf("%d songs left", tracks)
	}
	if got, err := os.ReadFile(filepath.Join(src, "Album", "01 Song.mp3")); err != nil || string(got) != string(data) {
		t.Fatal("the source's file changed:", err)
	}
}

// A OneDrive: added, signed in through the callback or with the address pasted (only with a state
// made for it), its folders listed, one made (managed) and chosen; the folder kept while the
// library has files in it; a new client secret tried first.
func TestStorageOneDrive(t *testing.T) {
	s, h, token := storageServer(t)
	ctx := context.Background()
	g := storagetest.NewGraph(t)
	t.Cleanup(storage.UseMicrosoft(g.Login(), g.API()))
	g.Add("Music/a.flac", []byte("fLaC"))
	add := func(mode string) *httptest.ResponseRecorder {
		return do(t, h, "POST", "/api/v1/storage", token, map[string]any{"kind": "onedrive", "name": "OD " + mode, "mode": mode,
			"config": map[string]string{"client_id": g.ClientID}, "secret": map[string]string{"client_secret": g.ClientSecret}})
	}
	if rec := do(t, h, "POST", "/api/v1/storage", token, map[string]any{"kind": "onedrive", "mode": "source", "config": map[string]string{"client_id": "x"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("without a client secret: %d %s", rec.Code, rec.Body)
	}
	rec := add("source")
	if rec.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	if st := decode[storeInfo](t, rec.Body); st.SignedIn || st.Location != "OneDrive" || strings.Contains(string(st.Config), g.ClientSecret) {
		t.Fatalf("added %+v", st)
	}
	var v struct {
		Redirect string `json:"onedrive_redirect"`
		Paste    bool   `json:"onedrive_paste"`
	}
	json.NewDecoder(do(t, h, "GET", "/api/v1/storage", token, nil).Body).Decode(&v)
	if v.Redirect != "https://music.example/oauth/onedrive/callback" || v.Paste {
		t.Fatalf("redirect %+v", v)
	}
	if rec := do(t, h, "GET", "/api/v1/storage/1/folders", token, nil); rec.Code != http.StatusConflict {
		t.Fatalf("folders before signing in: %d %s", rec.Code, rec.Body)
	}
	connect := func(id int) (string, url.Values) {
		rec := do(t, h, "POST", fmt.Sprintf("/api/v1/storage/%d/connect", id), token, nil)
		var c struct {
			URL      string `json:"url"`
			Redirect string `json:"redirect_uri"`
			Paste    bool   `json:"paste"`
		}
		json.NewDecoder(rec.Body).Decode(&c)
		u, err := url.Parse(c.URL)
		if rec.Code != 200 || err != nil || c.Redirect != v.Redirect || c.Paste || u.Query().Get("redirect_uri") != v.Redirect {
			t.Fatalf("connect: %d %+v", rec.Code, c)
		}
		return u.Query().Get("state"), u.Query()
	}
	callback := func(q string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/oauth/onedrive/callback?"+q, nil))
		return rec
	}
	state, q := connect(1)
	if q.Get("scope") != "offline_access Files.Read" {
		t.Fatalf("a source asks for %q", q.Get("scope"))
	}
	if rec := callback("state=onedrive.1.forged&code=" + g.Code); rec.Code != http.StatusForbidden {
		t.Fatalf("a forged state: %d", rec.Code)
	}
	if rec := callback(url.Values{"state": {state}, "error": {"access_denied"}, "error_description": {"The user declined."}}.Encode()); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "access_denied") {
		t.Fatalf("declined: %d %s", rec.Code, rec.Body)
	}
	state, _ = connect(1)
	if rec := callback(url.Values{"state": {state}, "code": {g.Code}}.Encode()); rec.Code != 200 || !strings.Contains(rec.Body.String(), "已連線 OneDrive") {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body)
	}
	st := decode[storageView](t, do(t, h, "GET", "/api/v1/storage", token, nil).Body).Stores[0]
	if !st.SignedIn || !strings.Contains(string(st.Config), `"account":"Ann"`) || strings.Contains(string(st.Config), "refresh") {
		t.Fatalf("signed in %+v", st)
	}
	rec = do(t, h, "GET", "/api/v1/storage/1/folders", token, nil)
	var fl struct {
		Here    storage.Folder   `json:"here"`
		Folders []storage.Folder `json:"folders"`
	}
	json.NewDecoder(rec.Body).Decode(&fl)
	if rec.Code != 200 || fl.Here.Path != "/" || len(fl.Folders) != 1 || fl.Folders[0].Name != "Music" {
		t.Fatalf("folders: %d %+v", rec.Code, fl)
	}
	if rec := do(t, h, "POST", "/api/v1/storage/1/folders", token, map[string]string{"name": "New"}); rec.Code != http.StatusConflict {
		t.Fatalf("a folder made for a source: %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, "PATCH", "/api/v1/storage/1", token, map[string]string{"folder": fl.Folders[0].ID})
	if st := decode[storeInfo](t, rec.Body); rec.Code != 200 || st.Location != "OneDrive /Music" || st.State != storage.StateOK {
		t.Fatalf("folder chosen: %d %+v", rec.Code, st)
	}

	// Managed: signed in with the address pasted; a folder made and chosen; kept while in use.
	if rec := add("managed"); rec.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	g.Code = "the-second-code"
	state, q = connect(2)
	if q.Get("scope") != "offline_access Files.ReadWrite" {
		t.Fatalf("a managed store asks for %q", q.Get("scope"))
	}
	pasted := func(id int, state string) *httptest.ResponseRecorder {
		return do(t, h, "POST", fmt.Sprintf("/api/v1/storage/%d/connect", id), token,
			map[string]string{"url": " http://localhost/oauth/onedrive/callback?code=" + g.Code + "&state=" + url.QueryEscape(state) + "\n"})
	}
	if rec := pasted(1, state); rec.Code != http.StatusBadRequest {
		t.Fatalf("another store's sign-in: %d %s", rec.Code, rec.Body)
	}
	if rec := pasted(2, state); rec.Code != 200 || !decode[storeInfo](t, rec.Body).SignedIn {
		t.Fatalf("pasted: %d %s", rec.Code, rec.Body)
	}
	if rec := pasted(2, state); rec.Code != http.StatusBadRequest {
		t.Fatalf("pasted twice: %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, "POST", "/api/v1/storage/2/folders", token, map[string]string{"name": "Kanade"})
	f := decode[storage.Folder](t, rec.Body)
	if rec.Code != http.StatusCreated || f.Path != "/Kanade" {
		t.Fatalf("made: %d %+v", rec.Code, f)
	}
	if rec := do(t, h, "PATCH", "/api/v1/storage/2", token, map[string]string{"folder": fl.Here.ID}); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "overlaps") {
		t.Fatalf("the drive's root, around the source's folder: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "PATCH", "/api/v1/storage/2", token, map[string]string{"folder": f.ID}); rec.Code != 200 {
		t.Fatalf("folder chosen: %d %s", rec.Code, rec.Body)
	}
	a, _ := s.lib.CreateAsset(ctx, library.Asset{SHA256: "s1", Size: 4, Format: "flac", Codec: "flac"})
	s.lib.MarkVerified(ctx, a.ID, "onedrive:2:ITEM!9")
	if rec := do(t, h, "PATCH", "/api/v1/storage/2", token, map[string]string{"folder": fl.Folders[0].ID}); rec.Code != http.StatusConflict {
		t.Fatalf("another folder while in use: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "PATCH", "/api/v1/storage/2", token, map[string]any{"secret": map[string]string{"client_secret": "wrong"}}); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "invalid_client") {
		t.Fatalf("a wrong client secret: %d %s", rec.Code, rec.Body)
	}
}

// Deleting a song answers once the database has it, whatever the storage location's trash does:
// a bucket that does not answer holds up neither the request beyond trashWait nor imports, and the
// file reaches the trash once it answers (review #225).
func TestDeleteDoesNotWaitForASlowTrash(t *testing.T) {
	defer func(w time.Duration) { trashWait = w }(trashWait)
	trashWait = 200 * time.Millisecond
	s, h, token := storageServer(t)
	ctx := context.Background()
	fake := storagetest.NewS3(t)
	held := make(chan struct{})
	fake.Fail = func(r *http.Request) int { // the copy to .kanade-trash waits
		if r.Header.Get("X-Amz-Copy-Source") != "" {
			select {
			case <-held:
			case <-r.Context().Done():
			}
		}
		return 0
	}
	rec := do(t, h, "POST", "/api/v1/storage", token, map[string]any{"kind": "s3", "name": "R2", "mode": "managed",
		"config": map[string]string{"endpoint": fake.URL, "bucket": fake.Bucket}, "secret": map[string]string{"access_key_id": fake.AccessKey, "secret_access_key": fake.SecretKey}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	st := decode[storeInfo](t, rec.Body)
	song := func(sha, key string) int64 {
		fake.Put(key, []byte(sha))
		a, _ := s.lib.CreateAsset(ctx, library.Asset{SHA256: sha, Size: int64(len(sha)), Format: "flac", Codec: "flac"})
		if err := s.lib.MarkVerified(ctx, a.ID, storage.Ref("s3", st.ID, key)); err != nil {
			t.Fatal(err)
		}
		res, err := s.lib.Publish(ctx, a.ID, library.EntryInput{Title: "Song " + sha})
		if err != nil {
			t.Fatal(err)
		}
		return res.TrackID
	}
	gone := song("aaaa", "library/A/01.flac")
	start := time.Now()
	rec = do(t, h, "DELETE", fmt.Sprintf("/api/v1/tracks/%d", gone), token, nil)
	if took := time.Since(start); rec.Code != 200 || took > 2*time.Second {
		t.Fatalf("delete: %d %s after %v", rec.Code, rec.Body, took)
	}
	// Imported meanwhile: taken up at once.
	done := make(chan int64)
	go func() { done <- song("bbbb", "library/B/01.flac") }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("an import waited for another file's move to the trash")
	}
	close(held)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if n, _, _ := s.lib.TrashPending(ctx); n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the file never reached the trash")
		}
	}
	for _, name := range fake.Names() {
		if name == "library/A/01.flac" {
			t.Fatalf("still in place: %v", fake.Names())
		}
	}
}
