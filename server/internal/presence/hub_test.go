package presence

import (
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func song(state, title string, pos int64) Report {
	return Report{Device: "desk", DeviceName: "Windows · Chrome", State: state, Title: title, Artist: "A", Album: "B", DurationMS: 200_000, PositionMS: pos}
}

// Which player a companion shows (review #135): the one that last started playing, never taking
// turns as players tell again; a paused one only when none plays; a following one only its
// browser's. Late reports are dropped, heartbeats are no change, a jump is; players that stop
// telling or whose login ended go.
func TestHubPicks(t *testing.T) {
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
	pick := func(device string) string {
		if n := h.Pick(1, device); n != nil {
			return n.Player + ":" + n.State + ":" + n.Title
		}
		return "none"
	}

	r := song(Playing, "One", 0)
	r.Seq = 1
	h.Put(1, 10, "tab-a-000", r)
	if !told() || pick("") != "tab-a-000:playing:One" {
		t.Fatalf("first: %s", pick(""))
	}
	c.add(5 * time.Second)
	b := song(Playing, "Two", 0)
	b.Seq, b.Device = 1, "phone"
	h.Put(1, 11, "tab-b-000", b)
	if pick("") != "tab-b-000:playing:Two" || pick("desk") != "tab-a-000:playing:One" || pick("tv") != "none" {
		t.Fatalf("the last started: %s; desk %s", pick(""), pick("desk"))
	}
	// A heartbeat: where the clock puts it, so no change, and no turn taken.
	told()
	c.add(30 * time.Second)
	r.Seq, r.PositionMS = 2, 35_500
	h.Put(1, 10, "tab-a-000", r)
	if told() || pick("") != "tab-b-000:playing:Two" {
		t.Fatalf("a heartbeat changed things: %s", pick(""))
	}
	if n := h.Pick(1, "desk"); n.PositionMS != 35_500 {
		t.Fatalf("position %d", n.PositionMS)
	}
	c.add(2 * time.Second)
	if n := h.Pick(1, "desk"); n.PositionMS != 37_500 {
		t.Fatalf("the position moves on: %d", n.PositionMS)
	}
	// A seek is a change; a late report is dropped.
	r.Seq, r.PositionMS = 3, 120_000
	h.Put(1, 10, "tab-a-000", r)
	if !told() {
		t.Fatal("a seek is a change")
	}
	late := r
	late.Seq, late.State = 2, Paused
	if h.Put(1, 10, "tab-a-000", late) || h.Pick(1, "desk").State != Playing {
		t.Fatal("an older report was taken")
	}
	if h.Put(2, 20, "tab-a-000", song(Playing, "x", 0)) {
		t.Fatal("another user's player")
	}
	// Played again from the start: tab a started last.
	c.add(time.Second)
	r.Seq, r.Title, r.PositionMS = 4, "Three", 0
	h.Put(1, 10, "tab-a-000", r)
	if pick("") != "tab-a-000:playing:Three" {
		t.Fatalf("another song: %s", pick(""))
	}
	// Paused: the other one, playing, is shown; both paused, the last paused.
	r.Seq, r.State = 5, Paused
	h.Put(1, 10, "tab-a-000", r)
	if pick("") != "tab-b-000:playing:Two" {
		t.Fatalf("paused: %s", pick(""))
	}
	c.add(time.Second)
	b.Seq, b.State = 2, Paused
	h.Put(1, 11, "tab-b-000", b)
	if pick("") != "tab-b-000:paused:Two" {
		t.Fatalf("both paused: %s", pick(""))
	}
	// A login ended: its players go; the rest stop telling and go too.
	h.EndSessions(1, 0, 11)
	if pick("") != "tab-a-000:paused:Three" {
		t.Fatalf("after the phone logged out: %s", pick(""))
	}
	c.add(91 * time.Second)
	h.expire()
	if pick("") != "none" || len(h.Players(1)) != 0 {
		t.Fatalf("expired: %s", pick(""))
	}
	r.Seq = 6
	h.Put(1, 10, "tab-a-000", r)
	r.Seq, r.State = 7, Stopped
	h.Put(1, 10, "tab-a-000", r)
	if pick("") != "none" {
		t.Fatal("stopped is gone")
	}
}

func TestShownBy(t *testing.T) {
	n := &Now{State: Playing, Title: "T", Artist: "A", Album: "B", DurationMS: 100, PositionMS: 40}
	if s := ShownBy(n, DefaultShow); s.Artist != "A" || s.Album != "B" || s.PositionMS != 40 || s.DurationMS != 100 {
		t.Fatalf("all: %+v", s)
	}
	if s := ShownBy(n, Show{}); s.Artist != "" || s.Album != "" || s.DurationMS != 0 || s.Title != "T" {
		t.Fatalf("title only: %+v", s)
	}
	p := *n
	p.State = Paused
	if s := ShownBy(&p, DefaultShow); s.State != "none" {
		t.Fatalf("paused, cleared: %+v", s)
	}
	if s := ShownBy(&p, Show{Paused: "show", Time: true}); s.State != Paused || s.DurationMS != 0 {
		t.Fatalf("paused, shown without time: %+v", s)
	}
	if s := ShownBy(nil, DefaultShow); s.State != "none" {
		t.Fatal(s)
	}
}
