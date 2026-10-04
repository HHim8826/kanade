package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/library"
)

// The statistics endpoints add up plays reported the usual way, in the time zone asked for; the
// listening can be exported and cleared (review #93).
func TestStatsEndpoints(t *testing.T) {
	ctx := context.Background()
	s, h := newTestServer(t)
	tok := loginToken(t, s, h)
	a, _ := s.lib.CreateAsset(ctx, library.Asset{SHA256: "st", Size: 1, Format: "flac", Codec: "flac", DurationMS: 200_000})
	s.lib.MarkVerified(ctx, a.ID, "d-st")
	s.lib.Publish(ctx, a.ID, library.EntryInput{Title: "Song", Artist: "Singer", Album: "Album", AlbumArtist: "Singer"})
	for i, heard := range []int64{60_000, 120_000, 120_000} { // the last one sent again
		if rec := do(t, h, "POST", "/api/v1/plays", tok, map[string]any{"session": "s1", "asset_id": a.ID, "listened_ms": heard,
			"position_ms": heard, "seq": i + 1}); rec.Code != http.StatusNoContent && rec.Code != 200 {
			t.Fatalf("play report: %d %s", rec.Code, rec.Body)
		}
	}
	get := func(path string, out any) int {
		t.Helper()
		rec := do(t, h, "GET", path, tok, nil)
		if out != nil {
			json.Unmarshal(rec.Body.Bytes(), out)
		}
		return rec.Code
	}
	tz := "&tz=Asia%2FTaipei"
	today := time.Now().In(func() *time.Location { l, _ := time.LoadLocation("Asia/Taipei"); return l }()).Format(time.DateOnly)
	var sum map[string]library.Summary
	if code := get("/api/v1/stats/summary?kind=music"+tz, &sum); code != 200 || sum["today"].MS != 120_000 || sum["today"].Plays != 1 || sum["year"].Tracks != 1 {
		t.Fatalf("summary %d %+v", code, sum)
	}
	var days struct {
		Days []library.DayTotal `json:"days"`
	}
	if get("/api/v1/stats/days?year=0"+tz, &days); len(days.Days) != 1 || days.Days[0].Date != today {
		t.Fatalf("days %+v", days)
	}
	var top []library.TopItem
	if get("/api/v1/stats/top?group=tracks&by=plays"+tz, &top); len(top) != 1 || top[0].Track == nil || top[0].Plays != 1 {
		t.Fatalf("top %+v", top)
	}
	var tr library.Trends
	if code := get("/api/v1/stats/trends?from="+today+"&to="+today+tz, &tr); code != 200 || len(tr.Days) != 1 || tr.NewCount != 1 {
		t.Fatalf("trends %d %+v", code, tr)
	}
	if code := get("/api/v1/stats/days?tz=Nowhere%2FCity", nil); code != http.StatusBadRequest {
		t.Fatalf("unknown zone: %d", code)
	}
	rec := do(t, h, "GET", "/api/v1/stats/export?format=csv"+tz, tok, nil)
	if lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n"); len(lines) < 2 || !strings.Contains(lines[1], "Song") {
		t.Fatalf("export: %q", rec.Body.String())
	}
	if rec := do(t, h, "DELETE", "/api/v1/stats", tok, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("clear: %d", rec.Code)
	}
	if get("/api/v1/stats/summary?"+tz[1:], &sum); sum["today"].MS != 0 {
		t.Fatalf("after clearing %+v", sum)
	}
}
