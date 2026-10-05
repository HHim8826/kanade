package discord

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/presence"
)

const (
	clientIDKey = "discord.client_id"
	secretKey   = "discord.client_secret"
	imageKey    = "discord.image" // an asset of the application, or an https address
)

// Statuses a link can show while it shows something.
var Statuses = []string{"online", "idle", "dnd"}

type Link struct {
	User       int64         `json:"-"`
	DiscordID  string        `json:"discord_id"`
	Name       string        `json:"name"`
	Access     string        `json:"-"`
	Refresh    string        `json:"-"`
	Expires    int64         `json:"-"`
	Follow     string        `json:"follow"`
	FollowName string        `json:"follow_name"`
	Show       presence.Show `json:"show"`
	Status     string        `json:"status"`
	LinkedAt   int64         `json:"linked_at"`
	Error      string        `json:"error,omitempty"`
}

// State is how a link is doing now.
type State struct {
	Connected bool   `json:"connected"` // to Discord, showing something
	Showing   string `json:"showing,omitempty"`
	Error     string `json:"error,omitempty"`
}

type Service struct {
	DB        *sql.DB
	Hub       *presence.Hub
	OAuth     *OAuth
	Gateway   *Gateway
	Log       *slog.Logger
	PublicURL string
	Grace     time.Duration // how long a connection stays once there is nothing to show

	mu      sync.Mutex
	ctx     context.Context // Run's; nil until it runs
	follows map[int64]context.CancelFunc
	states  map[int64]State
	pending map[string]pendingLink // OAuth states, to a user
}

type pendingLink struct {
	user    int64
	expires time.Time
}

func New(d *sql.DB, hub *presence.Hub, publicURL string, log *slog.Logger) *Service {
	return &Service{DB: d, Hub: hub, OAuth: NewOAuth(), Gateway: NewGateway(), Log: log, PublicURL: strings.TrimRight(publicURL, "/"),
		Grace: 2 * time.Minute, follows: map[int64]context.CancelFunc{}, states: map[int64]State{}, pending: map[string]pendingLink{}}
}

// RedirectURI is where Discord sends the person back: it goes in the application's OAuth2 redirects.
func (s *Service) RedirectURI() string { return s.PublicURL + "/oauth/discord/callback" }

func (s *Service) App(ctx context.Context) App {
	id, _ := db.GetSetting(ctx, s.DB, clientIDKey)
	secret, _ := db.GetSetting(ctx, s.DB, secretKey)
	return App{ClientID: id, Secret: secret, RedirectURI: s.RedirectURI()}
}

func (s *Service) Image(ctx context.Context) string {
	v, _ := db.GetSetting(ctx, s.DB, imageKey)
	return v
}

// SetApp sets the Discord application; an empty secret keeps the one set.
func (s *Service) SetApp(ctx context.Context, clientID, secret, image string) error {
	clientID, secret, image = strings.TrimSpace(clientID), strings.TrimSpace(secret), strings.TrimSpace(image)
	if clientID != "" && (len(clientID) < 15 || len(clientID) > 22 || strings.Trim(clientID, "0123456789") != "") {
		return fmt.Errorf("%w: the client ID is the number Discord's developer portal shows", ErrInvalid)
	}
	if len(secret) > 200 || len(image) > 256 || strings.HasPrefix(image, "http") && !strings.HasPrefix(image, "https://") {
		return fmt.Errorf("%w: the secret or the picture is not right", ErrInvalid)
	}
	if err := db.SetSetting(ctx, s.DB, clientIDKey, clientID); err != nil {
		return err
	}
	if secret != "" || clientID == "" {
		if err := db.SetSetting(ctx, s.DB, secretKey, secret); err != nil {
			return err
		}
	}
	if err := db.SetSetting(ctx, s.DB, imageKey, image); err != nil {
		return err
	}
	s.restartAll()
	return nil
}

var ErrInvalid = errors.New("invalid")

// ---- links ----

const linkCols = `user_id, discord_id, discord_name, access_token, refresh_token, expires_at, follow, follow_name, show, status, linked_at, error`

func scanLink(row interface{ Scan(...any) error }) (*Link, error) {
	var l Link
	var show string
	if err := row.Scan(&l.User, &l.DiscordID, &l.Name, &l.Access, &l.Refresh, &l.Expires, &l.Follow, &l.FollowName, &show, &l.Status, &l.LinkedAt, &l.Error); err != nil {
		return nil, err
	}
	l.Show = presence.DefaultShow
	json.Unmarshal([]byte(show), &l.Show)
	return &l, nil
}

// Link is a user's link, or nil.
func (s *Service) Link(ctx context.Context, user int64) (*Link, error) {
	l, err := scanLink(s.DB.QueryRowContext(ctx, `SELECT `+linkCols+` FROM discord_links WHERE user_id = ?`, user))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return l, err
}

// Linked says whether a user's playing is shown: web players tell only then.
func (s *Service) Linked(ctx context.Context, user int64) bool {
	var one int
	return s.DB.QueryRowContext(ctx, `SELECT 1 FROM discord_links WHERE user_id = ? AND error = ''`, user).Scan(&one) == nil
}

// Begin starts linking: the address to send the person to.
func (s *Service) Begin(ctx context.Context, user int64) (string, error) {
	app := s.App(ctx)
	if !app.Ready() {
		return "", fmt.Errorf("%w: set the Discord application's client ID and secret first", ErrInvalid)
	}
	b := make([]byte, 24)
	rand.Read(b)
	state := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	now := time.Now()
	for k, p := range s.pending {
		if now.After(p.expires) {
			delete(s.pending, k)
		}
	}
	s.pending[state] = pendingLink{user: user, expires: now.Add(10 * time.Minute)}
	s.mu.Unlock()
	return s.OAuth.AuthorizeURL(app, state), nil
}

var (
	ErrState = errors.New("this link request is unknown or expired: start again from Kanade's settings page")
	// ErrScope is a link without the presence permission: the application has no Social SDK.
	ErrScope = errors.New("Discord did not grant the presence permission; turn on the Social SDK for the application")
)

// Finish completes linking with what Discord sent back, and starts showing; it says whose it was.
func (s *Service) Finish(ctx context.Context, state, code string) (int64, error) {
	s.mu.Lock()
	p, ok := s.pending[state]
	delete(s.pending, state)
	s.mu.Unlock()
	if !ok || time.Now().After(p.expires) {
		return 0, ErrState
	}
	app := s.App(ctx)
	tok, err := s.OAuth.Exchange(ctx, app, code)
	if err != nil {
		return p.user, err
	}
	if !strings.Contains(tok.Scope, "sdk.social_layer_presence") {
		return p.user, ErrScope
	}
	id, name, err := s.OAuth.Me(ctx, tok.Access)
	if err != nil {
		return p.user, err
	}
	show, _ := json.Marshal(presence.DefaultShow)
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO discord_links (user_id, discord_id, discord_name, access_token, refresh_token, expires_at, show, linked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET discord_id = excluded.discord_id, discord_name = excluded.discord_name, access_token = excluded.access_token,
			refresh_token = excluded.refresh_token, expires_at = excluded.expires_at, linked_at = excluded.linked_at, error = ''`,
		p.user, id, name, tok.Access, tok.Refresh, tok.Expires.UnixMilli(), string(show), db.Now()); err != nil {
		return p.user, err
	}
	s.Start(p.user)
	return p.user, nil
}

// Change sets what a link follows and shows (nil: unchanged).
func (s *Service) Change(ctx context.Context, user int64, follow, followName *string, show *presence.Show, status *string) error {
	l, err := s.Link(ctx, user)
	if err != nil {
		return err
	}
	if l == nil {
		return ErrNotLinked
	}
	if follow != nil {
		l.Follow, l.FollowName = clip(*follow, 64), ""
		if followName != nil {
			l.FollowName = clip(*followName, 80)
		}
	}
	if show != nil {
		if show.Paused != "show" {
			show.Paused = "clear"
		}
		l.Show = *show
	}
	if status != nil {
		ok := false
		for _, v := range Statuses {
			ok = ok || v == *status
		}
		if !ok {
			return fmt.Errorf("%w: status is online, idle or dnd", ErrInvalid)
		}
		l.Status = *status
	}
	b, _ := json.Marshal(l.Show)
	if _, err := s.DB.ExecContext(ctx, `UPDATE discord_links SET follow = ?, follow_name = ?, show = ?, status = ? WHERE user_id = ?`,
		l.Follow, l.FollowName, string(b), l.Status, user); err != nil {
		return err
	}
	s.Hub.Poke(user)
	return nil
}

var ErrNotLinked = errors.New("not linked to Discord")

// Unlink stops showing, takes the token back from Discord and forgets the link.
func (s *Service) Unlink(ctx context.Context, user int64) error {
	l, err := s.Link(ctx, user)
	if err != nil || l == nil {
		return err
	}
	s.stop(user)
	if app := s.App(ctx); app.Ready() {
		if err := s.OAuth.Revoke(ctx, app, l.Refresh); err != nil {
			s.Log.Info("discord: revoking", "err", err)
		}
	}
	_, err = s.DB.ExecContext(ctx, `DELETE FROM discord_links WHERE user_id = ?`, user)
	s.mu.Lock()
	delete(s.states, user)
	s.mu.Unlock()
	return err
}

func (s *Service) setError(user int64, msg string) {
	s.DB.Exec(`UPDATE discord_links SET error = ? WHERE user_id = ?`, clip(msg, 300), user)
	s.setState(user, func(st *State) { st.Connected, st.Error = false, msg })
}

func (s *Service) setState(user int64, f func(*State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.states[user]
	f(&st)
	s.states[user] = st
}

// StateOf is how a user's link is doing.
func (s *Service) StateOf(user int64) State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.states[user]
}

// token is the link's access token, renewed when it is near its end.
func (s *Service) token(ctx context.Context, user int64) (string, error) {
	l, err := s.Link(ctx, user)
	if err != nil {
		return "", err
	}
	if l == nil {
		return "", ErrNotLinked
	}
	if time.Until(time.UnixMilli(l.Expires)) > time.Hour {
		return l.Access, nil
	}
	tok, err := s.OAuth.Refresh(ctx, s.App(ctx), l.Refresh)
	if err != nil {
		return "", err
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE discord_links SET access_token = ?, refresh_token = ?, expires_at = ? WHERE user_id = ?`,
		tok.Access, tok.Refresh, tok.Expires.UnixMilli(), user); err != nil {
		return "", err
	}
	return tok.Access, nil
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// ---- following what plays ----

// Run shows the linked users' playing until ctx ends.
func (s *Service) Run(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	rows, err := s.DB.QueryContext(ctx, `SELECT user_id FROM discord_links WHERE error = ''`)
	if err != nil {
		s.Log.Warn("discord: links", "err", err)
		return
	}
	var users []int64
	for rows.Next() {
		var u int64
		if rows.Scan(&u) == nil {
			users = append(users, u)
		}
	}
	rows.Close()
	for _, u := range users {
		s.Start(u)
	}
	<-ctx.Done()
}

// Start (re)starts following a user's playing, once Run runs.
func (s *Service) Start(user int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil {
		return
	}
	if stop := s.follows[user]; stop != nil {
		stop()
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.follows[user] = cancel
	s.states[user] = State{}
	go s.follow(ctx, user)
}

func (s *Service) stop(user int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if stop := s.follows[user]; stop != nil {
		stop()
		delete(s.follows, user)
	}
}

func (s *Service) restartAll() {
	s.mu.Lock()
	users := make([]int64, 0, len(s.follows))
	for u := range s.follows {
		users = append(users, u)
	}
	s.mu.Unlock()
	for _, u := range users {
		s.Start(u)
	}
}

// follow keeps Discord showing what a user plays: a connection opens when there is something to
// show and closes a while after there is none, so Kanade does not keep the person online.
func (s *Service) follow(ctx context.Context, user int64) {
	changes, unwatch := s.Hub.Watch(user)
	defer unwatch()
	var conn context.CancelFunc
	var done chan error
	want := newLatest()
	grace := time.NewTimer(time.Hour)
	grace.Stop()
	defer grace.Stop()
	closeConn := func() {
		if conn != nil {
			conn()
			<-done
			conn, done = nil, nil
		}
		s.setState(user, func(st *State) { st.Connected, st.Showing = false, "" })
	}
	defer closeConn()
	last := ""
	for {
		l, err := s.Link(ctx, user)
		if err != nil || l == nil || l.Error != "" {
			return
		}
		p := s.Hub.Pick(user, l.Follow)
		shown := presence.ShownBy(p, l.Show)
		act := Activity(shown, s.Image(ctx), time.Now())
		key, _ := json.Marshal([]any{shown.State, shown.Title, shown.Artist, shown.Album, shown.DurationMS > 0, l.Status})
		if p != nil {
			key = fmt.Appendf(key, "|%d|%s", p.Changed, p.Player)
		}
		if string(key) != last {
			last = string(key)
			want.set(Presence{Status: l.Status, Activity: act})
			s.setState(user, func(st *State) { st.Showing = shown.Title })
		}
		if act != nil {
			grace.Stop()
			if conn == nil {
				cctx, cancel := context.WithCancel(ctx)
				conn, done = cancel, make(chan error, 1)
				go func() { done <- s.connection(cctx, user, want) }()
			}
		} else if conn != nil {
			grace.Reset(s.Grace)
		}
		select {
		case <-ctx.Done():
			return
		case <-changes:
		case <-grace.C:
			closeConn()
		case err := <-done:
			conn, done = nil, nil
			if errors.Is(err, ErrAuth) {
				s.setError(user, "Discord 不再接受這個連結（授權被撤銷或已失效），請重新連結。")
				return
			}
		}
	}
}

// connection keeps a connection to Discord's gateway for a user, connecting again when it drops,
// until ctx ends (nil) or Discord refuses the link (ErrAuth).
func (s *Service) connection(ctx context.Context, user int64, want *latest) error {
	backoff := 5 * time.Second
	for {
		token, err := s.token(ctx, user)
		if errors.Is(err, ErrAuth) || errors.Is(err, ErrNotLinked) {
			return ErrAuth
		}
		if err == nil {
			err = s.Gateway.connect(ctx, token, want, func() {
				backoff = 5 * time.Second
				s.setState(user, func(st *State) { st.Connected, st.Error = true, "" })
			})
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, ErrAuth) {
				// Refused: renewed once, else the link is no good.
				s.DB.Exec(`UPDATE discord_links SET expires_at = 0 WHERE user_id = ?`, user)
				if _, rerr := s.token(ctx, user); rerr != nil {
					return ErrAuth
				}
				continue
			}
		}
		s.setState(user, func(st *State) { st.Connected, st.Error = false, err.Error() })
		if !errors.Is(err, errReconnect) {
			s.Log.Info("discord: connection", "user", user, "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
}
