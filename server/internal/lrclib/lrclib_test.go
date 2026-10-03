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
