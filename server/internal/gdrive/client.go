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
	"net"
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

	mu         sync.Mutex
	cfg        *ClientConfig
	tok        *Token
	refreshing *refresh                 // the access token being refreshed, outside mu
	checked    map[string]checkedFolder // cached folder ID -> when it was last seen in its parent
}

// A refresh of the access token, which others needing one wait for.
type refresh struct {
	done chan struct{}
	err  error
}

// Google not answering never holds anything up for long (review #149): connecting, and the answer
// to a request once it is sent, have time limits; a body that stops moving either way is given up
// (stallAfter); refreshing the token has its own limit.
const (
	connectTimeout = 30 * time.Second
	answerTimeout  = 2 * time.Minute
)

var (
	stallAfter     = 2 * time.Minute // variables for the tests
	refreshTimeout = 30 * time.Second
)

func New(d *sql.DB, redirectURI string) *Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = connectTimeout
	t.ResponseHeaderTimeout = answerTimeout
	return &Client{db: d, redirectURI: redirectURI, http: &http.Client{Transport: t}}
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

// bearer returns a valid access token, refreshing it when it is about to expire. The refresh is
// one for everyone needing it, and runs outside mu: Status and the rest never wait for Google.
func (c *Client) bearer(ctx context.Context, forceRefresh bool) (string, error) {
	for {
		c.mu.Lock()
		if err := c.loadLocked(ctx); err != nil {
			c.mu.Unlock()
			return "", err
		}
		if c.cfg == nil || c.tok == nil {
			c.mu.Unlock()
			return "", ErrNotConnected
		}
		if !forceRefresh && time.Until(c.tok.Expiry) >= time.Minute {
			auth := "Bearer " + c.tok.AccessToken
			c.mu.Unlock()
			return auth, nil
		}
		if r := c.refreshing; r != nil {
			c.mu.Unlock()
			select {
			case <-r.done:
			case <-ctx.Done():
				return "", ctx.Err()
			}
			if r.err != nil {
				return "", r.err
			}
			forceRefresh = false // refreshed just now
			continue
		}
		r := &refresh{done: make(chan struct{})}
		c.refreshing = r
		cfg, tok := *c.cfg, c.tok
		c.mu.Unlock()
		r.err = c.refresh(ctx, cfg, tok)
		c.mu.Lock()
		c.refreshing = nil
		c.mu.Unlock()
		close(r.done)
		if r.err != nil {
			return "", r.err
		}
		forceRefresh = false
	}
}

// refresh gets a new access token for tok, and keeps it unless the token was replaced meanwhile
// (connected again). It is not the asker's to cancel: others may be waiting for it.
func (c *Client) refresh(ctx context.Context, cfg ClientConfig, tok *Token) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()
	t, fields, err := c.exchange(ctx, cfg, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {tok.RefreshToken},
	})
	if err != nil {
		var te *tokenError
		if errors.As(err, &te) && te.code == "invalid_grant" {
			return fmt.Errorf("refresh access token: %w (%v)", ErrAuthExpired, err)
		}
		return fmt.Errorf("refresh access token: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok != tok {
		return nil // replaced: the next turn uses the new one
	}
	next := *tok
	next.AccessToken, next.Expiry = t.AccessToken, t.Expiry
	if t.RefreshToken != "" {
		next.RefreshToken = t.RefreshToken
	}
	if _, ok := fields["refresh_token_expires_in"]; ok {
		next.RefreshTokenExpiresIn = t.RefreshTokenExpiresIn
	}
	if err := c.writeCredential(ctx, credToken, &next); err != nil {
		return err
	}
	c.tok = &next
	return nil
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

// Explain names why reading from Drive failed, for a player to say (review #167): not_connected,
// auth_expired, gone (the file is no longer there), or drive (anything else); lasting when trying
// again soon cannot help.
func Explain(err error) (reason string, lasting bool) {
	switch {
	case errors.Is(err, ErrNotConnected):
		return "not_connected", true
	case errors.Is(err, ErrAuthExpired):
		return "auth_expired", true
	case IsNotFound(err):
		return "gone", true
	}
	return "drive", false
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
		w := watch(ctx)
		req, err := http.NewRequestWithContext(w.ctx, method, rawURL, rd)
		if err != nil {
			w.end()
			return nil, err
		}
		for k, v := range header {
			req.Header[k] = v
		}
		req.Header.Set("Authorization", auth)
		resp, err := c.http.Do(req)
		if err != nil {
			w.end()
			if ctx.Err() != nil || attempt >= 4 {
				return nil, err
			}
			if !sleepCtx(ctx, backoff(attempt)) {
				return nil, ctx.Err()
			}
			continue
		}
		if resp.StatusCode < 400 {
			resp.Body = w.body(resp.Body)
			return resp, nil
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		w.end()
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

// ErrStalled is a request whose body stopped moving, either way, for stallAfter.
var ErrStalled = errors.New("google drive stopped sending or taking data")

// watched is a request's context, which ends when its body makes no progress for stallAfter: the
// response's while it is read (not the time between reads, which is the reader's), or the
// request's while the transport takes it.
type watched struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	timer  *time.Timer
}

func watch(ctx context.Context) *watched {
	ctx, cancel := context.WithCancelCause(ctx)
	w := &watched{ctx: ctx, cancel: cancel}
	w.timer = time.AfterFunc(stallAfter, func() { cancel(ErrStalled) })
	w.timer.Stop() // until a body moves; the transport's own limits cover the rest
	return w
}

func (w *watched) end() {
	w.timer.Stop()
	w.cancel(nil)
}

// stalled turns the error of a body that stopped into ErrStalled.
func (w *watched) stalled(err error) error {
	if err != nil && errors.Is(context.Cause(w.ctx), ErrStalled) {
		return ErrStalled
	}
	return err
}

// body watches a response body as it is read; closing it ends the request.
func (w *watched) body(rc io.ReadCloser) io.ReadCloser { return &watchedBody{rc, w} }

type watchedBody struct {
	rc io.ReadCloser
	w  *watched
}

func (b *watchedBody) Read(p []byte) (int, error) {
	b.w.timer.Reset(stallAfter)
	n, err := b.rc.Read(p)
	b.w.timer.Stop()
	return n, b.w.stalled(err)
}

func (b *watchedBody) Close() error {
	err := b.rc.Close()
	b.w.end()
	return err
}

// sending watches a request body as the transport takes it: the time between its reads is the
// time the connection would not take more.
func (w *watched) sending(r io.Reader) io.Reader { return &sendingBody{r, w} }

type sendingBody struct {
	r io.Reader
	w *watched
}

func (b *sendingBody) Read(p []byte) (int, error) {
	b.w.timer.Stop()
	n, err := b.r.Read(p)
	if err == nil {
		b.w.timer.Reset(stallAfter)
	}
	return n, err
}

// UseHTTPClient replaces the HTTP client before the client is used, for tests that answer
// Google's endpoints locally.
func (c *Client) UseHTTPClient(h *http.Client) { c.http = h }
