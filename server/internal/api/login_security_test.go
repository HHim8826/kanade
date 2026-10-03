package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/HHim8826/kanade/server/internal/webauthn/webauthntest"
)

func loginRequest(t *testing.T, h http.Handler, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", path, bytes.NewReader(b))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestPasswordCookieLoginRequiresSameOriginAndCSRFHeader(t *testing.T) {
	s, h := newTestServer(t)
	if err := s.auth.CreateUser(context.Background(), "admin", "correct horse battery", false); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"username": "admin", "password": "correct horse battery", "cookie": true}
	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{"cross-site form", map[string]string{"Origin": "https://evil.example", "Content-Type": "text/plain"}},
		{"foreign origin with header", map[string]string{"Origin": "https://evil.example", csrfHeader: "kanade"}},
		{"same-site different origin", map[string]string{"Sec-Fetch-Site": "same-site", csrfHeader: "kanade"}},
		{"cross-site without origin", map[string]string{"Sec-Fetch-Site": "cross-site", csrfHeader: "kanade"}},
		{"missing header", map[string]string{"Origin": "https://music.example"}},
		{"no browser headers", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := loginRequest(t, h, "/api/v1/login", body, tc.headers)
			if r.Code != http.StatusForbidden || r.Header().Get("Set-Cookie") != "" {
				t.Fatalf("unsafe login accepted: %d cookie set=%v", r.Code, r.Header().Get("Set-Cookie") != "")
			}
		})
	}
	r := loginRequest(t, h, "/api/v1/login", body, map[string]string{"Origin": "https://music.example", "Sec-Fetch-Site": "same-origin", csrfHeader: "kanade"})
	if r.Code != http.StatusOK {
		t.Fatalf("same-origin login: %d %s", r.Code, r.Body)
	}
	cookies := r.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].MaxAge != 90*24*3600 {
		t.Fatalf("insecure session cookie: %+v", cookies)
	}
	if bytes.Contains(r.Body.Bytes(), []byte(cookies[0].Value)) {
		t.Fatal("cookie token exposed to page scripts")
	}
	body["cookie"] = false
	if r := loginRequest(t, h, "/api/v1/login", body, nil); r.Code != 200 || r.Header().Get("Set-Cookie") != "" {
		t.Fatalf("native token login: %d", r.Code)
	}
}

func TestPasskeyCookieLoginCannotBeInjectedCrossSite(t *testing.T) {
	s, h := newTestServer(t)
	if err := s.auth.CreateUser(context.Background(), "admin", "correct horse battery", false); err != nil {
		t.Fatal(err)
	}
	const rpID, origin = "music.example", "https://music.example"
	rp, _ := s.rp()
	a := webauthntest.New(webauthntest.ES256)
	ch, err := s.auth.NewRegistrationChallenge(context.Background(), 1, "correct horse battery", "test")
	if err != nil {
		t.Fatal(err)
	}
	cd, att := a.Create(rpID, origin, ch)
	if _, err := s.auth.AddPasskey(context.Background(), rp, 1, "test", cd, att); err != nil {
		t.Fatal(err)
	}
	var o struct{ Challenge string }
	if r := do(t, h, "POST", "/api/v1/passkeys/login/options", "", nil); json.Unmarshal(r.Body.Bytes(), &o) != nil || r.Code != 200 {
		t.Fatal("options")
	}
	cd, ad, sig := a.Get(rpID, origin, unb64(t, o.Challenge))
	body := map[string]any{"id": e64(a.ID), "clientDataJSON": e64(cd), "authenticatorData": e64(ad), "signature": e64(sig), "userHandle": e64([]byte{0, 0, 0, 0, 0, 0, 0, 1}), "cookie": true}
	for _, headers := range []map[string]string{
		{"Origin": "https://evil.example", "Content-Type": "text/plain"},
		{"Origin": "https://evil.example", csrfHeader: "kanade"},
		nil,
	} {
		r := loginRequest(t, h, "/api/v1/passkeys/login", body, headers)
		if r.Code != http.StatusForbidden || r.Header().Get("Set-Cookie") != "" {
			t.Errorf("injected assertion accepted: %d cookie set=%v", r.Code, r.Header().Get("Set-Cookie") != "")
		}
	}
	// Rejected cross-site requests must not consume the legitimate login challenge.
	r := loginRequest(t, h, "/api/v1/passkeys/login", body, map[string]string{"Origin": origin, csrfHeader: "kanade"})
	if r.Code != http.StatusOK || len(r.Result().Cookies()) != 1 {
		t.Fatalf("legitimate passkey login: %d %s", r.Code, r.Body)
	}
}

func TestCrossSiteCannotAllocatePasskeyChallenges(t *testing.T) {
	_, h := newTestServer(t)
	for range 10 {
		r := loginRequest(t, h, "/api/v1/passkeys/login/options", nil, map[string]string{"Origin": "https://evil.example", "Content-Type": "text/plain"})
		if r.Code != http.StatusForbidden {
			t.Errorf("cross-site challenge allocated: %d", r.Code)
		}
	}
	if r := do(t, h, "POST", "/api/v1/passkeys/login/options", "", nil); r.Code != http.StatusOK {
		t.Fatalf("cross-site requests exhausted the legitimate client's challenge budget: %d", r.Code)
	}
}
