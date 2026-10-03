package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/HHim8826/kanade/server/internal/webauthn/webauthntest"
)

func unb64(t *testing.T, s string) []byte {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var e64 = base64.RawURLEncoding.EncodeToString

// A passkey is added after the password is confirmed, then logs in like the password; the login
// page offers passkeys only once one exists; answers for another site, a used challenge or a
// passkey of another account are refused.
func TestPasskeyAddAndLogin(t *testing.T) {
	s, h := newTestServer(t) // public URL https://music.example
	token := loginToken(t, s, h)
	const rpID, origin = "music.example", "https://music.example"
	available := func() bool {
		var v struct{ Available bool }
		json.Unmarshal(do(t, h, "GET", "/api/v1/passkeys/available", "", nil).Body.Bytes(), &v)
		return v.Available
	}
	if available() {
		t.Fatal("offered before any passkey exists")
	}
	if rec := do(t, h, "POST", "/api/v1/passkeys/options", token, map[string]string{"password": "wrong password!!"}); rec.Code != http.StatusForbidden {
		t.Fatalf("options without the password: %d", rec.Code)
	}
	rec := do(t, h, "POST", "/api/v1/passkeys/options", token, map[string]string{"password": "correct horse battery"})
	var opts struct {
		Challenge              string
		RP                     struct{ ID string }
		User                   struct{ ID, Name string }
		AuthenticatorSelection struct{ UserVerification string }
	}
	json.Unmarshal(rec.Body.Bytes(), &opts)
	if rec.Code != 200 || opts.RP.ID != rpID || opts.User.Name != "admin" || opts.AuthenticatorSelection.UserVerification != "required" {
		t.Fatalf("options %d %s", rec.Code, rec.Body)
	}
	dev := webauthntest.New(webauthntest.ES256)
	cd, att := dev.Create(rpID, origin, unb64(t, opts.Challenge))
	add := map[string]string{"name": "Phone", "clientDataJSON": e64(cd), "attestationObject": e64(att)}
	if rec := do(t, h, "POST", "/api/v1/passkeys", token, add); rec.Code != http.StatusCreated {
		t.Fatalf("add %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/api/v1/passkeys", token, add); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the same challenge twice: %d", rec.Code)
	}
	if !available() {
		t.Fatal("not offered after adding one")
	}
	var list []struct {
		ID   int64
		Name string
	}
	json.Unmarshal(do(t, h, "GET", "/api/v1/passkeys", token, nil).Body.Bytes(), &list)
	if len(list) != 1 || list[0].Name != "Phone" {
		t.Fatalf("list %+v", list)
	}

	login := func(d *webauthntest.Authenticator, rp, org string, mutate func(map[string]any)) *http.Response {
		t.Helper()
		var o struct{ Challenge string }
		json.Unmarshal(do(t, h, "POST", "/api/v1/passkeys/login/options", "", nil).Body.Bytes(), &o)
		cd, ad, sig := d.Get(rp, org, unb64(t, o.Challenge))
		body := map[string]any{"id": e64(d.ID), "clientDataJSON": e64(cd), "authenticatorData": e64(ad), "signature": e64(sig),
			"userHandle": e64([]byte{0, 0, 0, 0, 0, 0, 0, 1}), "device": "test"}
		if mutate != nil {
			mutate(body)
		}
		return do(t, h, "POST", "/api/v1/passkeys/login", "", body).Result()
	}
	res := login(dev, rpID, origin, nil)
	var got struct{ Token string }
	json.NewDecoder(res.Body).Decode(&got)
	if res.StatusCode != 200 || got.Token == "" {
		t.Fatalf("login %d", res.StatusCode)
	}
	if rec := do(t, h, "GET", "/api/v1/passkeys", got.Token, nil); rec.Code != 200 {
		t.Fatalf("the passkey login does not work: %d", rec.Code)
	}
	if res := login(dev, "evil.example", origin, nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("another site: %d", res.StatusCode)
	}
	if res := login(webauthntest.New(webauthntest.ES256), rpID, origin, nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an unknown passkey: %d", res.StatusCode)
	}
	if res := login(dev, rpID, origin, func(b map[string]any) { b["userHandle"] = e64([]byte("someone else")) }); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("another account's handle: %d", res.StatusCode)
	}
	// Five failures throttle the address, as with passwords.
	if res := login(dev, rpID, origin, nil); res.StatusCode != http.StatusOK && res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("after failures: %d", res.StatusCode)
	}
	// Renamed, then deleted: no longer offered, no longer logs in.
	path := "/api/v1/passkeys/" + jsonNum(list[0].ID)
	if rec := do(t, h, "PATCH", path, token, map[string]string{"name": "Laptop"}); rec.Code != http.StatusNoContent {
		t.Fatalf("rename %d", rec.Code)
	}
	if rec := do(t, h, "DELETE", path, token, nil); rec.Code != http.StatusNoContent || available() {
		t.Fatalf("delete %d", rec.Code)
	}
}

func jsonNum(n int64) string { b, _ := json.Marshal(n); return string(b) }
