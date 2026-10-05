package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The Discord endpoints (review #135): the secret never comes back; players tell nothing while the
// account is not linked; Discord's callback goes back to the settings page saying how it went.
func TestDiscordEndpoints(t *testing.T) {
	s, h := newTestServer(t)
	tok := loginToken(t, s, h)
	report := map[string]any{"device": "desk", "seq": 1, "state": "playing", "title": "Undine", "duration_ms": 1000}
	rec := do(t, h, "PUT", "/api/v1/presence/players/tab-aaaa-1", tok, report)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"publish":false`) || len(s.presence.Players(1)) != 0 {
		t.Fatalf("not linked: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "PUT", "/api/v1/presence/players/x", tok, report); rec.Code != http.StatusBadRequest {
		t.Fatalf("a bad player: %d", rec.Code)
	}
	if rec := do(t, h, "POST", "/api/v1/discord/link", tok, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("no application: %d", rec.Code)
	}
	if rec := do(t, h, "PUT", "/api/v1/discord/app", tok, map[string]string{"client_id": "123456789012345678", "client_secret": "s3cret-value"}); rec.Code != 204 {
		t.Fatalf("app: %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, "GET", "/api/v1/discord", tok, nil)
	var info struct {
		ClientID    string `json:"client_id"`
		HasSecret   bool   `json:"has_secret"`
		RedirectURI string `json:"redirect_uri"`
	}
	json.Unmarshal(rec.Body.Bytes(), &info)
	if strings.Contains(rec.Body.String(), "s3cret") || !info.HasSecret || info.RedirectURI != "https://music.example/oauth/discord/callback" {
		t.Fatalf("info %s", rec.Body)
	}
	// Saving without a secret keeps it.
	do(t, h, "PUT", "/api/v1/discord/app", tok, map[string]string{"client_id": "123456789012345678", "image": "kanade"})
	if app := s.discord.App(t.Context()); app.Secret != "s3cret-value" {
		t.Fatal("the secret went")
	}
	var begun struct{ URL string }
	rec = do(t, h, "POST", "/api/v1/discord/link", tok, nil)
	json.Unmarshal(rec.Body.Bytes(), &begun)
	if !strings.HasPrefix(begun.URL, "https://discord.com/oauth2/authorize?") || !strings.Contains(begun.URL, "sdk.social_layer_presence") {
		t.Fatalf("begin: %s", rec.Body)
	}
	for q, want := range map[string]string{"?error=access_denied": "denied", "?state=forged&code=x": "expired"} {
		rec := do(t, h, "GET", "/oauth/discord/callback"+q, "", nil)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/app/#/settings?discord="+want {
			t.Fatalf("%s: %d %s", q, rec.Code, rec.Header().Get("Location"))
		}
	}
	if rec := do(t, h, "PATCH", "/api/v1/discord/link", tok, map[string]string{"status": "idle"}); rec.Code != http.StatusNotFound {
		t.Fatalf("change without a link: %d", rec.Code)
	}
}
