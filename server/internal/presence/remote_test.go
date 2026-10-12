package presence

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func next(t *testing.T, c *Conn) Event {
	t.Helper()
	select {
	case e := <-c.Events():
		return e
	default:
		t.Fatal("nothing sent")
		return Event{}
	}
}

func quiet(c *Conn) bool {
	select {
	case <-c.Events():
		return false
	default:
		return true
	}
}

func closed(c *Conn) bool {
	select {
	case <-c.Done():
		return true
	default:
		return false
	}
}

// Commands between an account's players (review #138): sent in order, only to a player connected
// and of the same account; answered once, to the one that asked, and not once too late; a player
// connecting again takes its place, and the old connection hears nothing more; logins ending close
// theirs.
func TestCommands(t *testing.T) {
	c := &clock{time.Unix(1_800_000_000, 0)}
	h := NewHub()
	h.now = c.now
	a, err := h.Connect(1, 10, "tab-a-000", "desk", "Windows · Chrome")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := h.Connect(1, 11, "tab-b-000", "phone", "Android · Chrome")
	x, _ := h.Connect(2, 20, "tab-x-000", "other", "Linux · Firefox")
	if _, err := h.Connect(2, 20, "tab-b-000", "phone", ""); !errors.Is(err, ErrTaken) {
		t.Fatalf("another account's player taken: %v", err)
	}

	first, err := h.Send(1, "tab-a-000", "tab-b-000", "pause", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := h.Send(1, "tab-a-000", "tab-b-000", "seek", json.RawMessage(`{"sec":30}`))
	if second.Seq <= first.Seq || second.Expires != c.t.Add(CommandTTL).UnixMilli() || second.ID == first.ID {
		t.Fatalf("seq %d then %d, expires %d", first.Seq, second.Seq, second.Expires)
	}
	if e := next(t, b); e.Name != "command" || e.Data.(Command).Seq != first.Seq || e.Data.(Command).Op != "pause" {
		t.Fatalf("first sent: %+v", e)
	}
	if e := next(t, b); e.Data.(Command).Seq != second.Seq || string(e.Data.(Command).Args) != `{"sec":30}` {
		t.Fatalf("second sent: %+v", e)
	}
	for _, target := range []string{"tab-x-000", "tab-none-0"} {
		if _, err := h.Send(1, "tab-a-000", target, "pause", nil); !errors.Is(err, ErrOffline) {
			t.Fatalf("%s: %v", target, err)
		}
	}
	if !quiet(x) {
		t.Fatal("another account's player was sent something")
	}

	// Answers: by the player sent it only, once, to the one that asked.
	if h.Ack(2, "tab-b-000", Ack{Seq: first.Seq, OK: true}) || h.Ack(1, "tab-a-000", Ack{Seq: first.Seq, OK: true}) {
		t.Fatal("answered by another")
	}
	if !h.Ack(1, "tab-b-000", Ack{Seq: first.Seq, OK: true, Data: json.RawMessage(`{"queue":[]}`)}) {
		t.Fatal("not taken")
	}
	if e := next(t, a); e.Name != "ack" || e.Data.(Ack).ID != first.ID || !e.Data.(Ack).OK || string(e.Data.(Ack).Data) != `{"queue":[]}` {
		t.Fatalf("answer: %+v", e)
	}
	if h.Ack(1, "tab-b-000", Ack{Seq: first.Seq, OK: true}) {
		t.Fatal("answered twice")
	}
	// Too late: dropped, and forgotten.
	c.add(CommandTTL + time.Second)
	if h.Ack(1, "tab-b-000", Ack{Seq: second.Seq, OK: true}) || !quiet(a) {
		t.Fatal("a late answer went through")
	}
	h.expire()
	if len(h.pending) != 0 {
		t.Fatalf("%d waiting still", len(h.pending))
	}

	// A player not reading falls behind: refused, not queued for ever.
	var full error
	for i := 0; i < 70 && full == nil; i++ {
		_, full = h.Send(1, "tab-b-000", "tab-a-000", "next", nil)
	}
	if !errors.Is(full, ErrBusy) {
		t.Fatalf("not refused: %v", full)
	}
	for !quiet(a) {
	}

	// Connecting again: the new connection takes the commands; the old one ends, and closing it
	// late leaves the new one.
	b2, _ := h.Connect(1, 11, "tab-b-000", "phone", "Android · Chrome")
	if !closed(b) || closed(b2) {
		t.Fatal("the old connection is still open")
	}
	h.Disconnect(b)
	cmd, err := h.Send(1, "tab-a-000", "tab-b-000", "toggle", nil)
	if err != nil || next(t, b2).Data.(Command).Seq != cmd.Seq || !quiet(b) {
		t.Fatalf("after connecting again: %v", err)
	}

	// The phone's login ends: its connection closes, it is not listed, and nothing reaches it.
	h.EndSessions(1, 0, 11)
	if !closed(b2) {
		t.Fatal("still open after its login ended")
	}
	if _, err := h.Send(1, "tab-a-000", "tab-b-000", "toggle", nil); !errors.Is(err, ErrOffline) {
		t.Fatalf("sent after its login ended: %v", err)
	}
	for _, d := range h.Devices(1) {
		if d.Player == "tab-b-000" {
			t.Fatalf("listed: %+v", d)
		}
	}
	if closed(a) || closed(x) {
		t.Fatal("another login's connection closed")
	}
}

// The devices listed (review #138): a player connected that played nothing is idle; one that tells
// shows what it plays and whether it is connected; one gone leaves what its browser played last,
// paused where it was, until the browser plays again, Recent passes or every login ends. Changes
// for a device controlling it are told, but are no change of the song.
func TestDevices(t *testing.T) {
	c := &clock{time.Unix(1_800_000_000, 0)}
	h := NewHub()
	h.now = c.now
	changes, stop := h.Watch(1)
	defer stop()
	told := func() bool {
		select {
		case <-changes:
			return true
		default:
			return false
		}
	}
	find := func(device string) *Now { // a song it plays, or played last, before an idle player
		var found *Now
		for _, d := range h.Devices(1) {
			if d.Device == device && (found == nil || found.State == Idle) {
				found = &d
			}
		}
		return found
	}
	h.Connect(1, 10, "tab-a-000", "desk", "Windows · Chrome")
	if d := find("desk"); d == nil || d.State != Idle || !d.Online || d.DeviceName != "Windows · Chrome" {
		t.Fatalf("idle: %+v", d)
	}
	r := song(Playing, "One", 10_000)
	r.Seq, r.Control = 1, Control{AssetID: 7, QID: 3, Queue: "q1", Index: 2, Count: 9, Volume: 0.5, Mode: "order"}
	h.Put(1, 10, "tab-a-000", r)
	d := find("desk")
	if d == nil || d.State != Playing || !d.Online || d.AssetID != 7 || d.Count != 9 || d.Volume != 0.5 {
		t.Fatalf("playing: %+v", d)
	}
	changed := d.Changed
	told()
	c.add(time.Second)
	r.Seq, r.Volume, r.PositionMS = 2, 0.8, 11_000
	h.Put(1, 10, "tab-a-000", r)
	if d := find("desk"); !told() || d.Volume != 0.8 || d.Changed != changed {
		t.Fatalf("the volume: told, not a change of the song: %+v", d)
	}
	// A phone that tells without a connection: listed, not online.
	p := song(Paused, "Two", 5_000)
	p.Seq, p.Device = 1, "phone"
	h.Put(1, 11, "tab-p-000", p)
	if d := find("phone"); d == nil || d.Online || d.State != Paused {
		t.Fatalf("phone: %+v", d)
	}
	// The desk's tab closes while playing: its browser played that, paused where it was then.
	c.add(20 * time.Second)
	h.Remove(1, "tab-a-000")
	c.add(time.Hour)
	d = find("desk")
	if d == nil || !d.Gone || d.Online || d.State != Paused || d.PositionMS != 31_000 || d.Title != "One" || d.Player != "" || d.AssetID != 7 {
		t.Fatalf("gone: %+v", d)
	}
	// Its connection still open, it is listed as idle too; the browser playing again replaces it.
	if n := len(h.Devices(1)); n != 3 {
		t.Fatalf("%d listed", n)
	}
	r = song(Playing, "Three", 0)
	r.Seq = 3
	h.Put(1, 10, "tab-a-000", r)
	if d := find("desk"); d == nil || d.Gone || d.Title != "Three" {
		t.Fatalf("playing again: %+v", d)
	}
	if n := len(h.Devices(1)); n != 2 {
		t.Fatalf("%d listed after playing again", n)
	}
	// A connected player that stopped telling stays; one that is not connected goes after Expire,
	// leaving its browser's last song.
	c.add(h.Expire + time.Second)
	h.expire()
	if d := find("desk"); d == nil || d.Gone || d.Title != "Three" {
		t.Fatalf("the connected one went: %+v", d)
	}
	if d := find("phone"); d == nil || !d.Gone || d.Title != "Two" || d.PositionMS != 5_000 {
		t.Fatalf("the phone: %+v", d)
	}
	c.add(h.Recent + time.Second)
	h.expire()
	if d := find("phone"); d != nil {
		t.Fatalf("kept past Recent: %+v", d)
	}
	h.Remove(1, "tab-a-000")
	h.EndSessions(1, -1)
	if n := len(h.Devices(1)); n != 0 {
		t.Fatalf("%d listed after every login ended", n)
	}
}

// A player that has played nothing tells its controls (review #211): listed idle with them, for a
// device controlling it to start from; still not a player (nothing for Discord, nothing kept once it
// goes). Told before it connected, it is not kept (it tells again once connected); an older report,
// or another account's, changes nothing.
func TestIdleControls(t *testing.T) {
	h := NewHub()
	changes, stop := h.Watch(1)
	defer stop()
	told := func() bool {
		select {
		case <-changes:
			return true
		default:
			return false
		}
	}
	only := func() Now {
		t.Helper()
		list := h.Devices(1)
		if len(list) != 1 {
			t.Fatalf("listed %+v", list)
		}
		return list[0]
	}
	idle := Report{Device: "phone", DeviceName: "Android · Chrome", Seq: 1, State: Idle, Control: Control{Volume: 0.6, Muted: true, Mode: "order"}}
	h.Put(1, 10, "tab-c-000", idle)
	conn, _ := h.Connect(1, 10, "tab-c-000", "phone", "Android · Chrome")
	told()
	if d := only(); d.State != Idle || d.Muted || d.Volume != 0 || d.Mode != "" {
		t.Fatalf("told before it connected: %+v", d)
	}
	idle.Seq = 2
	h.Put(1, 10, "tab-c-000", idle)
	if d := only(); !told() || d.State != Idle || !d.Muted || d.Volume != 0.6 || d.Mode != "order" || !d.Online || d.Title != "" {
		t.Fatalf("idle: %+v", d)
	}
	idle.Seq = 3
	if h.Put(1, 10, "tab-c-000", idle); told() {
		t.Fatal("the same again is news")
	}
	old := idle
	old.Seq, old.Muted = 2, false
	h.Put(1, 10, "tab-c-000", old)
	other := idle
	other.Seq, other.Muted = 9, false
	h.Put(2, 20, "tab-c-000", other)
	if d := only(); !d.Muted || told() {
		t.Fatalf("an older report, or another account's: %+v", d)
	}
	if len(h.Players(1)) != 0 || h.Pick(1, "") != nil {
		t.Fatal("an idle player shows on Discord")
	}
	// It plays: a player. It stops: idle again, with its controls as they are then.
	r := song(Playing, "One", 10_000)
	r.Seq, r.Device, r.Control = 4, "phone", Control{AssetID: 7, Count: 1, Volume: 0.6, Muted: true, Mode: "order"}
	h.Put(1, 10, "tab-c-000", r)
	if d := only(); d.State != Playing || d.Title != "One" {
		t.Fatalf("playing: %+v", d)
	}
	idle.Seq, idle.Muted, idle.Mode = 5, false, "shuffle"
	h.Put(1, 10, "tab-c-000", idle)
	if d := only(); d.State != Idle || d.Muted || d.Mode != "shuffle" || d.AssetID != 0 || d.Title != "" {
		t.Fatalf("idle again: %+v", d)
	}
	h.Disconnect(conn)
	if list := h.Devices(1); len(list) != 0 {
		t.Fatalf("kept once gone: %+v", list)
	}
}
