package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/lrclib"
)

// Online lyrics: only the song's own title, artist and length go to LRCLIB; an automatic match is
// stored only where there are none, a chosen one replaces what is there.
func TestFindAndUseOnlineLyrics(t *testing.T) {
	ctx := context.Background()
	s, h := newTestServer(t)
	token := loginToken(t, s, h)
	var asked url.Values
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/search":
			asked = r.URL.Query()
			w.Write([]byte(`[{"id": 7, "trackName": "Song", "artistName": "Singer", "duration": 200.5, "syncedLyrics": "[00:01.00]la"},
				{"id": 8, "trackName": "Song (live)", "artistName": "Singer", "duration": 260, "plainLyrics": "live"},
				{"id": 9, "trackName": "Song", "artistName": "Singer", "duration": 200, "instrumental": true}]`))
		case "/api/get/7":
			w.Write([]byte(`{"id": 7, "trackName": "Song", "artistName": "Singer", "duration": 200.5, "syncedLyrics": "[00:01.00]la"}`))
		case "/api/get/8":
			w.Write([]byte(`{"id": 8, "plainLyrics": "live"}`))
		case "/api/get/9":
			w.Write([]byte(`{"id": 9, "instrumental": true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()
	s.lrclib = lrclib.New("https://music.example/")
	s.lrclib.Base = fake.URL + "/api"
	a, _ := s.lib.CreateAsset(ctx, library.Asset{SHA256: "ly", Size: 1, Format: "flac", Codec: "flac", DurationMS: 200_000})
	s.lib.MarkVerified(ctx, a.ID, "drive-ly")
	r, _ := s.lib.Publish(ctx, a.ID, library.EntryInput{Title: "Song", Artist: "Singer", Album: "Album", AlbumArtist: "Singer"})
	path := fmt.Sprintf("/api/v1/tracks/%d/lyrics/online", r.TrackID)

	rec := do(t, h, "GET", path, token, nil)
	var list []lrclib.Candidate
	json.Unmarshal(rec.Body.Bytes(), &list)
	if rec.Code != 200 || len(list) != 3 || list[0].ID != 7 || !list[0].Exact || list[1].Exact {
		t.Fatalf("candidates %d %s", rec.Code, rec.Body)
	}
	if asked.Get("track_name") != "Song" || asked.Get("artist_name") != "Singer" || len(asked) != 2 {
		t.Fatalf("sent %v", asked)
	}
	if rec := do(t, h, "POST", path, token, map[string]any{"id": 9}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("instrumental stored: %d", rec.Code)
	}
	if rec := do(t, h, "POST", path, token, map[string]any{"id": 7, "auto": true}); rec.Code != 200 || rec.Body.String() != `{"saved":true}`+"\n" {
		t.Fatalf("auto: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", path, token, map[string]any{"id": 8, "auto": true}); rec.Body.String() != `{"saved":false}`+"\n" {
		t.Fatalf("auto over existing lyrics: %s", rec.Body)
	}
	if rec := do(t, h, "POST", path, token, map[string]any{"id": 8}); rec.Body.String() != `{"saved":true}`+"\n" {
		t.Fatalf("chosen: %s", rec.Body)
	}
	if l, _ := s.lib.Lyrics(ctx, r.TrackID); l.Text != "live" || l.Source != library.LyricsLRCLIB {
		t.Fatalf("lyrics %+v", l)
	}
	if rec := do(t, h, "GET", path, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("without login: %d", rec.Code)
	}
}

// LRCLIB failing is not said with 502, which Cloudflare replaces with its own page (reading as
// Kanade being down): 503 with the message, and a wait to ask again when LRCLIB did not answer.
func TestOnlineLyricsWhenLRCLIBFails(t *testing.T) {
	ctx := context.Background()
	s, h := newTestServer(t)
	token := loginToken(t, s, h)
	answer := http.StatusInternalServerError
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if answer == http.StatusOK {
			w.Write([]byte(`{"not": "a list"}`))
			return
		}
		w.WriteHeader(answer)
	}))
	defer fake.Close()
	s.lrclib = lrclib.New("https://music.example/")
	s.lrclib.Base = fake.URL + "/api"
	a, _ := s.lib.CreateAsset(ctx, library.Asset{SHA256: "ly", Size: 1, Format: "flac", Codec: "flac", DurationMS: 200_000})
	s.lib.MarkVerified(ctx, a.ID, "drive-ly")
	r, _ := s.lib.Publish(ctx, a.ID, library.EntryInput{Title: "Song", Artist: "Singer", Album: "Album", AlbumArtist: "Singer"})
	path := fmt.Sprintf("/api/v1/tracks/%d/lyrics/online", r.TrackID)

	for _, c := range []struct {
		answer int
		retry  string
	}{{http.StatusInternalServerError, "30"}, {http.StatusTooManyRequests, "30"}, {http.StatusOK, ""}} {
		answer = c.answer
		rec := do(t, h, "GET", path, token, nil)
		var body struct{ Error string }
		if rec.Code != http.StatusServiceUnavailable || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Error == "" ||
			rec.Header().Get("Retry-After") != c.retry {
			t.Fatalf("LRCLIB answering %d: %d %q %s", c.answer, rec.Code, rec.Header().Get("Retry-After"), rec.Body)
		}
	}
}
