// Package presence keeps what each web player of an account is playing, as the player says (review
// #135), for showing it elsewhere: the account owner's Discord status, and the account's other
// devices, which can take the playback over or control it (review #138, remote.go). A player tells
// only once it has really played: a queue brought back paused, a song loaded ahead or an album
// looked at tell nothing. What Discord shows follows one browser, or any, and of its players the
// one that last started playing; only what the owner chose to show is shown. All of it lives in
// memory only: a restart forgets it.
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
	Idle    = "idle" // a player connected (remote.go) that has played nothing: it tells its controls only
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
	AlbumID    int64  `json:"album_id"` // the library's album the song is played from, if any
	DurationMS int64  `json:"duration_ms"`
	PositionMS int64  `json:"position_ms"`
	Control
}

// Control is what another device controlling a player shows of it and acts on (review #138).
type Control struct {
	AssetID int64   `json:"asset_id,omitempty"`
	TrackID int64   `json:"track_id,omitempty"`
	CoverID int64   `json:"cover_id,omitempty"`
	QID     int64   `json:"qid,omitempty"`   // the queue entry playing, as that player numbers them
	Queue   string  `json:"queue,omitempty"` // the queue's version: another at each change of it
	Index   int     `json:"index"`
	Count   int     `json:"count"`
	Volume  float64 `json:"volume"`
	Muted   bool    `json:"muted,omitempty"`
	Mode    string  `json:"mode,omitempty"`
	Blocked bool    `json:"blocked,omitempty"` // its browser would not start playing without a click there
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
	AlbumID    int64  `json:"album_id,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	PositionMS int64  `json:"position_ms"`
	Changed    int64  `json:"changed"` // when it last changed (ms)
	Control
	Online bool  `json:"online"`         // connected: it can be asked to do something now (remote.go)
	Gone   bool  `json:"gone,omitempty"` // a player no longer there: what its browser played last, paused where it was
	Seen   int64 `json:"seen,omitempty"` // gone: when it was last heard of (ms)
}

// Hub holds the players. Players that stop telling go after Expire; what a browser played last is
// kept for Recent after its player went.
type Hub struct {
	Expire time.Duration
	Recent time.Duration
	now    func() time.Time

	mu      sync.Mutex
	players map[string]*player
	subs    map[int64]map[chan struct{}]bool // by user: told of every change
	recent  map[int64]map[string]*player     // by user and browser: the last player gone with a song
	conns   map[string]*Conn                 // by player
	pending map[int64]*pending               // commands sent, by seq, until acknowledged or expired
	seq     int64
}

func NewHub() *Hub {
	return &Hub{Expire: 90 * time.Second, Recent: 12 * time.Hour, now: time.Now, players: map[string]*player{},
		subs: map[int64]map[chan struct{}]bool{}, recent: map[int64]map[string]*player{}, conns: map[string]*Conn{},
		pending: map[int64]*pending{}, seq: time.Now().UnixMilli() * 1000}
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
	r.Queue, r.Mode = clip(r.Queue, 64), clip(r.Mode, 16)
	r.Volume = min(max(r.Volume, 0), 1)
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
		if r.State == Idle {
			h.idleLocked(user, id, r)
		}
		return true
	}
	if h.recent[user][r.Device] != nil { // its browser plays again: what it played before is no news
		delete(h.recent[user], r.Device)
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
	if !ok || was.State != r.State || was.Title != r.Title || was.Artist != r.Artist || was.Album != r.Album || was.AlbumID != r.AlbumID ||
		was.DurationMS != r.DurationMS || moved {
		p.changed = now
		h.tell(user)
	} else if was.Control != r.Control || was.DeviceName != r.DeviceName { // shown to a device controlling it, not a change of the song
		h.tell(user)
	}
	return true
}

// idleLocked keeps what an idle player tells, its controls (review #211), with its connection: a
// device controlling it starts from them (the volume, muted, the mode). It is not a player: no song,
// nothing on Discord, nothing kept once it goes. Told before it connected, it tells again then.
func (h *Hub) idleLocked(user int64, id string, r Report) {
	c := h.conns[id]
	if c == nil || c.user != user || r.Seq <= c.seq {
		return
	}
	c.seq = r.Seq
	if ctl := (Control{Volume: r.Volume, Muted: r.Muted, Mode: r.Mode, Blocked: r.Blocked}); ctl != c.ctl {
		c.ctl = ctl
		h.tell(user)
	}
}

// jump is how far a position may be from where the clock puts it before it counts as moved (ms).
const jump = 2000

// Remove drops a player (its tab closed); what it played is kept as its browser's last.
func (h *Hub) Remove(user int64, id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p, ok := h.players[id]; ok && p.user == user {
		h.keep(p)
		delete(h.players, id)
		h.tell(user)
	}
}

// keep remembers p, going, as what its browser played last: paused where it was.
func (h *Hub) keep(p *player) {
	now := h.now()
	q := *p
	if q.State == Playing {
		q.PositionMS += now.Sub(q.at).Milliseconds()
		if q.DurationMS > 0 {
			q.PositionMS = min(q.PositionMS, q.DurationMS)
		}
	}
	q.State, q.at, q.seen = Paused, now, now
	if h.recent[p.user] == nil {
		h.recent[p.user] = map[string]*player{}
	}
	h.recent[p.user][p.Device] = &q
}

// EndSessions drops the players of logins that ended, and closes their connections: those of
// sessions, or with keep > 0 every one of user's but keep, or with keep < 0 every one of user's
// (what they played last too).
func (h *Hub) EndSessions(user int64, keep int64, sessions ...int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ended := func(session int64) bool {
		drop := keep < 0 || keep > 0 && session != keep
		for _, s := range sessions {
			drop = drop || session == s
		}
		return drop
	}
	gone := false
	for id, p := range h.players {
		if p.user == user && ended(p.session) {
			delete(h.players, id)
			gone = true
		}
	}
	for id, c := range h.conns {
		if c.user == user && ended(c.session) {
			delete(h.conns, id)
			close(c.done)
			gone = true
		}
	}
	if keep < 0 && len(h.recent[user]) > 0 {
		delete(h.recent, user)
		gone = true
	}
	if gone {
		h.tell(user)
	}
}

// Poke tells user's watchers to look again (what is shown of it changed).
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

func (h *Hub) nowOf(p *player, now time.Time) Now {
	pos := p.PositionMS
	if p.State == Playing {
		pos += now.Sub(p.at).Milliseconds()
		if p.DurationMS > 0 {
			pos = min(pos, p.DurationMS)
		}
	}
	return Now{Player: p.id, Device: p.Device, DeviceName: p.DeviceName, State: p.State, Title: p.Title, Artist: p.Artist, Album: p.Album,
		AlbumID: p.AlbumID, DurationMS: p.DurationMS, PositionMS: pos, Changed: p.changed.UnixMilli(), Control: p.Control}
}

// Pick is what is shown of user following device (empty: any): of its players, the one that last
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
		// A connected player that stopped telling (a paused tab in the background) is still there.
		if c := h.conns[id]; now.Sub(p.seen) > h.Expire && (c == nil || c.user != p.user) {
			h.keep(p)
			delete(h.players, id)
			h.tell(p.user)
		}
	}
	for user, devices := range h.recent {
		for device, p := range devices {
			if now.Sub(p.seen) > h.Recent {
				delete(devices, device)
				h.tell(user)
			}
		}
		if len(devices) == 0 {
			delete(h.recent, user)
		}
	}
	for seq, c := range h.pending {
		if now.After(c.expires) {
			delete(h.pending, seq)
		}
	}
}
