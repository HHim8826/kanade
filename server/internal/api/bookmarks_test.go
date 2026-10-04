package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/HHim8826/kanade/server/internal/library"
)

// The bookmark endpoints make, list, rename and delete named places in a song (review #98).
func TestBookmarkEndpoints(t *testing.T) {
	ctx := context.Background()
	s, h := newTestServer(t)
	tok := loginToken(t, s, h)
	a, _ := s.lib.CreateAsset(ctx, library.Asset{SHA256: "bm", Size: 1, Format: "flac", Codec: "flac", DurationMS: 600_000})
	s.lib.MarkVerified(ctx, a.ID, "d-bm")
	r, _ := s.lib.Publish(ctx, a.ID, library.EntryInput{Title: "Drama", Artist: "Cast", Album: "Box", AlbumArtist: "Cast"})
	var b library.Bookmark
	rec := do(t, h, "POST", "/api/v1/bookmarks", tok, map[string]any{"asset_id": a.ID, "position_ms": 125_000, "name": "下次從這裡"})
	json.Unmarshal(rec.Body.Bytes(), &b)
	if rec.Code != http.StatusCreated || b.ID == 0 || b.Asset == nil {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/api/v1/bookmarks", tok, map[string]any{"asset_id": a.ID, "position_ms": 999_000, "name": "x"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("outside the song: %d", rec.Code)
	}
	if rec := do(t, h, "PATCH", "/api/v1/bookmarks/"+itoa(b.ID), tok, map[string]any{"name": "結局前", "note": "記得"}); rec.Code != http.StatusNoContent {
		t.Fatalf("rename: %d", rec.Code)
	}
	var list []library.Bookmark
	json.Unmarshal(do(t, h, "GET", "/api/v1/bookmarks?track="+itoa(r.TrackID), tok, nil).Body.Bytes(), &list)
	if len(list) != 1 || list[0].Name != "結局前" || list[0].Note != "記得" || list[0].PositionMS != 125_000 {
		t.Fatalf("list: %+v", list)
	}
	if rec := do(t, h, "DELETE", "/api/v1/bookmarks/"+itoa(b.ID), tok, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := do(t, h, "DELETE", "/api/v1/bookmarks/"+itoa(b.ID), tok, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("delete again: %d", rec.Code)
	}
}
