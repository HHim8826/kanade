package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/auth"
)

// A page of another site cannot log the browser in, or use up the address's passkey requests; the
// web client's same-site logins and the apps' token logins work.
func TestLoginRefusesOtherSites(t *testing.T) {
	s, h := newTestServer(t)
	if err := s.auth.CreateUser(context.Background(), "admin", "correct horse battery", false); err != nil {
		t.Fatal(err)
	}
	send := func(path, body string, header map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "https://music.example"+path, strings.NewReader(body))
		for k, v := range header {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	sessions := func() int {
		var n int
		s.db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&n)
		return n
	}
	const cookieLogin = `{"username":"admin","password":"correct horse battery","cookie":true}`
	web := map[string]string{"Content-Type": "application/json", "Origin": "https://music.example", "Sec-Fetch-Site": "same-origin", "X-Requested-With": "kanade"}
	for _, c := range []struct {
		name   string
		header map[string]string
	}{
		// What a form on another site sends: text/plain, cross-site, no custom header.
		{"form of another site", map[string]string{"Content-Type": "text/plain", "Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}},
		{"another site, with the header", map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site", "X-Requested-With": "kanade"}},
		{"another subdomain", map[string]string{"Origin": "https://evil.music.example", "Sec-Fetch-Site": "same-site", "X-Requested-With": "kanade"}},
		{"another origin, older browser", map[string]string{"Origin": "https://evil.example", "X-Requested-With": "kanade"}},
		{"same site without the header", map[string]string{"Origin": "https://music.example", "Sec-Fetch-Site": "same-origin"}},
	} {
		if rec := send("/api/v1/login", cookieLogin, c.header); rec.Code != http.StatusForbidden || rec.Header().Get("Set-Cookie") != "" {
			t.Errorf("%s: %d %q", c.name, rec.Code, rec.Header().Get("Set-Cookie"))
		}
	}
	if n := sessions(); n != 0 {
		t.Fatalf("refused logins made %d sessions", n)
	}
	// Refused passkey requests take no challenge from the address's budget.
	for range 10 {
		if rec := send("/api/v1/passkeys/login/options", `{}`, map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}); rec.Code != http.StatusForbidden {
			t.Fatalf("options from another site: %d", rec.Code)
		}
	}
	if rec := send("/api/v1/passkeys/login/options", `{}`, web); rec.Code != http.StatusOK {
		t.Fatalf("options from the site: %d %s", rec.Code, rec.Body)
	}
	if rec := send("/api/v1/passkeys/login", `{"cookie":true}`, map[string]string{"Origin": "https://music.example", "Sec-Fetch-Site": "same-origin"}); rec.Code != http.StatusForbidden {
		t.Fatalf("passkey cookie login without the header: %d", rec.Code)
	}

	rec := send("/api/v1/login", cookieLogin, web)
	cookie := rec.Result().Cookies()
	if rec.Code != http.StatusOK || len(cookie) != 1 || !cookie[0].HttpOnly || !cookie[0].Secure || cookie[0].SameSite != http.SameSiteStrictMode ||
		cookie[0].MaxAge != int(auth.SessionLifetime/time.Second) {
		t.Fatalf("web login %d %+v", rec.Code, cookie)
	}
	// An app sends no Origin and asks for the token.
	if rec := send("/api/v1/login", `{"username":"admin","password":"correct horse battery"}`, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "token") {
		t.Fatalf("app login %d %s", rec.Code, rec.Body)
	}
}
