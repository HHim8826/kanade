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
