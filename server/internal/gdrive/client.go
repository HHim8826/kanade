// Package gdrive talks to the Google Drive v3 REST API directly (decision D1).
package gdrive

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
)

const (
	apiBase    = "https://www.googleapis.com/drive/v3"
	uploadBase = "https://www.googleapis.com/upload/drive/v3"
	FolderMime = "application/vnd.google-apps.folder"

	credClient = "google_client"
	credToken  = "google_token"
)

var ErrNotConnected = errors.New("google drive is not connected")

// ErrAuthExpired: Google no longer accepts the refresh token (it expired, as it does after 7 days
// while the OAuth app is in testing, or access was revoked): connecting again is the way back.
var ErrAuthExpired = errors.New("google authorization expired or was revoked")

// ClientConfig is the OAuth client registered in Google Cloud.
type ClientConfig struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// Token is what the token endpoint gave us.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
	Scope        string    `json:"scope"`
	ObtainedAt   time.Time `json:"obtained_at"`
	// Only sent while the OAuth app is in "Testing" (refresh token valid for 7 days).
	RefreshTokenExpiresIn int `json:"refresh_token_expires_in,omitempty"`
}

type Client struct {
	db          *sql.DB
	redirectURI string
	http        *http.Client

	mu      sync.Mutex
	cfg     *ClientConfig
	tok     *Token
	checked map[string]checkedFolder // cached folder ID -> when it was last seen in its parent
}

func New(d *sql.DB, redirectURI string) *Client {
	return &Client{db: d, redirectURI: redirectURI, http: &http.Client{}}
}

func (c *Client) readCredential(ctx context.Context, name string, v any) (bool, error) {
	var raw string
	err := c.db.QueryRowContext(ctx, `SELECT value FROM credentials WHERE name = ?`, name).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(raw), v)
}

func (c *Client) writeCredential(ctx context.Context, name string, v any) error {
	return putCredential(ctx, c.db, name, v)
}

func putCredential(ctx context.Context, ex interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, name string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = ex.ExecContext(ctx, `INSERT INTO credentials (name, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, name, string(raw), db.Now())
	return err
}

// loadLocked reads the client and token from the database the first time they are needed.
func (c *Client) loadLocked(ctx context.Context) error {
	if c.cfg == nil {
		var cfg ClientConfig
		ok, err := c.readCredential(ctx, credClient, &cfg)
		if err != nil {
			return err
		}
		if ok {
			c.cfg = &cfg
		}
	}
	if c.tok == nil {
		var tok Token
		ok, err := c.readCredential(ctx, credToken, &tok)
		if err != nil {
			return err
		}
		if ok {
			c.tok = &tok
		}
	}
	return nil
}

// SetClientConfig accepts the JSON downloaded from Google Cloud ({"web": {...}} or
// {"installed": {...}}) or a bare {"client_id", "client_secret"} object.
func (c *Client) SetClientConfig(ctx context.Context, raw []byte) error {
	var wrapped struct {
		Web       *ClientConfig `json:"web"`
		Installed *ClientConfig `json:"installed"`
	}
	var cfg ClientConfig
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return err
	}
	switch {
	case wrapped.Web != nil:
		cfg = *wrapped.Web
	case wrapped.Installed != nil:
		cfg = *wrapped.Installed
	default:
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return err
		}
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return errors.New("client_id or client_secret missing")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.loadLocked(ctx); err != nil {
		return err
	}
	// A token can only be refreshed by the client it was granted to: another client means
	// connecting again. Both in one transaction, so a failure keeps the old client and its token
	// (review #70).
	other := c.cfg != nil && c.cfg.ClientID != cfg.ClientID
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := putCredential(ctx, tx, credClient, cfg); err != nil {
		return err
	}
	if other {
		if _, err := tx.ExecContext(ctx, `DELETE FROM credentials WHERE name = ?`, credToken); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if other {
		c.tok = nil
	}
	c.cfg = &cfg
	return nil
}

// ImportToken stores a token obtained elsewhere (for example by the P0 spike).
func (c *Client) ImportToken(ctx context.Context, tok Token) error {
	if tok.RefreshToken == "" {
		return errors.New("token has no refresh_token")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.writeCredential(ctx, credToken, tok); err != nil {
		return err
	}
	c.tok = &tok
	return nil
}

type Status struct {
	HasClient   bool   `json:"has_client"`
	ClientID    string `json:"client_id,omitempty"`
	Connected   bool   `json:"connected"`
	TestingMode bool   `json:"testing_mode"` // refresh token expires after 7 days
	Account     string `json:"account,omitempty"`
	// RedirectURI is the one to register with the OAuth client. Paste: it is LocalhostRedirect,
	// so the authorization ends with pasting the address the browser shows.
	RedirectURI string `json:"redirect_uri"`
	Paste       bool   `json:"paste"`
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	c.mu.Lock()
	err := c.loadLocked(ctx)
	st := Status{HasClient: c.cfg != nil, Connected: c.tok != nil, RedirectURI: c.redirectURI, Paste: c.redirectURI == LocalhostRedirect}
	if c.cfg != nil {
		st.ClientID = c.cfg.ClientID
	}
	if c.tok != nil {
		st.TestingMode = c.tok.RefreshTokenExpiresIn > 0
	}
	c.mu.Unlock()
	return st, err
}

// bearer returns a valid access token, refreshing it when it is about to expire.
func (c *Client) bearer(ctx context.Context, forceRefresh bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.loadLocked(ctx); err != nil {
		return "", err
	}
	if c.cfg == nil || c.tok == nil {
		return "", ErrNotConnected
	}
	if forceRefresh || time.Until(c.tok.Expiry) < time.Minute {
		t, fields, err := c.exchange(ctx, url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {c.tok.RefreshToken},
		})
		if err != nil {
			var te *tokenError
			if errors.As(err, &te) && te.code == "invalid_grant" {
				return "", fmt.Errorf("refresh access token: %w (%v)", ErrAuthExpired, err)
			}
			return "", fmt.Errorf("refresh access token: %w", err)
		}
		c.tok.AccessToken, c.tok.Expiry = t.AccessToken, t.Expiry
		if t.RefreshToken != "" {
			c.tok.RefreshToken = t.RefreshToken
		}
		if _, ok := fields["refresh_token_expires_in"]; ok {
			c.tok.RefreshTokenExpiresIn = t.RefreshTokenExpiresIn
		}
		if err := c.writeCredential(ctx, credToken, c.tok); err != nil {
			return "", err
		}
	}
	return "Bearer " + c.tok.AccessToken, nil
}

// APIError is an error response from Drive.
type APIError struct {
	Status  int
	Reason  string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("drive %d %s: %s", e.Status, e.Reason, e.Message)
}

func IsNotFound(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func parseAPIError(status int, body []byte) *APIError {
	var v struct {
		Error struct {
			Message string `json:"message"`
			Errors  []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	e := &APIError{Status: status, Message: strings.TrimSpace(string(body))}
	if json.Unmarshal(body, &v) == nil && v.Error.Message != "" {
		e.Message = v.Error.Message
		if len(v.Error.Errors) > 0 {
			e.Reason = v.Error.Errors[0].Reason
		}
	}
	return e
}

func retryable(e *APIError) bool {
	switch e.Status {
	case 429, 500, 502, 503, 504:
		return true
	case 403:
		return e.Reason == "rateLimitExceeded" || e.Reason == "userRateLimitExceeded"
	}
	return false
}

func backoff(attempt int) time.Duration {
	return time.Duration(1<<attempt)*time.Second + time.Duration(rand.IntN(1000))*time.Millisecond
}

// Do sends a request whose body can be replayed. It refreshes the token once on 401 and
// retries rate limits and 5xx with exponential backoff. The caller closes the body.
func (c *Client) Do(ctx context.Context, method, rawURL string, body []byte, header http.Header) (*http.Response, error) {
	force := false
	for attempt := 0; ; attempt++ {
		auth, err := c.bearer(ctx, force)
		if err != nil {
			return nil, err
		}
		force = false
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
		if err != nil {
			return nil, err
		}
		for k, v := range header {
			req.Header[k] = v
		}
		req.Header.Set("Authorization", auth)
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil || attempt >= 4 {
				return nil, err
			}
			if !sleepCtx(ctx, backoff(attempt)) {
				return nil, ctx.Err()
			}
			continue
		}
		if resp.StatusCode < 400 {
			return resp, nil
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		e := parseAPIError(resp.StatusCode, data)
		switch {
		case e.Status == http.StatusUnauthorized && attempt == 0:
			force = true
		case retryable(e) && attempt < 5:
			if !sleepCtx(ctx, backoff(attempt)) {
				return nil, ctx.Err()
			}
		default:
			return nil, e
		}
	}
}

// Call is Do for JSON APIs.
func (c *Client) Call(ctx context.Context, method, rawURL string, in, out any) error {
	var body []byte
	header := http.Header{}
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
		header.Set("Content-Type", "application/json; charset=UTF-8")
	}
	resp, err := c.Do(ctx, method, rawURL, body, header)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// UseHTTPClient replaces the HTTP client before the client is used, for tests that answer
// Google's endpoints locally.
func (c *Client) UseHTTPClient(h *http.Client) { c.http = h }
