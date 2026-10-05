// Package discord shows what an account plays as its owner's Discord status (review #135), from the
// server: the owner links their Discord account once (OAuth, with the presence scope that Discord's
// Social SDK uses, which the Discord application must have turned on), and while a song plays the
// server keeps a connection to Discord's gateway as that person and sets the activity. Nothing runs
// on the user's computer or phone, and no Discord password or account token is ever asked for.
package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Scopes are what the link asks for: who the person is, and setting their presence.
const Scopes = "openid sdk.social_layer_presence identify"

var (
	// ErrAuth is a link Discord no longer accepts: it must be made again.
	ErrAuth        = errors.New("Discord no longer accepts this link")
	ErrUnavailable = errors.New("Discord did not answer; try again in a minute")
)

// App is the Discord application the link is made with.
type App struct {
	ClientID, Secret, RedirectURI string
}

func (a App) Ready() bool { return a.ClientID != "" && a.Secret != "" && a.RedirectURI != "" }

type Token struct {
	Access, Refresh string
	Expires         time.Time
	Scope           string
}

// OAuth speaks to Discord's web API.
type OAuth struct {
	Site string // https://discord.com
	API  string // https://discord.com/api/v10
	HTTP *http.Client
}

func NewOAuth() *OAuth {
	return &OAuth{Site: "https://discord.com", API: "https://discord.com/api/v10", HTTP: &http.Client{Timeout: 20 * time.Second}}
}

// AuthorizeURL is where the person agrees to the link.
func (o *OAuth) AuthorizeURL(app App, state string) string {
	q := url.Values{"client_id": {app.ClientID}, "response_type": {"code"}, "redirect_uri": {app.RedirectURI}, "scope": {Scopes},
		"state": {state}, "prompt": {"consent"}}
	return o.Site + "/oauth2/authorize?" + q.Encode()
}

func (o *OAuth) token(ctx context.Context, app App, form url.Values) (*Token, error) {
	form.Set("client_id", app.ClientID)
	form.Set("client_secret", app.Secret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.API+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var r struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
		Error        string `json:"error"`
		Description  string `json:"error_description"`
	}
	json.Unmarshal(body, &r)
	switch {
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized:
		if r.Error == "" {
			r.Error = resp.Status
		}
		return nil, fmt.Errorf("%w: %s %s", ErrAuth, r.Error, r.Description)
	case resp.StatusCode != http.StatusOK || r.AccessToken == "":
		return nil, fmt.Errorf("%w (HTTP %d)", ErrUnavailable, resp.StatusCode)
	}
	return &Token{Access: r.AccessToken, Refresh: r.RefreshToken, Expires: time.Now().Add(time.Duration(r.ExpiresIn) * time.Second), Scope: r.Scope}, nil
}

// Exchange trades the code Discord sent back for tokens.
func (o *OAuth) Exchange(ctx context.Context, app App, code string) (*Token, error) {
	return o.token(ctx, app, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {app.RedirectURI}})
}

// Refresh renews the tokens before they expire.
func (o *OAuth) Refresh(ctx context.Context, app App, refresh string) (*Token, error) {
	return o.token(ctx, app, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
}

// Revoke takes a token back (unlinking).
func (o *OAuth) Revoke(ctx context.Context, app App, token string) error {
	form := url.Values{"token": {token}, "client_id": {app.ClientID}, "client_secret": {app.Secret}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.API+"/oauth2/token/revoke", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("revoking: HTTP %d", resp.StatusCode)
	}
	return nil
}

// Me is the linked person: their Discord ID and the name to show for them.
func (o *OAuth) Me(ctx context.Context, access string) (id, name string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.API+"/users/@me", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("%w (%v)", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", "", ErrAuth
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("%w (HTTP %d)", ErrUnavailable, resp.StatusCode)
	}
	var u struct {
		ID         string `json:"id"`
		Username   string `json:"username"`
		GlobalName string `json:"global_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&u); err != nil || u.ID == "" {
		return "", "", fmt.Errorf("%w (an answer that is no user)", ErrUnavailable)
	}
	name = u.Username
	if u.GlobalName != "" && u.GlobalName != u.Username {
		name = u.GlobalName + "（@" + u.Username + "）"
	}
	return u.ID, name, nil
}
