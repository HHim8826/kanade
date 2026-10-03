package gdrive

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
)

// fakeTokenServer answers the authorization_code grant like Google does.
func fakeTokenServer(t *testing.T, scope string, testing bool) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("client_secret") != "secret" || r.Form.Get("code") != "good-code" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		resp := map[string]any{"access_token": "at", "refresh_token": "rt", "expires_in": 3599, "scope": scope, "token_type": "Bearer"}
		if testing {
			resp["refresh_token_expires_in"] = 604799
		}
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	old := tokenEndpoint
	tokenEndpoint = srv.URL
	t.Cleanup(func() { tokenEndpoint = old })
}

func newClient(t *testing.T) *Client {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	c := New(d, "https://music.example/oauth/google/callback")
	if err := c.SetClientConfig(context.Background(), []byte(`{"web":{"client_id":"id","client_secret":"secret"}}`)); err != nil {
		t.Fatal(err)
	}
	return c
}

func stateOf(t *testing.T, authURL string) string {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("redirect_uri"); got != "https://music.example/oauth/google/callback" {
		t.Fatalf("redirect_uri = %q", got)
	}
	return u.Query().Get("state")
}

func TestCallbackStoresTokenAndStateIsSingleUse(t *testing.T) {
	ctx := context.Background()
	fakeTokenServer(t, DriveScope, false)
	c := newClient(t)
	u, err := c.AuthURL(ctx, "someone@example.com")
	if err != nil {
		t.Fatal(err)
	}
	state := stateOf(t, u)
	st, err := c.Complete(ctx, url.Values{"state": {state}, "code": {"good-code"}})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Connected || st.TestingMode {
		t.Fatalf("status = %+v", st)
	}
	if _, err := c.Complete(ctx, url.Values{"state": {state}, "code": {"good-code"}}); !errors.Is(err, ErrBadState) {
		t.Fatalf("replayed state: err = %v, want ErrBadState", err)
	}
}

func TestForgedStateRejected(t *testing.T) {
	fakeTokenServer(t, DriveScope, false)
	c := newClient(t)
	if _, err := c.Complete(context.Background(), url.Values{"state": {"made-up"}, "code": {"good-code"}}); !errors.Is(err, ErrBadState) {
		t.Fatalf("err = %v, want ErrBadState", err)
	}
	if st, _ := c.Status(context.Background()); st.Connected {
		t.Fatal("forged callback connected Drive")
	}
}

func TestMissingDriveScopeRejected(t *testing.T) {
	ctx := context.Background()
	fakeTokenServer(t, "openid email", false)
	c := newClient(t)
	u, _ := c.AuthURL(ctx, "")
	_, err := c.Complete(ctx, url.Values{"state": {stateOf(t, u)}, "code": {"good-code"}})
	if err == nil || !strings.Contains(err.Error(), "Drive") {
		t.Fatalf("err = %v, want a missing-scope error", err)
	}
}

func TestTestingModeDetectedAndPastedURL(t *testing.T) {
	ctx := context.Background()
	fakeTokenServer(t, DriveScope, true)
	c := newClient(t)
	u, _ := c.AuthURL(ctx, "")
	pasted := "https://music.example/oauth/google/callback?state=" + stateOf(t, u) + "&code=good-code&scope=x"
	st, err := c.CompletePasted(ctx, pasted)
	if err != nil {
		t.Fatal(err)
	}
	if !st.TestingMode {
		t.Fatal("refresh_token_expires_in present but TestingMode false")
	}
}

func TestQuoteEscapesDriveQuery(t *testing.T) {
	if got := quote(`it's a \ test`); got != `'it\'s a \\ test'` {
		t.Fatalf("quote = %s", got)
	}
}

func TestRedirectFor(t *testing.T) {
	for _, c := range []struct{ public, want string }{
		{"https://music.example.com", "https://music.example.com/oauth/google/callback"},
		{"https://music.example.com/kanade/", "https://music.example.com/kanade/oauth/google/callback"},
		{"http://localhost:8080", "http://localhost:8080/oauth/google/callback"},
		{"http://127.0.0.1:8097", "http://127.0.0.1:8097/oauth/google/callback"},
		// Google takes neither plain http to another host nor an IP address: paste instead.
		{"http://203.0.113.7:8080", LocalhostRedirect},
		{"https://203.0.113.7", LocalhostRedirect},
		{"http://music.example.com", LocalhostRedirect},
		{"https://nas", LocalhostRedirect},
	} {
		if got := RedirectFor(c.public); got != c.want {
			t.Errorf("RedirectFor(%q) = %q, want %q", c.public, got, c.want)
		}
	}
}

func TestStatusTellsTheRedirect(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	c := New(d, RedirectFor("http://203.0.113.7:8080"))
	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.HasClient || st.ClientID != "" || !st.Paste || st.RedirectURI != LocalhostRedirect {
		t.Fatalf("before a client: %+v", st)
	}
	if err := c.SetClientConfig(ctx, []byte(`{"client_id":"123.apps.googleusercontent.com","client_secret":"s"}`)); err != nil {
		t.Fatal(err)
	}
	if st, _ = c.Status(ctx); !st.HasClient || st.ClientID != "123.apps.googleusercontent.com" {
		t.Fatalf("after a client: %+v", st)
	}
	u, err := c.AuthURL(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := mustParse(t, u).Query().Get("redirect_uri"); got != LocalhostRedirect {
		t.Fatalf("redirect_uri = %q", got)
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestAnotherClientMeansConnectingAgain(t *testing.T) {
	ctx := context.Background()
	fakeTokenServer(t, DriveScope, false)
	c := newClient(t)
	u, _ := c.AuthURL(ctx, "")
	if _, err := c.CompletePasted(ctx, "https://music.example/oauth/google/callback?state="+stateOf(t, u)+"&code=good-code"); err != nil {
		t.Fatal(err)
	}
	// The same client again (saved twice): still connected.
	if err := c.SetClientConfig(ctx, []byte(`{"client_id":"id","client_secret":"secret"}`)); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.Status(ctx); !st.Connected {
		t.Fatal("saving the same client disconnected Drive")
	}
	if err := c.SetClientConfig(ctx, []byte(`{"client_id":"other","client_secret":"s2"}`)); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.Status(ctx); st.Connected || st.ClientID != "other" {
		t.Fatalf("after another client: %+v", st)
	}
	// Also after a restart (read from the database).
	again := New(c.db, c.redirectURI)
	if st, _ := again.Status(ctx); st.Connected {
		t.Fatal("the old token is still stored")
	}
}

// Switching clients is one transaction: when dropping the old token fails, the old client and its
// token stay, in memory and in the database, and saving the new client again works (review #70).
func TestFailedClientSwitchKeepsTheOldClient(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	if err := c.ImportToken(ctx, Token{AccessToken: "at", RefreshToken: "rt", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `CREATE TRIGGER deny_token_delete BEFORE DELETE ON credentials
		WHEN OLD.name = 'google_token' BEGIN SELECT RAISE(ABORT, 'simulated storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := c.SetClientConfig(ctx, []byte(`{"client_id":"other","client_secret":"s2"}`)); err == nil {
		t.Fatal("the switch did not fail")
	}
	for name, cl := range map[string]*Client{"in memory": c, "after a restart": New(c.db, c.redirectURI)} {
		if st, err := cl.Status(ctx); err != nil || !st.Connected || st.ClientID != "id" {
			t.Fatalf("%s: %+v %v", name, st, err)
		}
	}
	if _, err := c.db.ExecContext(ctx, `DROP TRIGGER deny_token_delete`); err != nil {
		t.Fatal(err)
	}
	again := New(c.db, c.redirectURI)
	if err := again.SetClientConfig(ctx, []byte(`{"client_id":"other","client_secret":"s2"}`)); err != nil {
		t.Fatal(err)
	}
	if st, _ := New(c.db, c.redirectURI).Status(ctx); st.Connected || st.ClientID != "other" {
		t.Fatalf("after saving again: %+v", st)
	}
}

// A refresh token Google no longer accepts is told apart from other failures (review #64).
func TestRevokedGrantMeansConnectingAgain(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "Token has been expired or revoked."})
	}))
	defer srv.Close()
	old := tokenEndpoint
	tokenEndpoint = srv.URL
	defer func() { tokenEndpoint = old }()
	c := newClient(t)
	c.ImportToken(ctx, Token{AccessToken: "at", RefreshToken: "rt", Expiry: time.Now().Add(-time.Hour)})
	_, err := c.About(ctx)
	if !errors.Is(err, ErrAuthExpired) || !strings.Contains(err.Error(), "Token has been expired or revoked.") {
		t.Fatalf("err = %v", err)
	}
}
