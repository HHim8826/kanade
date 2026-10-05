package bangumi

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
)

// Accounts are the Bangumi accounts Kanade's accounts are linked to (review #94), so that their
// owners manage their collections from the library: what they ask to change is written to
// Bangumi, nothing else.

const (
	appIDKey     = "bangumi.app_id"
	appSecretKey = "bangumi.app_secret"
)

var (
	ErrInvalid   = errors.New("invalid")
	ErrNotLinked = errors.New("not linked to Bangumi")
	ErrState     = errors.New("this link request is unknown or expired: start again from Kanade's settings page")
)

// Link is a Kanade account's link to a Bangumi account.
type Link struct {
	User     int64  `json:"-"`
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Nickname string `json:"nickname"`
	Access   string `json:"-"`
	Refresh  string `json:"-"`
	Expires  int64  `json:"-"`
	LinkedAt int64  `json:"linked_at"`
	Error    string `json:"error,omitempty"`
}

type Accounts struct {
	DB        *sql.DB
	Client    *Client
	PublicURL string

	mu      sync.Mutex
	pending map[string]pending
	tags    map[int64]cachedTags // a person's own tags, from their music collections
}

type pending struct {
	user    int64
	expires time.Time
}

type cachedTags struct {
	tags []string
	at   time.Time
}

func NewAccounts(d *sql.DB, c *Client, publicURL string) *Accounts {
	return &Accounts{DB: d, Client: c, PublicURL: strings.TrimRight(publicURL, "/"), pending: map[string]pending{}, tags: map[int64]cachedTags{}}
}

// RedirectURI is where Bangumi sends the person back: the application's callback address.
func (a *Accounts) RedirectURI() string { return a.PublicURL + "/oauth/bangumi/callback" }

func (a *Accounts) App(ctx context.Context) App {
	id, _ := db.GetSetting(ctx, a.DB, appIDKey)
	secret, _ := db.GetSetting(ctx, a.DB, appSecretKey)
	return App{ID: id, Secret: secret, RedirectURI: a.RedirectURI()}
}

// SetApp sets the Bangumi application; an empty secret keeps the one set.
func (a *Accounts) SetApp(ctx context.Context, id, secret string) error {
	id, secret = strings.TrimSpace(id), strings.TrimSpace(secret)
	if len(id) > 100 || len(secret) > 200 || strings.ContainsAny(id+secret, " \t\n") {
		return fmt.Errorf("%w: the App ID or App Secret is not right", ErrInvalid)
	}
	if err := db.SetSetting(ctx, a.DB, appIDKey, id); err != nil {
		return err
	}
	if secret != "" || id == "" {
		return db.SetSetting(ctx, a.DB, appSecretKey, secret)
	}
	return nil
}

func (a *Accounts) Link(ctx context.Context, user int64) (*Link, error) {
	var l Link
	err := a.DB.QueryRowContext(ctx, `SELECT user_id, bgm_id, username, nickname, access_token, refresh_token, expires_at, linked_at, error
		FROM bangumi_links WHERE user_id = ?`, user).Scan(&l.User, &l.ID, &l.Username, &l.Nickname, &l.Access, &l.Refresh, &l.Expires, &l.LinkedAt, &l.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &l, err
}

// Begin starts linking: the address to send the person to.
func (a *Accounts) Begin(ctx context.Context, user int64) (string, error) {
	app := a.App(ctx)
	if !app.Ready() {
		return "", fmt.Errorf("%w: set the Bangumi application's App ID and App Secret first", ErrInvalid)
	}
	b := make([]byte, 24)
	rand.Read(b)
	state := base64.RawURLEncoding.EncodeToString(b)
	a.mu.Lock()
	now := time.Now()
	for k, p := range a.pending {
		if now.After(p.expires) {
			delete(a.pending, k)
		}
	}
	a.pending[state] = pending{user: user, expires: now.Add(10 * time.Minute)}
	a.mu.Unlock()
	return a.Client.AuthorizeURL(app, state), nil
}

// Finish completes linking with what Bangumi sent back.
func (a *Accounts) Finish(ctx context.Context, state, code string) (int64, error) {
	a.mu.Lock()
	p, ok := a.pending[state]
	delete(a.pending, state)
	a.mu.Unlock()
	if !ok || time.Now().After(p.expires) {
		return 0, ErrState
	}
	tok, err := a.Client.Exchange(ctx, a.App(ctx), code)
	if err != nil {
		return p.user, err
	}
	me, err := a.Client.Me(ctx, tok.Access)
	if err != nil {
		return p.user, err
	}
	_, err = a.DB.ExecContext(ctx, `INSERT INTO bangumi_links (user_id, bgm_id, username, nickname, access_token, refresh_token, expires_at, linked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET bgm_id = excluded.bgm_id, username = excluded.username, nickname = excluded.nickname,
			access_token = excluded.access_token, refresh_token = excluded.refresh_token, expires_at = excluded.expires_at,
			linked_at = excluded.linked_at, error = ''`,
		p.user, me.ID, me.Username, me.Nickname, tok.Access, tok.Refresh, tok.Expires.UnixMilli(), db.Now())
	a.mu.Lock()
	delete(a.tags, p.user)
	a.mu.Unlock()
	return p.user, err
}

// Unlink forgets the link (Bangumi has no way to take a token back: it expires within a week).
func (a *Accounts) Unlink(ctx context.Context, user int64) error {
	_, err := a.DB.ExecContext(ctx, `DELETE FROM bangumi_links WHERE user_id = ?`, user)
	a.mu.Lock()
	delete(a.tags, user)
	a.mu.Unlock()
	return err
}

// Session is a linked person, for asking Bangumi as them.
type Session struct {
	Access, Username string
}

// Session is how to ask Bangumi as a user's linked person, its token renewed when near its end;
// ErrNotLinked when there is no working link (a refused one is marked so).
func (a *Accounts) Session(ctx context.Context, user int64) (*Session, error) {
	l, err := a.Link(ctx, user)
	if err != nil {
		return nil, err
	}
	if l == nil || l.Error != "" {
		return nil, ErrNotLinked
	}
	if time.Until(time.UnixMilli(l.Expires)) > 24*time.Hour {
		return &Session{Access: l.Access, Username: l.Username}, nil
	}
	tok, err := a.Client.Refresh(ctx, a.App(ctx), l.Refresh)
	if errors.Is(err, ErrAuth) {
		a.Failed(user)
		return nil, ErrNotLinked
	}
	if err != nil {
		if time.Now().Before(time.UnixMilli(l.Expires)) {
			return &Session{Access: l.Access, Username: l.Username}, nil // still good a while: renewed next time
		}
		return nil, err
	}
	if _, err := a.DB.ExecContext(ctx, `UPDATE bangumi_links SET access_token = ?, refresh_token = ?, expires_at = ? WHERE user_id = ?`,
		tok.Access, tok.Refresh, tok.Expires.UnixMilli(), user); err != nil {
		return nil, err
	}
	return &Session{Access: tok.Access, Username: l.Username}, nil
}

// Failed marks a user's link as one Bangumi no longer accepts.
func (a *Accounts) Failed(user int64) {
	a.DB.Exec(`UPDATE bangumi_links SET error = ? WHERE user_id = ?`, "Bangumi 不再接受這個連結（授權過期或被撤銷），請重新連結。", user)
}

// MyTags are the tags a person put on their music collections, the most used first (kept ten
// minutes).
func (a *Accounts) MyTags(ctx context.Context, user int64, s *Session) ([]string, error) {
	a.mu.Lock()
	if c, ok := a.tags[user]; ok && time.Since(c.at) < 10*time.Minute {
		a.mu.Unlock()
		return c.tags, nil
	}
	a.mu.Unlock()
	count := map[string]int{}
	for offset := 0; offset < 500; offset += 50 {
		p, err := a.Client.Collections(ctx, s.Access, s.Username, Music, 0, 50, offset)
		if err != nil {
			return nil, err
		}
		for _, c := range p.Data {
			for _, t := range c.Tags {
				count[t]++
			}
		}
		if offset+50 >= p.Total {
			break
		}
	}
	tags := make([]string, 0, len(count))
	for t := range count {
		tags = append(tags, t)
	}
	sort.Slice(tags, func(i, j int) bool {
		if count[tags[i]] != count[tags[j]] {
			return count[tags[i]] > count[tags[j]]
		}
		return tags[i] < tags[j]
	})
	if len(tags) > 30 {
		tags = tags[:30]
	}
	a.mu.Lock()
	a.tags[user] = cachedTags{tags, time.Now()}
	a.mu.Unlock()
	return tags, nil
}

// Changed drops what is kept of a person's tags (they changed a collection).
func (a *Accounts) Changed(user int64) {
	a.mu.Lock()
	delete(a.tags, user)
	a.mu.Unlock()
}
