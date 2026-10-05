package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/library"
)

// fakeBangumi answers as Bangumi's API does, for subjects 531 and 1269 and a search; down makes it
// fail.
func fakeBangumi(t *testing.T, down *atomic.Bool, reads *atomic.Int32) *httptest.Server {
	subjects := map[string]string{
		"531": `{"id":531,"type":2,"name":"ARIA The ANIMATION","name_cn":"水星领航员","summary":"ネオ・ヴェネツィア","date":"2005-10-06",
			"platform":"TV","images":{"large":"https://lain.bgm.tv/pic/cover/l/531.jpg"},"rating":{"rank":300,"total":5000,"score":8.2}}`,
		"1269": `{"id":1269,"type":2,"name":"ARIA The NATURAL","name_cn":"","summary":"","date":null,"platform":"TV","images":null,
			"rating":{"rank":0,"total":0,"score":0}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.UserAgent(), "HHim8826/kanade/") {
			t.Errorf("User-Agent %q", r.UserAgent())
		}
		if down.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/v0/search/subjects":
			var body struct {
				Keyword string               `json:"keyword"`
				Filter  struct{ Type []int } `json:"filter"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.Keyword == "86" { // words that are also a number
				w.Write([]byte(`{"total":1,"data":[{"id":302189,"type":2,"name":"86―エイティシックス―","images":null}]}`))
				return
			}
			if len(body.Filter.Type) == 1 && body.Filter.Type[0] == 4 {
				w.Write([]byte(`{"total":0,"data":[]}`))
				return
			}
			w.Write([]byte(`{"total":2,"data":[` + subjects["531"] + `,` + subjects["1269"] + `]}`))
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v0/subjects/"):
			reads.Add(1)
			if s, ok := subjects[strings.TrimPrefix(r.URL.Path, "/v0/subjects/")]; ok {
				w.Write([]byte(s))
				return
			}
			http.Error(w, `{"title":"Not Found"}`, http.StatusNotFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The works endpoints (review #94): candidates by words, link or number, marked when kept and
// linked; linking keeps the work, and links what is kept while Bangumi is down; the work's page
// reads a stale description again in the background, and a failed refresh keeps what it had;
// songs' uses; unlinking and undo; only Bangumi's pictures are fetched.
func TestWorkEndpoints(t *testing.T) {
	ctx := context.Background()
	s, h := newTestServer(t)
	tok := loginToken(t, s, h)
	var down atomic.Bool
	var reads atomic.Int32
	srv := fakeBangumi(t, &down, &reads)
	s.bgm.Base, s.bgm.Gap = srv.URL, 0

	a, _ := s.lib.CreateAsset(ctx, library.Asset{SHA256: "w", Size: 1, Format: "flac", Codec: "flac"})
	s.lib.MarkVerified(ctx, a.ID, "d-w")
	pub, _ := s.lib.Publish(ctx, a.ID, library.EntryInput{Title: "Undine", Artist: "x", Album: "ARIA OST", AlbumArtist: "y"})
	var album, track int64
	s.db.QueryRow(`SELECT album_id, track_id FROM album_entries WHERE id = ?`, pub.EntryID).Scan(&album, &track)

	call := func(method, path string, body any, out any) int {
		t.Helper()
		rec := do(t, h, method, path, tok, body)
		if out != nil {
			json.Unmarshal(rec.Body.Bytes(), out)
		}
		return rec.Code
	}
	type page struct {
		Total    int         `json:"total"`
		Subjects []candidate `json:"subjects"`
	}
	var p page
	if code := call("GET", "/api/v1/bangumi/search?q=ARIA&types=2,4&album="+itoa(album), nil, &p); code != 200 || p.Total != 2 || len(p.Subjects) != 2 ||
		p.Subjects[0].Name != "ARIA The ANIMATION" || p.Subjects[0].Score != 8.2 || !p.Subjects[0].Image || p.Subjects[0].URL != "https://bgm.tv/subject/531" ||
		p.Subjects[1].Image || p.Subjects[1].Score != 0 || p.Subjects[0].Linked {
		t.Fatalf("search: %d %+v", code, p)
	}
	if code := call("GET", "/api/v1/bangumi/search?q=ARIA&types=5", nil, nil); code != http.StatusBadRequest {
		t.Fatalf("an unknown type: %d", code)
	}
	if call("GET", "/api/v1/bangumi/search?q=https://bangumi.tv/subject/1269", nil, &p); len(p.Subjects) != 1 || !p.Subjects[0].ByID || p.Subjects[0].SourceID != "1269" {
		t.Fatalf("by link: %+v", p)
	}
	if code := call("GET", "/api/v1/bangumi/search?q=https://bgm.tv/subject/42", nil, nil); code != http.StatusNotFound {
		t.Fatalf("a link to no subject: %d", code)
	}
	if call("GET", "/api/v1/bangumi/search?q=531", nil, &p); len(p.Subjects) != 2 || !p.Subjects[0].ByID || p.Subjects[1].ByID {
		t.Fatalf("by number, then the words, without it twice: %+v", p)
	}
	if call("GET", "/api/v1/bangumi/search?q=86", nil, &p); len(p.Subjects) != 1 || p.Subjects[0].SourceID != "302189" {
		t.Fatalf("a number that is no subject is words: %+v", p)
	}

	var linked struct {
		Group  int64 `json:"group"`
		WorkID int64 `json:"work_id"`
	}
	if code := call("POST", "/api/v1/albums/"+itoa(album)+"/works", map[string]any{"source_id": "https://bgm.tv/subject/531"}, &linked); code != 200 ||
		linked.Group == 0 || linked.WorkID == 0 {
		t.Fatalf("link: %d %+v", code, linked)
	}
	if call("GET", "/api/v1/bangumi/search?q=ARIA&album="+itoa(album), nil, &p); !p.Subjects[0].Linked || p.Subjects[0].WorkID != linked.WorkID {
		t.Fatalf("marked linked: %+v", p.Subjects[0])
	}
	var detail library.AlbumDetail
	if call("GET", "/api/v1/albums/"+itoa(album), nil, &detail); len(detail.Works) != 1 || detail.Works[0].NameCN != "水星领航员" {
		t.Fatalf("album: %+v", detail.Works)
	}

	// Bangumi down: searching says so; a kept work links, a new one cannot.
	down.Store(true)
	s.bgm.Forget(1269) // read for the search by link above: kept for a while
	if code := call("GET", "/api/v1/bangumi/search?q=ARIA+TV", nil, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("search while down: %d", code)
	}
	if code := call("POST", "/api/v1/albums/"+itoa(album)+"/works", map[string]any{"source_id": "1269"}, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("a new work while down: %d", code)
	}
	s.bgm.Forget(531)
	if code := call("DELETE", "/api/v1/albums/"+itoa(album)+"/works/"+itoa(linked.WorkID), nil, nil); code != 200 {
		t.Fatalf("unlink: %d", code)
	}
	if code := call("POST", "/api/v1/albums/"+itoa(album)+"/works", map[string]any{"source_id": "531"}, &linked); code != 200 || linked.Group == 0 {
		t.Fatalf("a kept work while down: %d %+v", code, linked)
	}
	if code := call("POST", "/api/v1/works/"+itoa(linked.WorkID)+"/refresh", nil, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("refresh while down: %d", code)
	}
	var wp struct {
		Work  library.Work        `json:"work"`
		Songs []library.WorkTrack `json:"songs"`
		URL   string              `json:"url"`
		Stale bool                `json:"stale"`
	}
	if code := call("GET", "/api/v1/works/"+itoa(linked.WorkID), nil, &wp); code != 200 || wp.Work.Name != "ARIA The ANIMATION" || wp.Work.Albums != 1 ||
		wp.URL != "https://bgm.tv/subject/531" || wp.Stale {
		t.Fatalf("work page while down: %d %+v", code, wp)
	}
	down.Store(false)

	// Songs' uses.
	if code := call("PUT", "/api/v1/tracks/"+itoa(track)+"/works", map[string]any{"works": []map[string]any{{"work_id": linked.WorkID, "use": "op"}}}, nil); code != 200 {
		t.Fatalf("use: %d", code)
	}
	if code := call("PUT", "/api/v1/tracks/"+itoa(track)+"/works", map[string]any{"works": []map[string]any{{"work_id": linked.WorkID, "use": "?"}}}, nil); code != http.StatusBadRequest {
		t.Fatalf("a use that is none: %d", code)
	}
	if call("GET", "/api/v1/works/"+itoa(linked.WorkID), nil, &wp); len(wp.Songs) != 1 || wp.Songs[0].Use != "op" || wp.Songs[0].Title != "Undine" {
		t.Fatalf("songs: %+v", wp.Songs)
	}
	var list []library.Work
	if call("GET", "/api/v1/works", nil, &list); len(list) != 1 || list[0].Tracks != 1 {
		t.Fatalf("works: %+v", list)
	}

	// A description read long ago is read again, without the page waiting.
	s.db.Exec(`UPDATE works SET fetched_at = ?, score = 1 WHERE id = ?`, time.Now().Add(-40*24*time.Hour).UnixMilli(), linked.WorkID)
	before := reads.Load()
	if call("GET", "/api/v1/works/"+itoa(linked.WorkID), nil, &wp); !wp.Stale || wp.Work.Score != 1 {
		t.Fatalf("stale: %+v", wp)
	}
	for i := 0; i < 100 && reads.Load() == before; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	for i := 0; i < 100; i++ {
		if call("GET", "/api/v1/works/"+itoa(linked.WorkID), nil, &wp); !wp.Stale {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if wp.Stale || wp.Work.Score != 8.2 {
		t.Fatalf("refreshed: %+v", wp)
	}

	// Unlinking takes the album's songs' uses; undo brings both.
	var un struct{ Group int64 }
	call("DELETE", "/api/v1/albums/"+itoa(album)+"/works/"+itoa(linked.WorkID), nil, &un)
	if call("GET", "/api/v1/works", nil, &list); len(list) != 0 {
		t.Fatalf("unlinked works listed: %+v", list)
	}
	if code := call("POST", "/api/v1/edits/"+itoa(un.Group)+"/undo", nil, nil); code != 200 {
		t.Fatalf("undo: %d", code)
	}
	if call("GET", "/api/v1/works/"+itoa(linked.WorkID), nil, &wp); wp.Work.Albums != 1 || len(wp.Songs) != 1 {
		t.Fatalf("undone: %+v", wp)
	}

	// Pictures only from Bangumi: this work's (kept from the fake server, which is not Bangumi's)
	// and a subject without one.
	s.db.Exec(`UPDATE works SET image = ? WHERE id = ?`, srv.URL+"/x.jpg", linked.WorkID)
	if code := call("GET", "/api/v1/works/"+itoa(linked.WorkID)+"/image?size=256", nil, nil); code != http.StatusNotFound {
		t.Fatalf("a picture elsewhere: %d", code)
	}
	if code := call("GET", "/api/v1/bangumi/subjects/1269/image", nil, nil); code != http.StatusNotFound {
		t.Fatalf("a subject without a picture: %d", code)
	}
	if code := call("GET", "/api/v1/works/999", nil, nil); code != http.StatusNotFound {
		t.Fatalf("no such work: %d", code)
	}
}
