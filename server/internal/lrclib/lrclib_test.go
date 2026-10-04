package lrclib

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFindRanksAndGets(t *testing.T) {
	var searches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("no User-Agent")
		}
		switch r.URL.Path {
		case "/api/search":
			searches.Add(1)
			if r.URL.Query().Get("artist_name") != "" { // nothing with the artist: asked again by title
				w.Write([]byte(`[]`))
				return
			}
			w.Write([]byte(`[
				{"id": 1, "trackName": "Undine", "artistName": "Other", "albumName": "X", "duration": 300, "plainLyrics": "a"},
				{"id": 2, "trackName": "ＵＮＤＩＮＥ", "artistName": "牧野由依", "albumName": "ARIA", "duration": 251.4, "plainLyrics": "p", "syncedLyrics": "[00:01.00]海へ\n[00:05.00]二行目"},
				{"id": 3, "trackName": "Undine", "artistName": "牧野由依", "albumName": "ARIA", "duration": 251, "plainLyrics": "only plain"},
				{"id": 4, "trackName": "Undine", "artistName": "牧野由依", "duration": 251, "plainLyrics": "", "syncedLyrics": ""}
			]`))
		case "/api/get/2":
			w.Write([]byte(`{"id": 2, "trackName": "Undine", "artistName": "牧野由依", "duration": 251.4, "syncedLyrics": "[00:01.00]海へ"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New("https://music.example/")
	c.Base = srv.URL + "/api"
	ctx := context.Background()
	list, err := c.Find(ctx, Song{Title: "Undine", Artist: "牧野由依", DurationMS: 250_500})
	if err != nil || searches.Load() != 2 {
		t.Fatalf("find: %v, %d searches", err, searches.Load())
	}
	if len(list) != 3 || list[0].ID != 2 || !list[0].Exact || !list[0].Synced || list[0].Preview != "海へ\n二行目" ||
		list[1].ID != 3 || !list[1].Exact || list[2].ID != 1 || list[2].Exact {
		t.Fatalf("ranking %+v", list)
	}
	l, err := c.Get(ctx, 2)
	if err != nil || l.Text() != "[00:01.00]海へ" {
		t.Fatalf("get %+v %v", l, err)
	}
	if _, err := c.Get(ctx, 9); err != ErrNotFound {
		t.Fatalf("missing: %v", err)
	}
	// Cached: the same search again asks nothing.
	c.Find(ctx, Song{Title: "Undine", Artist: "牧野由依"})
	if searches.Load() != 2 {
		t.Fatalf("not cached: %d", searches.Load())
	}
}

// An answer that is not what was asked for (an error page sent with 200, broken JSON, another shape)
// is not cached: once LRCLIB is back, the same search asks again (review #80).
func TestBadAnswersAreNotCached(t *testing.T) {
	ctx := context.Background()
	for _, bad := range []string{
		`<!DOCTYPE html><html><title>502 Bad Gateway</title></html>`,
		`[{"id": 1, "trackName": "Undine"`,
		`{"message": "busy"}`,
		`null`,
		`[{"trackName": "no id"}]`,
	} {
		var calls atomic.Int32
		var good atomic.Bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			switch {
			case !good.Load():
				w.Write([]byte(bad))
			case r.URL.Path == "/api/get/7":
				w.Write([]byte(`{"id": 7, "trackName": "Undine", "plainLyrics": "a"}`))
			default:
				w.Write([]byte(`[]`))
			}
		}))
		c := New("https://music.example/")
		c.Base = srv.URL + "/api"
		if _, err := c.Find(ctx, Song{Title: "Undine"}); err == nil {
			t.Fatalf("%q: no error", bad)
		}
		if _, err := c.Get(ctx, 7); err == nil {
			t.Fatalf("%q: get: no error", bad)
		}
		good.Store(true)
		before := calls.Load()
		if list, err := c.Find(ctx, Song{Title: "Undine"}); err != nil || len(list) != 0 {
			t.Fatalf("%q: after recovery: %v %v", bad, list, err)
		}
		if l, err := c.Get(ctx, 7); err != nil || l.Plain != "a" {
			t.Fatalf("%q: get after recovery: %v %v", bad, l, err)
		}
		asks := int32(len(queries(Song{Title: "Undine"}))) + 1 // Find's searches, then the get
		if calls.Load() != before+asks {
			t.Fatalf("%q: asked %d times after recovery", bad, calls.Load()-before)
		}
		// The good answers, the empty list too, are cached.
		c.Find(ctx, Song{Title: "Undine"})
		c.Get(ctx, 7)
		if calls.Load() != before+asks {
			t.Fatalf("%q: good answers not cached", bad)
		}
		srv.Close()
	}
}

// LRCLIB answers about one uncached search in four with 503 "ServerOverloaded" and Retry-After: 1:
// such a busy answer is asked again after the wait, a few times; a long wait or another error is not.
func TestBusyIsAskedAgain(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name       string
		status     int
		retryAfter string
		busy       int32 // busy answers before a good one
		ok         bool
		calls      int32
	}{
		{"busy twice", http.StatusServiceUnavailable, "1", 2, true, 3},
		{"429 without a wait", http.StatusTooManyRequests, "", 1, true, 2},
		{"busy throughout", http.StatusServiceUnavailable, "1", 100, false, 1 + busyTries},
		{"a long wait", http.StatusServiceUnavailable, "120", 1, false, 1},
		{"a server error", http.StatusInternalServerError, "", 1, false, 1},
	} {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) <= c.busy {
				if c.retryAfter != "" {
					w.Header().Set("Retry-After", c.retryAfter)
				}
				w.WriteHeader(c.status)
				w.Write([]byte(`{"message":"The server is busy, please retry in a moment","name":"ServerOverloaded","statusCode":503}`))
				return
			}
			w.Write([]byte(`[{"id": 1, "trackName": "Undine", "plainLyrics": "a"}]`))
		}))
		cl := New("https://music.example/")
		cl.Base = srv.URL + "/api"
		cl.second = time.Millisecond
		list, err := cl.Find(ctx, Song{Title: "Undine"})
		if c.ok && (err != nil || len(list) != 1) || !c.ok && !errors.Is(err, ErrUnavailable) || calls.Load() != c.calls {
			t.Errorf("%s: %v %v after %d calls", c.name, list, err, calls.Load())
		}
		srv.Close()
	}
}

// fakeSearch answers searches from a table of query strings, [] for any other, and records them.
func fakeSearch(t *testing.T, answers map[string]string) (*Client, *[]string) {
	t.Helper()
	var asked []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.RawQuery)
		mu.Unlock()
		if a, ok := answers[r.URL.Query().Encode()]; ok {
			w.Write([]byte(a))
			return
		}
		w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)
	c := New("https://music.example/")
	c.Base = srv.URL + "/api"
	return c, &asked
}

func enc(kv ...string) string {
	q := url.Values{}
	for i := 0; i < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	return q.Encode()
}

// A title that carries its singer ("曲名 / 歌手") is also looked for as the title and singer, and by
// keywords; what is found with the same title, singer and length is exact (review #122).
func TestDecoratedTitleCanFindLyrics(t *testing.T) {
	ctx := context.Background()
	rec := `[{"id": 31, "trackName": "ユーフォリア", "artistName": "牧野由依", "duration": 250, "syncedLyrics": "[00:01.00]words"}]`
	for _, tc := range []struct {
		name    string
		answers map[string]string
		want    int // requests
	}{
		{"by title and singer", map[string]string{enc("track_name", "ユーフォリア", "artist_name", "牧野由依"): rec}, 3},
		{"by keywords", map[string]string{enc("q", "ユーフォリア 牧野由依"): rec}, 5},
	} {
		c, asked := fakeSearch(t, tc.answers)
		list, err := c.Find(ctx, Song{Title: "ユーフォリア / 牧野由依", Artist: "Makino Yui", DurationMS: 250_000})
		if err != nil || len(list) != 1 || list[0].ID != 31 || !list[0].Exact {
			t.Fatalf("%s: %+v %v", tc.name, list, err)
		}
		if len(*asked) != tc.want {
			t.Fatalf("%s: asked %v", tc.name, *asked)
		}
	}
	// A slash without spaces is part of the title.
	if _, _, ok := splitSinger("Fate/stay night"); ok {
		t.Fatal("split Fate/stay night")
	}
	if n, s, ok := splitSinger("髪とヘアピンと私／斎藤千和"); !ok || n != "髪とヘアピンと私" || s != "斎藤千和" {
		t.Fatalf("full-width slash: %q %q %v", n, s, ok)
	}
}

// A record without words found by a narrow search does not end the search, and lyrics of only
// space are neither a candidate nor exact (review #123).
func TestNoUsableLyricsStillFallsBack(t *testing.T) {
	ctx := context.Background()
	good := `{"id": 202, "trackName": "Undine", "artistName": "牧野由依", "duration": 200, "syncedLyrics": "[00:01.00]la"}`
	for _, empty := range []string{`"plainLyrics": null`, `"plainLyrics": ""`, `"plainLyrics": " \n\t "`} {
		narrow := `[{"id": 201, "trackName": "Undine", "artistName": "Makino Yui", "duration": 200, ` + empty + `}]`
		c, asked := fakeSearch(t, map[string]string{
			enc("track_name", "Undine", "artist_name", "Makino Yui"): narrow,
			enc("track_name", "Undine"):                              "[" + good + "]",
		})
		list, err := c.Find(ctx, Song{Title: "Undine", Artist: "Makino Yui", DurationMS: 200_000})
		if err != nil || len(list) != 1 || list[0].ID != 202 {
			t.Fatalf("%s: %+v %v (asked %v)", empty, list, err, *asked)
		}
		if len(*asked) != 2 {
			t.Fatalf("%s: asked %v", empty, *asked)
		}
	}
	// An instrumental record stays one, never exact.
	c, _ := fakeSearch(t, map[string]string{enc("track_name", "Undine"): `[{"id": 9, "trackName": "Undine", "duration": 200, "instrumental": true}]`})
	if list, _ := c.Find(ctx, Song{Title: "Undine", DurationMS: 200_000}); len(list) != 1 || !list[0].Instrumental || list[0].Exact {
		t.Fatalf("instrumental: %+v", list)
	}
}
