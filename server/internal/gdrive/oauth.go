package gdrive

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
)

const (
	authEndpoint = "https://accounts.google.com/o/oauth2/v2/auth"
	DriveScope   = "https://www.googleapis.com/auth/drive"
	stateTTL     = time.Hour
)

// tokenEndpoint is a variable so tests can point it at a fake server.
var tokenEndpoint = "https://oauth2.googleapis.com/token"

var ErrBadState = errors.New("unknown or expired authorization state; start again")

// CallbackPath is where Google sends the administrator's browser back to the service.
const CallbackPath = "/oauth/google/callback"

// LocalhostRedirect is the redirect for a service Google will not send the browser back to:
// Google takes only https redirects to a domain, or http to localhost. It leads nowhere; the
// administrator pastes the address the browser ends on (CompletePasted).
const LocalhostRedirect = "http://localhost" + CallbackPath

// RedirectFor is the OAuth redirect for a service reached at publicURL: its own callback when
// that has https and a domain name (or is on this computer), LocalhostRedirect otherwise (plain
// http, or an IP address).
func RedirectFor(publicURL string) string {
	base := strings.TrimRight(publicURL, "/")
	u, err := url.Parse(base)
	if err != nil {
		return LocalhostRedirect
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	switch {
	case host == "localhost" || ip != nil && ip.IsLoopback():
		return base + CallbackPath
	case u.Scheme == "https" && ip == nil && strings.Contains(host, "."):
		return base + CallbackPath
	}
	return LocalhostRedirect
}

// tokenError is an error answer from the token endpoint, such as invalid_grant.
type tokenError struct {
	status      int
	code, about string
}

func (e *tokenError) Error() string {
	return strings.TrimSpace(fmt.Sprintf("token endpoint %d: %s %s", e.status, e.code, e.about))
}

// exchange calls the token endpoint; it also returns the response's fields so callers
// can see whether refresh_token_expires_in was present. c.mu must be held or cfg stable.
func (c *Client) exchange(ctx context.Context, form url.Values) (Token, map[string]any, error) {
	if c.cfg == nil {
		return Token{}, nil, errors.New("no OAuth client configured")
	}
	form.Set("client_id", c.cfg.ClientID)
	form.Set("client_secret", c.cfg.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return Token{}, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return Token{}, nil, fmt.Errorf("token endpoint %d: unreadable response", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		code, _ := m["error"].(string)
		about, _ := m["error_description"].(string)
		return Token{}, nil, &tokenError{resp.StatusCode, code, about}
	}
	str := func(k string) string { s, _ := m[k].(string); return s }
	num := func(k string) int { n, _ := m[k].(float64); return int(n) }
	now := time.Now()
	return Token{
		AccessToken:           str("access_token"),
		RefreshToken:          str("refresh_token"),
		Scope:                 str("scope"),
		Expiry:                now.Add(time.Duration(num("expires_in")) * time.Second),
		ObtainedAt:            now,
		RefreshTokenExpiresIn: num("refresh_token_expires_in"),
	}, m, nil
}

// AuthURL starts an authorization. The returned URL is opened in the administrator's browser;
// Google then redirects to the callback with a single-use state stored here.
func (c *Client) AuthURL(ctx context.Context, loginHint string) (string, error) {
	c.mu.Lock()
	err := c.loadLocked(ctx)
	cfg := c.cfg
	c.mu.Unlock()
	if err != nil {
		return "", err
	}
	if cfg == nil {
		return "", errors.New("no OAuth client configured")
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	state := hex.EncodeToString(raw)
	now := db.Now()
	if _, err := c.db.ExecContext(ctx, `DELETE FROM oauth_states WHERE created_at < ?`, now-stateTTL.Milliseconds()); err != nil {
		return "", err
	}
	if _, err := c.db.ExecContext(ctx, `INSERT INTO oauth_states (state, created_at) VALUES (?, ?)`, state, now); err != nil {
		return "", err
	}
	q := url.Values{
		"client_id":     {cfg.ClientID},
		"redirect_uri":  {c.redirectURI},
		"response_type": {"code"},
		"scope":         {DriveScope},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
		"state":         {state},
	}
	if loginHint != "" {
		q.Set("login_hint", loginHint)
	}
	return authEndpoint + "?" + q.Encode(), nil
}

// consumeState deletes the state and reports whether it was valid.
func (c *Client) consumeState(ctx context.Context, state string) error {
	var created int64
	err := c.db.QueryRowContext(ctx, `DELETE FROM oauth_states WHERE state = ? RETURNING created_at`, state).Scan(&created)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrBadState
	}
	if err != nil {
		return err
	}
	if db.Now()-created > stateTTL.Milliseconds() {
		return ErrBadState
	}
	return nil
}

// Complete handles the callback query (state, code or error) and stores the token.
func (c *Client) Complete(ctx context.Context, q url.Values) (Status, error) {
	if err := c.consumeState(ctx, q.Get("state")); err != nil {
		return Status{}, err
	}
	if e := q.Get("error"); e != "" {
		return Status{}, fmt.Errorf("authorization was not granted: %s", e)
	}
	code := q.Get("code")
	if code == "" {
		return Status{}, errors.New("callback has no code")
	}
	c.mu.Lock()
	if err := c.loadLocked(ctx); err != nil {
		c.mu.Unlock()
		return Status{}, err
	}
	tok, _, err := c.exchange(ctx, url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {c.redirectURI},
	})
	c.mu.Unlock()
	if err != nil {
		return Status{}, err
	}
	if !strings.Contains(" "+tok.Scope+" ", " "+DriveScope+" ") {
		return Status{}, errors.New("google did not grant Drive access; tick the Drive permission on the consent screen")
	}
	if err := c.ImportToken(ctx, tok); err != nil {
		return Status{}, err
	}
	return c.Status(ctx)
}

// CompletePasted finishes an authorization from a callback URL copied out of the browser,
// for when the callback could not reach the service.
func (c *Client) CompletePasted(ctx context.Context, rawURL string) (Status, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return Status{}, err
	}
	return c.Complete(ctx, u.Query())
}
