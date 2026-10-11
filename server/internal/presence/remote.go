package presence

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// Taking playback over and controlling it from another device (review #138). Each web player keeps
// a connection open (an event stream); the hub sends a player what another of the same account's
// players asks of it (a Command), and sends the answer (an Ack) back to the one that asked. Nothing
// waits for a player not connected: a command is sent at once or refused, and one that is late (its
// player got it after Expires, say from the background) is not carried out, so an old one is never
// played again; a player carries out each seq once.

// CommandTTL is how long a command may take to be carried out and answered.
const CommandTTL = 10 * time.Second

// Ops are what a player can be asked to do; the web player says what each does with its args.
var Ops = map[string]bool{
	"toggle": true, "play": true, "pause": true, "next": true, "prev": true, "seek": true, "seekBy": true, "volume": true, "volumeBy": true,
	"mute": true, "mode": true,
	"jump": true, "enqueue": true, "playNext": true, "playQueue": true, "snapshot": true, "release": true,
}

var (
	ErrOffline = errors.New("player not connected")
	ErrBusy    = errors.New("player not keeping up")
	ErrTaken   = errors.New("player of another account")
	ErrTooMany = errors.New("too many players")
)

// Event is what goes down a player's connection: a command for it, the answer to one it sent, or
// the account's devices as they are now (sent by the connection's handler).
type Event struct {
	Name string
	Data any
}

// Command is what a player is asked to do. Seq grows with each command the hub sends; Expires is
// the hub's time (ms), which a player compares with its own through the connection's first event.
type Command struct {
	ID      string          `json:"id"`
	Seq     int64           `json:"seq"`
	From    string          `json:"from"`
	Op      string          `json:"op"`
	Args    json.RawMessage `json:"args,omitempty"`
	Expires int64           `json:"expires"`
}

// Ack is a player's answer to a command: done or why not, with what was asked for (a snapshot).
type Ack struct {
	ID    string          `json:"id"`
	Seq   int64           `json:"seq"`
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

type pending struct {
	user         int64
	from, target string
	id           string
	expires      time.Time
}

// Conn is a player's connection: Events to send down it until Done (another connection of the
// same player took its place, or its login ended).
type Conn struct {
	user, session     int64
	tab, device, name string
	since             time.Time
	out               chan Event
	done              chan struct{}
}

func (c *Conn) Events() <-chan Event  { return c.out }
func (c *Conn) Done() <-chan struct{} { return c.done }

// Connect opens player tab's connection; name is how its browser names itself, for a player that
// has told nothing yet.
func (h *Hub) Connect(user, session int64, tab, device, name string) (*Conn, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if old := h.conns[tab]; old != nil {
		if old.user != user {
			return nil, ErrTaken
		}
		delete(h.conns, tab)
		close(old.done)
	}
	n := 0
	for _, c := range h.conns {
		if c.user == user {
			n++
		}
	}
	if n >= maxPlayersPerUser {
		return nil, ErrTooMany
	}
	c := &Conn{user: user, session: session, tab: tab, device: clip(device, 64), name: clip(name, 80), since: h.now(),
		out: make(chan Event, 64), done: make(chan struct{})}
	h.conns[tab] = c
	h.tell(user)
	return c, nil
}

// Disconnect closes c, unless another connection of its player took its place already.
func (h *Hub) Disconnect(c *Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[c.tab] == c {
		delete(h.conns, c.tab)
		h.tell(c.user)
	}
}

// Send sends player target of user a command from player from.
func (h *Hub) Send(user int64, from, target, op string, args json.RawMessage) (Command, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.conns[target]
	if c == nil || c.user != user {
		return Command{}, ErrOffline
	}
	h.seq++
	now := h.now()
	cmd := Command{ID: randomID(), Seq: h.seq, From: from, Op: op, Args: args, Expires: now.Add(CommandTTL).UnixMilli()}
	select {
	case c.out <- Event{"command", cmd}:
	default:
		return Command{}, ErrBusy
	}
	h.pending[cmd.Seq] = &pending{user: user, from: from, target: target, id: cmd.ID, expires: now.Add(CommandTTL)}
	return cmd, nil
}

// Ack takes player tab's answer to a command sent it, and sends it to the player that asked;
// false when there is no such command waiting (answered already, too late, another's).
func (h *Hub) Ack(user int64, tab string, a Ack) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.pending[a.Seq]
	if p == nil || p.user != user || p.target != tab || h.now().After(p.expires) {
		return false
	}
	delete(h.pending, a.Seq)
	a.ID = p.id
	if c := h.conns[p.from]; c != nil && c.user == user {
		select {
		case c.out <- Event{"ack", a}:
		default:
		}
	}
	return true
}

// Devices lists user's players for its devices to choose from: those that told what they play,
// those connected that have played nothing (Idle), and what each browser with no song now played
// last (Gone).
func (h *Hub) Devices(user int64) []Now {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	out := []Now{}
	listed := map[string]bool{}
	playing := map[string]bool{} // browsers with a song
	for _, p := range h.players {
		if p.user != user {
			continue
		}
		n := h.nowOf(p, now)
		c := h.conns[p.id]
		n.Online = c != nil && c.user == user
		out = append(out, n)
		listed[p.id], playing[p.Device] = true, true
	}
	for id, c := range h.conns {
		if c.user != user || listed[id] {
			continue
		}
		out = append(out, Now{Player: id, Device: c.device, DeviceName: c.name, State: Idle, Changed: c.since.UnixMilli(), Online: true})
	}
	for device, p := range h.recent[user] {
		if playing[device] {
			continue
		}
		n := h.nowOf(p, now)
		n.Player, n.Gone, n.Seen = "", true, p.seen.UnixMilli()
		out = append(out, n)
	}
	return out
}

func randomID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}
