package discord

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/presence"
)

// fakeDiscord is Discord's web API and gateway, as far as linking and presence go.
type fakeDiscord struct {
	t        *testing.T
	api, gw  *httptest.Server
	mu       sync.Mutex
	access   string // the access token it accepts now
	refused  atomic.Bool
	revoked  atomic.Int32
	scope    string
	frames   chan map[string]any // what clients sent on the gateway (identify, presence)
	closeNow chan websocket.StatusCode
	conns    atomic.Int32
}

func newFake(t *testing.T) *fakeDiscord {
	f := &fakeDiscord{t: t, access: "acc1", scope: "openid sdk.social_layer_presence identify", frames: make(chan map[string]any, 64),
		closeNow: make(chan websocket.StatusCode, 1)}
	f.api = httptest.NewServer(http.HandlerFunc(f.serveAPI))
	f.gw = httptest.NewServer(http.HandlerFunc(f.serveGateway))
	t.Cleanup(func() { f.api.Close(); f.gw.Close() })
	return f
}

func (f *fakeDiscord) serveAPI(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	switch r.URL.Path {
	case "/oauth2/token":
		if r.Form.Get("client_secret") != "sec" {
			http.Error(w, `{"error":"invalid_client"}`, 401)
			return
		}
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if r.Form.Get("code") != "good" || r.Form.Get("redirect_uri") != "https://music.example/oauth/discord/callback" {
				http.Error(w, `{"error":"invalid_grant"}`, 400)
				return
			}
		case "refresh_token":
			if f.refused.Load() {
				http.Error(w, `{"error":"invalid_grant"}`, 400)
				return
			}
			f.mu.Lock()
			f.access = "acc2"
			f.mu.Unlock()
		}
		f.mu.Lock()
		access := f.access
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"access_token": access, "refresh_token": "ref", "expires_in": 604800, "scope": f.scope})
	case "/oauth2/token/revoke":
		f.revoked.Add(1)
	case "/users/@me":
		f.mu.Lock()
		ok := r.Header.Get("Authorization") == "Bearer "+f.access
		f.mu.Unlock()
		if !ok {
			http.Error(w, "{}", 401)
			return
		}
		w.Write([]byte(`{"id":"42","username":"ser1ka","global_name":"Seri"}`))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeDiscord) serveGateway(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	f.conns.Add(1)
	ctx := r.Context()
	write := func(v any) { b, _ := json.Marshal(v); c.Write(ctx, websocket.MessageText, b) }
	write(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 40000}})
	read := make(chan map[string]any)
	go func() {
		defer close(read)
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m map[string]any
			json.Unmarshal(b, &m)
			read <- m
		}
	}()
	for {
		select {
		case code := <-f.closeNow:
			c.Close(code, "bye")
			return
		case m, ok := <-read:
			if !ok {
				return
			}
			switch m["op"] {
			case 2.0:
				d := m["d"].(map[string]any)
				f.mu.Lock()
				ok := d["token"] == "Bearer "+f.access
				f.mu.Unlock()
				f.frames <- m
				if !ok {
					c.Close(4004, "Authentication failed.")
					return
				}
				write(map[string]any{"op": 0, "s": 1, "t": "READY", "d": map[string]any{"session_id": "x"}})
			case 1.0:
				write(map[string]any{"op": 11})
			case 3.0:
				f.frames <- m
			}
		}
	}
}

// next is the next frame of op sent on the gateway.
func (f *fakeDiscord) next(op float64) map[string]any {
	f.t.Helper()
	for {
		select {
		case m := <-f.frames:
			if m["op"] == op {
				return m["d"].(map[string]any)
			}
		case <-time.After(5 * time.Second):
			f.t.Fatalf("no op %v", op)
		}
	}
}

func setup(t *testing.T) (*Service, *fakeDiscord, *presence.Hub, context.CancelFunc) {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	d.Exec(`INSERT INTO users (id, username, password_hash, created_at) VALUES (1, 'a', 'x', 0)`)
	f := newFake(t)
	hub := presence.NewHub()
	s := New(d, hub, "https://music.example/", slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.OAuth.API, s.OAuth.Site = f.api.URL, f.api.URL
	s.Gateway.URL = "ws" + strings.TrimPrefix(f.gw.URL, "http")
	s.Gateway.MinGap = 0
	s.Grace = 300 * time.Millisecond
	rctx, cancel := context.WithCancel(ctx)
	go s.Run(rctx)
	t.Cleanup(cancel)
	for i := 0; i < 100; i++ { // Run has started
		s.mu.Lock()
		started := s.ctx != nil
		s.mu.Unlock()
		if started {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	return s, f, hub, cancel
}

func link(t *testing.T, s *Service) {
	t.Helper()
	ctx := context.Background()
	if err := s.SetApp(ctx, "123456789012345678", "sec", "kanade"); err != nil {
		t.Fatal(err)
	}
	u, err := s.Begin(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	q, _ := url.Parse(u)
	if q.Query().Get("scope") != Scopes || q.Query().Get("redirect_uri") != "https://music.example/oauth/discord/callback" {
		t.Fatalf("authorize %s", u)
	}
	if _, err := s.Finish(ctx, "forged", "good"); !errors.Is(err, ErrState) {
		t.Fatalf("a forged state: %v", err)
	}
	if user, err := s.Finish(ctx, q.Query().Get("state"), "good"); err != nil || user != 1 {
		t.Fatal(user, err)
	}
	if _, err := s.Finish(ctx, q.Query().Get("state"), "good"); !errors.Is(err, ErrState) {
		t.Fatalf("a state twice: %v", err)
	}
	l, _ := s.Link(ctx, 1)
	if l == nil || l.DiscordID != "42" || l.Name != "Seri（@ser1ka）" || l.Access != "acc1" || l.Status != "idle" || !s.Linked(ctx, 1) {
		t.Fatalf("link %+v", l)
	}
}

func playing(seq int64, state, title string, pos int64) presence.Report {
	return presence.Report{Device: "desk", Seq: seq, State: state, Title: title, Artist: "Round Table", Album: "ARIA OST", DurationMS: 230_000, PositionMS: pos}
}

// From the link to the status (review #135): nothing connects until something plays; then the
// gateway is told who (the link's token) and what (listening, the song, its artist and album, where
// in it); a pause clears it as chosen, and the connection goes a while after; a refused token is
// renewed; unlinking revokes it.
func TestServiceShowsWhatPlays(t *testing.T) {
	s, f, hub, _ := setup(t)
	if _, err := s.Begin(context.Background(), 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no application yet: %v", err)
	}
	if err := s.SetApp(context.Background(), "12", "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a bad client ID: %v", err)
	}
	link(t, s)
	time.Sleep(100 * time.Millisecond)
	if f.conns.Load() != 0 {
		t.Fatal("connected with nothing to show")
	}
	hub.Put(1, 7, "tab-a-1234", playing(1, presence.Playing, "Undine", 30_000))
	if id := f.next(2); id["token"] != "Bearer acc1" || id["intents"] != 0.0 {
		t.Fatalf("identify %v", id)
	}
	p := f.next(3)
	acts := p["activities"].([]any)
	a := acts[0].(map[string]any)
	ts := a["timestamps"].(map[string]any)
	if p["status"] != "idle" || a["type"] != 2.0 || a["name"] != "Kanade" || a["details"] != "Undine" || a["state"] != "Round Table" ||
		a["assets"].(map[string]any)["large_image"] != "kanade" || a["assets"].(map[string]any)["large_text"] != "ARIA OST" ||
		ts["end"].(float64)-ts["start"].(float64) != 230_000 {
		t.Fatalf("presence %v", p)
	}
	if st := s.StateOf(1); !st.Connected || st.Showing != "Undine" {
		t.Fatalf("state %+v", st)
	}
	// What is shown changed: the status and the album go.
	online := "online"
	show := presence.Show{Artist: true, Time: true, Paused: "show"}
	if err := s.Change(context.Background(), 1, nil, nil, &show, &online); err != nil {
		t.Fatal(err)
	}
	if p := f.next(3); p["status"] != "online" || p["activities"].([]any)[0].(map[string]any)["assets"].(map[string]any)["large_text"] != nil {
		t.Fatalf("changed %v", p)
	}
	hub.Put(1, 7, "tab-a-1234", playing(2, presence.Paused, "Undine", 40_000))
	if a := f.next(3)["activities"].([]any)[0].(map[string]any); a["state"] != "Round Table · 已暫停" || a["timestamps"] != nil {
		t.Fatalf("paused, shown %v", a)
	}
	// Stopped: cleared, then the connection goes.
	hub.Put(1, 7, "tab-a-1234", playing(3, presence.Stopped, "", 0))
	if p := f.next(3); len(p["activities"].([]any)) != 0 {
		t.Fatalf("cleared %v", p)
	}
	for i := 0; i < 100 && s.StateOf(1).Connected; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if s.StateOf(1).Connected {
		t.Fatal("still connected with nothing to show")
	}

	// Discord refuses the token: renewed, connected again with the new one.
	hub.Put(1, 7, "tab-a-1234", playing(4, presence.Playing, "Rainbow", 0))
	f.next(2)
	f.next(3)
	f.mu.Lock()
	f.access = "stale"
	f.mu.Unlock()
	f.closeNow <- 4004
	if id := f.next(2); id["token"] != "Bearer acc2" {
		t.Fatalf("renewed %v", id)
	}
	if a := f.next(3)["activities"].([]any)[0].(map[string]any); a["details"] != "Rainbow" {
		t.Fatalf("after renewal %v", a)
	}

	// Unlinking: disconnected, revoked, forgotten.
	if err := s.Unlink(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if f.revoked.Load() != 1 || s.Linked(context.Background(), 1) {
		t.Fatal("not revoked or still linked")
	}
}

// A link Discord no longer accepts and cannot renew stops, saying so; one without the presence scope
// (the Social SDK not turned on) is not made.
func TestServiceRefusedLink(t *testing.T) {
	s, f, hub, _ := setup(t)
	f.scope = "identify"
	s.SetApp(context.Background(), "123456789012345678", "sec", "")
	u, _ := s.Begin(context.Background(), 1)
	q, _ := url.Parse(u)
	if _, err := s.Finish(context.Background(), q.Query().Get("state"), "good"); !errors.Is(err, ErrScope) {
		t.Fatalf("no presence scope: %v", err)
	}
	f.scope = "openid sdk.social_layer_presence identify"
	link(t, s)
	f.mu.Lock()
	f.access = "other"
	f.mu.Unlock()
	f.refused.Store(true)
	hub.Put(1, 7, "tab-a-1234", playing(1, presence.Playing, "Undine", 0))
	for i := 0; i < 200 && s.StateOf(1).Error == ""; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if l, _ := s.Link(context.Background(), 1); l.Error == "" || s.Linked(context.Background(), 1) {
		t.Fatalf("a refused link goes on: %+v %+v", l, s.StateOf(1))
	}
}

func TestActivity(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	a := Activity(presence.Shown{State: presence.Playing, Title: "A", Artist: "B", Album: "C", PositionMS: 1000, DurationMS: 5000}, "", now)
	if a["details"] != "A ♪" || a["state"] != "B · C" || a["assets"] != nil || a["timestamps"].(map[string]any)["start"] != now.UnixMilli()-1000 {
		t.Fatalf("%v", a)
	}
	if Activity(presence.Shown{State: "none"}, "", now) != nil {
		t.Fatal("none")
	}
	long := Activity(presence.Shown{State: presence.Playing, Title: strings.Repeat("長", 300)}, "", now)
	if n := len([]rune(long["details"].(string))); n != 128 {
		t.Fatal(n)
	}
}
