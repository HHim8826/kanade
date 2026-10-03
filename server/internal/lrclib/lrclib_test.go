package lrclib

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
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
		if calls.Load() != before+2 {
			t.Fatalf("%q: asked %d times after recovery", bad, calls.Load()-before)
		}
		// The good answers, the empty list too, are cached.
		c.Find(ctx, Song{Title: "Undine"})
		c.Get(ctx, 7)
		if calls.Load() != before+2 {
			t.Fatalf("%q: good answers not cached", bad)
		}
		srv.Close()
	}
}
