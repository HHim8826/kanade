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
	"slices"
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
	// ClearAfter is how long nothing to show waits before Discord is told: a song that ends and the
	// next that starts is one change, not a blank between them (review #183).
	ClearAfter time.Duration
	// PausedFor is how long a pause is shown, when it is: after that, as when nothing plays, so a
	// tab left paused does not keep its owner online (review #184).
	PausedFor time.Duration
	// CoverURL is a public address of an album's picture (all: Kanade's own too), or "".
	CoverURL func(ctx context.Context, album int64, all bool) string

	mu       sync.Mutex
	ctx      context.Context // Run's; nil until it runs
	follows  map[int64]context.CancelFunc
	states   map[int64]State
	pending  map[string]pendingLink // OAuth states, to a user
	assets   map[string]*asset      // pictures given to Discord, by application and address
	renewals map[int64]*sync.Mutex  // one renewal of a user's tokens at a time (review #172)
	changes  sync.Mutex             // one change of a link's settings at a time (review #178)
}

// asset is a picture given to Discord: what Discord calls it once taken, or when it last would not
// take it (review #183).
type asset struct {
	mp     string
	failed time.Time
	busy   bool
}

// assetRetry is how long a picture Discord did not take is not given again.
const assetRetry = 10 * time.Minute

type pendingLink struct {
	user    int64
	expires time.Time
}

func New(d *sql.DB, hub *presence.Hub, publicURL string, log *slog.Logger) *Service {
	return &Service{DB: d, Hub: hub, OAuth: NewOAuth(), Gateway: NewGateway(), Log: log, PublicURL: strings.TrimRight(publicURL, "/"),
		Grace: 2 * time.Minute, ClearAfter: 2 * time.Second, PausedFor: 10 * time.Minute, follows: map[int64]context.CancelFunc{},
		states: map[int64]State{}, pending: map[string]pendingLink{}, assets: map[string]*asset{}, renewals: map[int64]*sync.Mutex{}}
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

// ShowChange changes some of what is shown (nil: unchanged).
type ShowChange struct {
	Artist *bool   `json:"artist"`
	Album  *bool   `json:"album"`
	Time   *bool   `json:"time"`
	Paused *string `json:"paused"`
	Cover  *string `json:"cover"`
}

// Change sets what a link follows and shows (nil: unchanged). Only what is given changes, and one
// change at a time: two made at once both hold (review #178).
func (s *Service) Change(ctx context.Context, user int64, follow, followName *string, show *ShowChange, status *string) error {
	s.changes.Lock()
	defer s.changes.Unlock()
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
		for _, f := range []struct {
			to   *bool
			from *bool
		}{{&l.Show.Artist, show.Artist}, {&l.Show.Album, show.Album}, {&l.Show.Time, show.Time}} {
			if f.from != nil {
				*f.to = *f.from
			}
		}
		if show.Paused != nil {
			l.Show.Paused = "clear"
			if *show.Paused == "show" {
				l.Show.Paused = "show"
			}
		}
		if show.Cover != nil {
			if !slices.Contains(presence.Covers, *show.Cover) {
				return fmt.Errorf("%w: cover is one of %s", ErrInvalid, strings.Join(presence.Covers, ", "))
			}
			l.Show.Cover = *show.Cover
		}
	}
	if status != nil {
		if !slices.Contains(Statuses, *status) {
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

// setError marks a link as one Discord no longer accepts, unless it was made again since (linked:
// when the link refused was made; review #172).
func (s *Service) setError(user, linked int64, msg string) {
	res, err := s.DB.Exec(`UPDATE discord_links SET error = ? WHERE user_id = ? AND linked_at = ?`, clip(msg, 300), user, linked)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.setState(user, func(st *State) { st.Connected, st.Error = false, msg })
	}
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
	for range 3 {
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
		access, err := s.renew(ctx, l)
		if !errors.Is(err, errChanged) {
			return access, err
		}
	}
	return "", errChanged
}

// errChanged is a link renewed or made again while it was being renewed: it is looked at again.
var errChanged = fmt.Errorf("%w (the link changed meanwhile)", ErrUnavailable)

// renew renews l's tokens: one renewal of a user's at a time, and only of the link as it was. A
// renewal that answers after the link was renewed by another, or made again (another account, or
// the same anew), changes nothing and fails nothing of it (review #172).
func (s *Service) renew(ctx context.Context, l *Link) (string, error) {
	s.mu.Lock()
	lock := s.renewals[l.User]
	if lock == nil {
		lock = &sync.Mutex{}
		s.renewals[l.User] = lock
	}
	s.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()
	same := func() bool {
		now, err := s.Link(ctx, l.User)
		return err == nil && now != nil && now.LinkedAt == l.LinkedAt && now.Refresh == l.Refresh
	}
	if !same() {
		return "", errChanged
	}
	tok, err := s.OAuth.Refresh(ctx, s.App(ctx), l.Refresh)
	if err != nil {
		if !same() {
			return "", errChanged
		}
		return "", err
	}
	res, err := s.DB.ExecContext(ctx, `UPDATE discord_links SET access_token = ?, refresh_token = ?, expires_at = ?
		WHERE user_id = ? AND linked_at = ? AND refresh_token = ?`, tok.Access, tok.Refresh, tok.Expires.UnixMilli(), l.User, l.LinkedAt, l.Refresh)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", errChanged
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
	timer := func() *time.Timer {
		t := time.NewTimer(time.Hour)
		t.Stop()
		return t
	}
	grace, clearing, wake := timer(), timer(), timer()
	defer grace.Stop()
	defer clearing.Stop()
	defer wake.Stop()
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
	showing := false  // what Discord was last given shows something
	waiting := false  // nothing to show, not told yet
	clearDue := false // and waited long enough
	for {
		l, err := s.Link(ctx, user)
		if err != nil || l == nil || l.Error != "" {
			return
		}
		p := s.Hub.Pick(user, l.Follow)
		// Paused a while: as when nothing plays (review #184).
		wake.Stop()
		if p != nil && p.State == presence.Paused {
			if left := time.Until(time.UnixMilli(p.Changed).Add(s.PausedFor)); left > 0 {
				wake.Reset(left)
			} else {
				p = nil
			}
		}
		shown := presence.ShownBy(p, l.Show)
		image := s.Image(ctx)
		if shown.AlbumID > 0 && s.CoverURL != nil {
			if u := s.CoverURL(ctx, shown.AlbumID, l.Show.Cover == "all"); u != "" {
				if mp := s.asset(ctx, user, u); mp != "" {
					image = mp
				}
			}
		}
		act := Activity(shown, image, time.Now())
		key := []byte("none")
		if act != nil {
			key, _ = json.Marshal([]any{shown.State, shown.Title, shown.Artist, shown.Album, shown.DurationMS > 0, l.Status, image})
			key = fmt.Appendf(key, "|%d|%s", p.Changed, p.Player)
		}
		switch {
		case string(key) == last:
			waiting, clearDue = false, false
			clearing.Stop()
		case act == nil && showing && !clearDue:
			// Nothing to show: told in a moment, unless something plays by then (review #183).
			if !waiting {
				waiting = true
				clearing.Reset(s.ClearAfter)
			}
		default:
			last, showing, waiting, clearDue = string(key), act != nil, false, false
			clearing.Stop()
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
		case <-wake.C:
		case <-clearing.C:
			clearDue = true
		case <-grace.C:
			closeConn()
		case err := <-done:
			conn, done = nil, nil
			if errors.Is(err, ErrAuth) {
				msg := "Discord 不再接受這個連結（授權被撤銷或已失效），請重新連結。"
				if errors.Is(err, errGatewayRefused) {
					msg = "Discord 拒絕用這個連結更新狀態，換了新的授權也一樣：請確認應用程式的 Social SDK 仍然開著，再重新連結。"
				}
				s.Log.Warn("discord: the link is refused", "user", user, "err", err)
				s.setError(user, l.LinkedAt, msg)
				return
			}
		}
	}
}

// asset is what Discord calls a public picture, for the user's application; "" until it took it,
// or when it will not (the activity then shows the application's own picture). Discord is asked
// apart from the status, which goes on meanwhile, and told again once it took it; a picture it
// refused is not given again for a while (review #183).
func (s *Service) asset(ctx context.Context, user int64, picture string) string {
	app := s.App(ctx).ClientID
	key := app + " " + picture
	s.mu.Lock()
	a := s.assets[key]
	if a == nil {
		if len(s.assets) > 2000 {
			clear(s.assets)
		}
		a = &asset{}
		s.assets[key] = a
	}
	if a.mp != "" || a.busy || time.Since(a.failed) < assetRetry {
		mp := a.mp
		s.mu.Unlock()
		return mp
	}
	a.busy = true
	s.mu.Unlock()
	go func() {
		mp := ""
		token, err := s.token(ctx, user)
		if err == nil {
			actx, cancel := context.WithTimeout(ctx, 15*time.Second)
			mp, err = s.OAuth.ExternalAsset(actx, app, token, picture)
			cancel()
		}
		if err != nil && ctx.Err() == nil {
			s.Log.Info("discord: picture", "err", err)
		}
		s.mu.Lock()
		a.busy, a.mp = false, mp
		if mp == "" && ctx.Err() == nil {
			a.failed = time.Now()
		}
		s.mu.Unlock()
		if mp != "" {
			s.Hub.Poke(user)
		}
	}()
	return ""
}

// errGatewayRefused is a link the gateway refuses even with tokens just renewed.
var errGatewayRefused = fmt.Errorf("%w: the gateway refuses it with renewed tokens too", ErrAuth)

// connection keeps a connection to Discord's gateway for a user, connecting again when it drops,
// until ctx ends (nil) or Discord refuses the link (ErrAuth). A refused token is renewed once; one
// refused again before Discord accepted a connection is the link refused, not renewed on and on
// (review #181). Renewing that fails for now (Discord busy, or limiting) is tried again later, as
// a connection that dropped is.
func (s *Service) connection(ctx context.Context, user int64, want *latest) error {
	backoff := 5 * time.Second
	renewed := false // since Discord last accepted a connection
	for {
		token, err := s.token(ctx, user)
		if errors.Is(err, ErrAuth) || errors.Is(err, ErrNotLinked) {
			return ErrAuth
		}
		if err == nil {
			err = s.Gateway.connect(ctx, token, want, func() {
				backoff, renewed = 5*time.Second, false
				s.setState(user, func(st *State) { st.Connected, st.Error = true, "" })
			})
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, ErrAuth) {
				if renewed {
					return errGatewayRefused
				}
				renewed = true
				s.Log.Info("discord: the gateway refused the token; renewing it", "user", user)
				s.DB.Exec(`UPDATE discord_links SET expires_at = 0 WHERE user_id = ? AND access_token = ?`, user, token)
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
