package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/gdrive"
)

// googleSays answers every request to Google with one response.
type googleSays struct {
	status int
	body   string
}

func (g googleSays) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: g.status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(g.body))}, nil
}

// When the account cannot be read (the authorization expired, Google is unreachable), the status
// still comes back, so the page keeps connecting again and changing the client (review #64).
func TestDriveInfoKeepsTheStatusWhenGoogleFails(t *testing.T) {
	ctx := context.Background()
	s, h := newTestServer(t)
	tok := loginToken(t, s, h)
	if err := s.drive.SetClientConfig(ctx, []byte(`{"client_id":"id","client_secret":"secret"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.drive.ImportToken(ctx, gdrive.Token{AccessToken: "at", RefreshToken: "rt", Expiry: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name      string
		google    googleSays
		reconnect bool
	}{
		{"revoked", googleSays{400, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`}, true},
		{"unreachable", googleSays{403, `{"error":{"message":"nope","errors":[{"reason":"forbidden"}]}}`}, false},
	} {
		if c.name == "unreachable" { // the token refreshes, then Drive refuses
			s.drive.UseHTTPClient(&http.Client{Transport: refreshThen{c.google}})
		} else {
			s.drive.UseHTTPClient(&http.Client{Transport: c.google})
		}
		rec := do(t, h, "GET", "/api/v1/drive", tok, nil)
		var out struct {
			Status       gdrive.Status `json:"status"`
			Account      any           `json:"account"`
			AccountError string        `json:"account_error"`
			Reconnect    bool          `json:"reconnect"`
		}
		json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != http.StatusOK || !out.Status.HasClient || !out.Status.Connected || out.Account != nil ||
			out.AccountError == "" || out.Reconnect != c.reconnect {
			t.Fatalf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
		if rec := do(t, h, "POST", "/api/v1/drive/auth", tok, nil); rec.Code != http.StatusOK {
			t.Fatalf("%s: connecting again: %d %s", c.name, rec.Code, rec.Body)
		}
	}
}

// refreshThen gives a fresh access token, then answers Drive with the given response.
type refreshThen struct{ drive googleSays }

func (r refreshThen) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "oauth2.googleapis.com" {
		return googleSays{200, `{"access_token":"new","expires_in":3600}`}.RoundTrip(req)
	}
	return r.drive.RoundTrip(req)
}
