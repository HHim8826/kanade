package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/HHim8826/kanade/server/internal/settings"
	"github.com/HHim8826/kanade/server/internal/staging"
)

// The service settings are kept, checked and applied when saved; a resource given as a serve flag
// keeps its value until the next start (reviews #74, #75, #77).
func TestServiceSettings(t *testing.T) {
	s, h := newTestServer(t)
	tok := loginToken(t, s, h)
	budget := &staging.Budget{Limit: 2048 << 20, Reserve: 4 << 30}
	var downloads settings.Downloads
	var drive settings.Drive
	s.staging, s.pinned = budget, map[string]bool{"reserve_gib": true}
	s.settings = &settings.Store{DB: s.db,
		OnResources: func(r settings.Resources) {
			s.cache.SetBudget(r.CacheMiB << 20)
			_, reserve := budget.Limits()
			budget.SetLimits(r.StagingMiB<<20, reserve) // the reserve is pinned
		},
		OnDownloads: func(d settings.Downloads) { downloads = d },
		OnDrive:     func(d settings.Drive) { drive = d },
	}
	type got struct {
		Resources struct {
			Saved  settings.Resources
			Now    map[string]int64
			Pinned []string
			Usage  map[string]int64
		}
		Downloads settings.Downloads
		Drive     settings.Drive
		Server    map[string]any
	}
	read := func(rec *httptest.ResponseRecorder) got {
		t.Helper()
		var g got
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &g) != nil {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		return g
	}
	g := read(do(t, h, "GET", "/api/v1/settings", tok, nil))
	if g.Resources.Saved != settings.DefaultResources() || g.Downloads != settings.DefaultDownloads() || g.Drive != settings.DefaultDrive() ||
		g.Resources.Now["cache_mib"] != 64 || len(g.Resources.Pinned) != 1 || g.Server["public_url"] != "https://music.example" {
		t.Fatalf("defaults %+v", g)
	}
	if rec := do(t, h, "PUT", "/api/v1/settings/resources", tok, map[string]int{"cache_mib": 1, "staging_mib": 4096, "reserve_gib": 8}); rec.Code != 400 {
		t.Fatalf("too small: %d", rec.Code)
	}
	g = read(do(t, h, "PUT", "/api/v1/settings/resources", tok, settings.Resources{CacheMiB: 256, StagingMiB: 4096, ReserveGiB: 8}))
	if g.Resources.Saved.ReserveGiB != 8 || g.Resources.Now["cache_mib"] != 256 || g.Resources.Now["staging_mib"] != 4096 || g.Resources.Now["reserve_gib"] != 4 {
		t.Fatalf("resources %+v", g.Resources)
	}
	if rec := do(t, h, "PUT", "/api/v1/settings/downloads", tok, settings.Downloads{Concurrent: 9, MaxPeers: 30}); rec.Code != 400 {
		t.Fatalf("too many at once: %d", rec.Code)
	}
	want := settings.Downloads{DownKiB: 500, Concurrent: 2, MaxPeers: 40, Seed: true, SeedRatio: 2, SeedHours: 0}
	if g = read(do(t, h, "PUT", "/api/v1/settings/downloads", tok, want)); g.Downloads != want || downloads != want {
		t.Fatalf("downloads %+v %+v", g.Downloads, downloads)
	}
	wantDrive := settings.Drive{AutoInbox: false, CheckMinutes: 30, SettleMinutes: 0}
	if g = read(do(t, h, "PUT", "/api/v1/settings/drive", tok, wantDrive)); g.Drive != wantDrive || drive != wantDrive {
		t.Fatalf("drive %+v", g.Drive)
	}
	// Kept: a store reading the same database finds them.
	again := &settings.Store{DB: s.db}
	if r, _ := again.Resources(context.Background()); r.CacheMiB != 256 {
		t.Fatalf("kept %+v", r)
	}
	if rec := do(t, h, "POST", "/api/v1/cache/trim", tok, nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "freed_bytes") {
		t.Fatalf("trim: %d %s", rec.Code, rec.Body)
	}
}
