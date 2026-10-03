package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/HHim8826/kanade/server/internal/library"
)

// The batch endpoints check every ID before changing anything, answer an edit group that undo takes
// back, and report each song a permanent deletion could not delete (review #83).
func TestBatchEndpoints(t *testing.T) {
	ctx := context.Background()
	s, h := newTestServer(t)
	tok := loginToken(t, s, h)
	song := func(sha, title, album string) library.PublishResult {
		a, _ := s.lib.CreateAsset(ctx, library.Asset{SHA256: sha, Size: 1, Format: "flac", Codec: "flac"})
		s.lib.MarkVerified(ctx, a.ID, "d-"+sha)
		r, err := s.lib.Publish(ctx, a.ID, library.EntryInput{Title: title, Artist: "x", Album: album, AlbumArtist: "y", TrackNo: 1})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	a, b := song("a", "A", "One"), song("b", "B", "Two")
	albumOf := func(entry int64) (id int64) {
		s.db.QueryRow(`SELECT album_id FROM album_entries WHERE id = ?`, entry).Scan(&id)
		return
	}
	A, B := albumOf(a.EntryID), albumOf(b.EntryID)
	post := func(path string, body any) (int, map[string]any) {
		t.Helper()
		rec := do(t, h, "POST", path, tok, body)
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	if code, _ := post("/api/v1/favorites/batch", map[string]any{"tracks": []int64{a.TrackID, 999}, "on": true}); code != http.StatusNotFound {
		t.Fatalf("unknown song: %d", code)
	}
	if code, _ := post("/api/v1/favorites/batch", map[string]any{"tracks": []int64{a.TrackID, b.TrackID}, "albums": []int64{A}, "on": true}); code != http.StatusNoContent {
		t.Fatalf("favorites: %d", code)
	}
	code, plan := post("/api/v1/albums/merge", map[string]any{"albums": []int64{A, B}, "title": "Both", "sections": true, "preview": true})
	if code != 200 || len(plan["moves"].([]any)) != 2 {
		t.Fatalf("preview: %d %v", code, plan)
	}
	code, res := post("/api/v1/albums/merge", map[string]any{"albums": []int64{A, B}, "title": "Both", "sections": true})
	if code != 200 || res["group"].(float64) == 0 {
		t.Fatalf("merge: %d %v", code, res)
	}
	merged := int64(res["album_id"].(float64))
	if rec := do(t, h, "GET", "/api/v1/albums/"+itoa(merged), tok, nil); !strings.Contains(rec.Body.String(), `"sections":{"1":"One","2":"Two"}`) {
		t.Fatalf("album: %s", rec.Body)
	}
	if code, _ := post("/api/v1/edits/"+itoa(int64(res["group"].(float64)))+"/undo", nil); code != 200 {
		t.Fatalf("undo: %d", code)
	}
	if code, _ := post("/api/v1/albums/edit", map[string]any{"albums": []int64{A, 404}, "date": "2020"}); code != http.StatusNotFound {
		t.Fatalf("edit with an unknown album: %d", code)
	}
	if code, res := post("/api/v1/tracks/place", map[string]any{"tracks": []int64{a.TrackID}, "album": B, "section": "Bonus"}); code != 200 || res["album_id"].(float64) != float64(B) {
		t.Fatalf("place: %d %v", code, res)
	}
	code, res = post("/api/v1/tracks/delete", map[string]any{"tracks": []int64{b.TrackID, 12345}})
	if code != 200 || len(res["deleted"].([]any)) != 1 || res["failed"].(map[string]any)["12345"] == nil {
		t.Fatalf("delete: %d %v", code, res)
	}
	code, res = post("/api/v1/albums/remove", map[string]any{"albums": []int64{A}, "delete_tracks": true})
	if code != 200 || len(res["deleted"].([]any)) != 0 { // A's song is still on B
		t.Fatalf("remove with songs: %d %v", code, res)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
