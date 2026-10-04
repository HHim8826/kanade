package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/HHim8826/kanade/server/internal/library"
)

// Smart playlists through the API: made from rules, previewed, changed, played on (review #96).
func TestSmartPlaylistEndpoints(t *testing.T) {
	ctx := context.Background()
	s, h := newTestServer(t)
	tok := loginToken(t, s, h)
	var ids []int64
	for _, sha := range []string{"s1", "s2", "s3"} {
		a, _ := s.lib.CreateAsset(ctx, library.Asset{SHA256: sha, Size: 1, Format: "flac", Codec: "flac", DurationMS: 100_000})
		s.lib.MarkVerified(ctx, a.ID, "d-"+sha)
		r, _ := s.lib.Publish(ctx, a.ID, library.EntryInput{Title: "Song " + sha, Artist: "Singer", Album: "Album", AlbumArtist: "Singer"})
		ids = append(ids, r.TrackID)
	}
	s.lib.SetFavorite(ctx, "track", ids[0], true)
	rules := map[string]any{"match": "all", "sort": "recent_added", "conditions": []map[string]any{{"field": "favorite", "op": "not"}}}
	var prev struct {
		Matches int                 `json:"matches"`
		Count   int                 `json:"count"`
		Tracks  []library.TrackItem `json:"tracks"`
	}
	rec := do(t, h, "POST", "/api/v1/playlists/preview", tok, map[string]any{"rules": rules})
	json.Unmarshal(rec.Body.Bytes(), &prev)
	if rec.Code != 200 || prev.Matches != 2 || prev.Count != 2 {
		t.Fatalf("preview %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/api/v1/playlists/preview", tok, map[string]any{"rules": map[string]any{"sort": "loudest"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad rules: %d", rec.Code)
	}
	var p library.PlaylistDetail
	rec = do(t, h, "POST", "/api/v1/playlists", tok, map[string]any{"name": "沒收藏的", "rules": rules})
	json.Unmarshal(rec.Body.Bytes(), &p)
	if rec.Code != http.StatusCreated || !p.Smart || len(p.Items) != 2 {
		t.Fatalf("create %d %s", rec.Code, rec.Body)
	}
	var next []library.TrackItem
	json.Unmarshal(do(t, h, "GET", "/api/v1/playlists/"+itoa(p.ID)+"/next?n=5&not="+itoa(ids[2]), tok, nil).Body.Bytes(), &next)
	if len(next) != 1 || next[0].ID != ids[1] {
		t.Fatalf("next %+v", next)
	}
	rec = do(t, h, "PUT", "/api/v1/playlists/"+itoa(p.ID)+"/rules", tok, map[string]any{"rules": map[string]any{"conditions": []map[string]any{{"field": "favorite", "op": "is"}}}})
	json.Unmarshal(rec.Body.Bytes(), &p)
	if rec.Code != 200 || len(p.Items) != 1 || p.Items[0].ID != ids[0] {
		t.Fatalf("rules changed %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/api/v1/playlists/"+itoa(p.ID)+"/items", tok, map[string]any{"items": []map[string]any{{"track_id": ids[1]}}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("items into a smart playlist: %d %s", rec.Code, rec.Body)
	}
}
