package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/HHim8826/kanade/server/internal/library"
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

// The album's picture for Discord (review #135): its Bangumi entry's when it has one; Kanade's own
// only when chosen, at a signed address that serves nothing unless some link shows Kanade's covers.
func TestPublicCover(t *testing.T) {
	ctx := context.Background()
	s, h := newTestServer(t)
	loginToken(t, s, h) // user 1
	s.cfg.PublicURL = "https://music.example"
	a, _ := s.lib.CreateAsset(ctx, library.Asset{SHA256: "c1", Size: 1, Format: "flac", Codec: "flac"})
	s.lib.MarkVerified(ctx, a.ID, "d-c1")
	pub, _ := s.lib.Publish(ctx, a.ID, library.EntryInput{Title: "t", Artist: "x", Album: "A", AlbumArtist: "y"})
	var album int64
	s.db.QueryRow(`SELECT album_id FROM album_entries WHERE id = ?`, pub.EntryID).Scan(&album)
	res, _ := s.db.Exec(`INSERT INTO covers (sha256, drive_file_id, mime, created_at) VALUES ('cov', 'd-cov', 'image/jpeg', 0)`)
	cover, _ := res.LastInsertId()
	s.db.Exec(`UPDATE albums SET cover_id = ? WHERE id = ?`, cover, album)

	if u := s.publicCoverURL(ctx, album, false); u != "" {
		t.Fatalf("Kanade's cover without choosing it: %q", u)
	}
	u := s.publicCoverURL(ctx, album, true)
	if !strings.HasPrefix(u, "https://music.example/pub/covers/"+itoa(cover)+"/") || !strings.HasSuffix(u, ".jpg") {
		t.Fatalf("own %q", u)
	}
	path := strings.TrimPrefix(u, "https://music.example")
	if rec := do(t, h, "GET", path, "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("served while no link shows Kanade's covers: %d", rec.Code)
	}
	s.db.Exec(`INSERT INTO discord_links (user_id, discord_id, access_token, refresh_token, expires_at, show, linked_at) VALUES (1, '42', 'a', 'r', 0, '{"cover":"all"}', 0)`)
	if rec := do(t, h, "GET", strings.Replace(path, ".jpg", "x.jpg", 1), "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("a forged address: %d", rec.Code)
	}
	if rec := do(t, h, "GET", path, "", nil); rec.Code == http.StatusNotFound || rec.Code == http.StatusUnauthorized {
		t.Fatalf("not served: %d", rec.Code) // Drive is not connected here: 503, past the checks
	}
	// Its Bangumi entry's picture first.
	w, _ := s.lib.PutWork(ctx, library.WorkData{Source: "bangumi", SourceID: "9001", Type: 3, Name: "A", Image: "https://lain.bgm.tv/pic/a.jpg"})
	s.lib.SetAlbumSubject(ctx, album, w)
	if u := s.publicCoverURL(ctx, album, false); u != "https://lain.bgm.tv/pic/a.jpg" {
		t.Fatalf("Bangumi's %q", u)
	}
}
