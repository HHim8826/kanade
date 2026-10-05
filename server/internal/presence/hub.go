// Package presence keeps what each web player of an account is playing, as the player says (review
// #135), for showing it elsewhere: the account owner's Discord status. A player tells only once it
// has really played: a queue brought back paused, a song loaded ahead or an album looked at tell
// nothing. What is shown follows one browser, or any, and of its players the one that last started
// playing; only what the owner chose to show is shown.
package presence

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// States a player tells.
const (
	Playing = "playing"
	Paused  = "paused"
	Stopped = "stopped"
)

// Report is what a player tells of itself.
type Report struct {
	Device     string `json:"device"`      // the browser: the same in all its tabs
	DeviceName string `json:"device_name"` // as the browser names itself ("Windows · Chrome")
	Seq        int64  `json:"seq"`         // grows with each report: an older one arriving late is dropped
	State      string `json:"state"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Album      string `json:"album"`
	DurationMS int64  `json:"duration_ms"`
	PositionMS int64  `json:"position_ms"`
}

type player struct {
	Report
	id      string
	user    int64
	session int64
	at      time.Time // when the position was PositionMS
	started time.Time // when it last started playing, or started another song playing
	changed time.Time
	seen    time.Time
}

// Now is a player's state as of now.
type Now struct {
	Player     string `json:"player"`
	Device     string `json:"device"`
	DeviceName string `json:"device_name"`
	State      string `json:"state"`
	Title      string `json:"title"`
	Artist     string `json:"artist,omitempty"`
	Album      string `json:"album,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	PositionMS int64  `json:"position_ms"`
	Changed    int64  `json:"changed"` // when it last changed (ms)
}

// Hub holds the players. Players that stop telling go after Expire.
type Hub struct {
	Expire time.Duration
	now    func() time.Time

	mu      sync.Mutex
	players map[string]*player
	subs    map[int64]map[chan struct{}]bool // by user: told of every change
	online  map[int64]int                    // companions with a stream open, by companion
}

func NewHub() *Hub {
	return &Hub{Expire: 90 * time.Second, now: time.Now, players: map[string]*player{}, subs: map[int64]map[chan struct{}]bool{},
		online: map[int64]int{}}
}

const maxPlayersPerUser = 50

func clip(s string, n int) string {
	s = strings.TrimSpace(strings.ToValidUTF8(s, ""))
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// Put takes a player's report; false when it was older than one taken already, or of a player of
// another user.
func (h *Hub) Put(user, session int64, id string, r Report) bool {
	r.Device, r.DeviceName = clip(r.Device, 64), clip(r.DeviceName, 80)
	r.Title, r.Artist, r.Album = clip(r.Title, 300), clip(r.Artist, 300), clip(r.Album, 300)
	r.DurationMS, r.PositionMS = max(r.DurationMS, 0), max(r.PositionMS, 0)
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	p, ok := h.players[id]
	if ok && (p.user != user || r.Seq <= p.Seq) {
		return false
	}
	if r.State != Playing && r.State != Paused {
		if ok {
			delete(h.players, id)
			h.tell(user)
		}
		return true
	}
	if !ok {
		n := 0
		for _, q := range h.players {
			if q.user == user {
				n++
			}
		}
		if n >= maxPlayersPerUser {
			return false
		}
		p = &player{id: id, user: user}
		h.players[id] = p
	}
	was, wasAt := p.Report, p.at
	if r.State == Playing && (was.State != Playing || was.Title != r.Title || was.Artist != r.Artist || was.Album != r.Album) {
		p.started = now
	}
	// Playing on, the position moves as the clock does: only a jump (a seek, a song begun again) is
	// a change.
	moved := r.PositionMS != was.PositionMS
	if ok && was.State == Playing && r.State == Playing {
		d := r.PositionMS - (was.PositionMS + now.Sub(wasAt).Milliseconds())
		moved = d > jump || d < -jump
	}
	p.Report, p.session, p.at, p.seen = r, session, now, now
	if !ok || was.State != r.State || was.Title != r.Title || was.Artist != r.Artist || was.Album != r.Album ||
		was.DurationMS != r.DurationMS || moved {
		p.changed = now
		h.tell(user)
	}
	return true
}

// jump is how far a position may be from where the clock puts it before it counts as moved (ms).
const jump = 2000

// Remove drops a player (its tab closed).
func (h *Hub) Remove(user int64, id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p, ok := h.players[id]; ok && p.user == user {
		delete(h.players, id)
		h.tell(user)
	}
}

// EndSessions drops the players of logins that ended: those of sessions, or with keep > 0 every
// one of user's but keep, or with keep < 0 every one of user's.
func (h *Hub) EndSessions(user int64, keep int64, sessions ...int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	gone := false
	for id, p := range h.players {
		if p.user != user {
			continue
		}
		drop := keep < 0 || keep > 0 && p.session != keep
		for _, s := range sessions {
			drop = drop || p.session == s
		}
		if drop {
			delete(h.players, id)
			gone = true
		}
	}
	if gone {
		h.tell(user)
	}
}

// Poke tells user's watchers to look again (a companion's settings changed).
func (h *Hub) Poke(user int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tell(user)
}

func (h *Hub) tell(user int64) {
	for c := range h.subs[user] {
		select {
		case c <- struct{}{}:
		default: // told already, not read yet
		}
	}
}

// Watch tells c of every change of user's players until stop.
func (h *Hub) Watch(user int64) (c <-chan struct{}, stop func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subs[user] == nil {
		h.subs[user] = map[chan struct{}]bool{}
	}
	h.subs[user][ch] = true
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs[user], ch)
		h.mu.Unlock()
	}
}

// Connected counts a companion's open stream until done.
func (h *Hub) Connected(companion int64) (done func()) {
	h.mu.Lock()
	h.online[companion]++
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		if h.online[companion]--; h.online[companion] <= 0 {
			delete(h.online, companion)
		}
		h.mu.Unlock()
	}
}

// Online says whether a companion has a stream open.
func (h *Hub) Online(companion int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.online[companion] > 0
}

func (h *Hub) nowOf(p *player, now time.Time) Now {
	pos := p.PositionMS
	if p.State == Playing {
		pos += now.Sub(p.at).Milliseconds()
		if p.DurationMS > 0 {
			pos = min(pos, p.DurationMS)
		}
	}
	return Now{Player: p.id, Device: p.Device, DeviceName: p.DeviceName, State: p.State, Title: p.Title, Artist: p.Artist, Album: p.Album,
		DurationMS: p.DurationMS, PositionMS: pos, Changed: p.changed.UnixMilli()}
}

// Pick is what a companion following device (empty: any) shows: of its players, the one that last
// started playing; when none plays, the one paused last. Players never take turns: one telling
// again changes nothing of which. Nil when there is none.
func (h *Hub) Pick(user int64, device string) *Now {
	h.mu.Lock()
	defer h.mu.Unlock()
	var best *player
	for _, p := range h.players {
		if p.user != user || (device != "" && p.Device != device) {
			continue
		}
		if best == nil || better(p, best) {
			best = p
		}
	}
	if best == nil {
		return nil
	}
	n := h.nowOf(best, h.now())
	return &n
}

// better says whether p is shown rather than q: playing before paused, then the one that started
// playing last (paused: changed last), then, at the same instant, always the same one.
func better(p, q *player) bool {
	if (p.State == Playing) != (q.State == Playing) {
		return p.State == Playing
	}
	a, b := p.changed, q.changed
	if p.State == Playing {
		a, b = p.started, q.started
	}
	if !a.Equal(b) {
		return a.After(b)
	}
	return p.id < q.id
}

// Players lists user's players, for the settings page.
func (h *Hub) Players(user int64) []Now {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	out := []Now{}
	for _, p := range h.players {
		if p.user == user {
			out = append(out, h.nowOf(p, now))
		}
	}
	return out
}

// Run drops the players that stopped telling, until ctx ends.
func (h *Hub) Run(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.expire()
		}
	}
}

func (h *Hub) expire() {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	for id, p := range h.players {
		if now.Sub(p.seen) > h.Expire {
			delete(h.players, id)
			h.tell(p.user)
		}
	}
}
