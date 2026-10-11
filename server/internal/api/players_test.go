package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type sse struct {
	t      *testing.T
	res    *http.Response
	events chan [2]string
}

// openEvents opens player tab's event stream.
func openEvents(t *testing.T, base, token, tab, device string) *sse {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/api/v1/players/"+tab+"/events?device="+device+"&name=Linux%20%C2%B7%20Firefox", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	s := &sse{t: t, res: res, events: make(chan [2]string, 100)}
	t.Cleanup(func() { res.Body.Close() })
	go func() {
		defer close(s.events)
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(nil, 8<<20)
		name := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				name = line[7:]
			case strings.HasPrefix(line, "data: "):
				s.events <- [2]string{name, line[6:]}
			}
		}
	}()
	return s
}

// wait is the next event named name (others skipped); ok false when the stream ended first.
func (s *sse) wait(name string) (data string, ok bool) {
	s.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case e, open := <-s.events:
			if !open {
				return "", false
			}
			if e[0] == name {
				return e[1], true
			}
		case <-timeout:
			s.t.Fatalf("no %s event", name)
		}
	}
}

func post(t *testing.T, base, token, path string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", base+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res
}

// Devices over HTTP (review #138): a player's event stream starts with the server's time and the
// account's devices, and sends them again as they change (a device named, a player telling); a
// command another player sends arrives on it, and the answer, data and all, arrives on the
// sender's; a player not connected is refused; logging out closes that login's streams.
func TestPlayerEvents(t *testing.T) {
	s, h := newTestServer(t)
	tok := loginToken(t, s, h)
	other := do(t, h, "POST", "/api/v1/login", "", map[string]string{"username": "admin", "password": "correct horse battery"})
	var lr struct{ Token string }
	json.Unmarshal(other.Body.Bytes(), &lr)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close) // after the streams close (cleanups run last first): it waits for them

	desk := openEvents(t, srv.URL, tok, "tab-desk-01", "deskdevice01")
	if hello, _ := desk.wait("hello"); !strings.Contains(hello, `"now":`) {
		t.Fatalf("hello %s", hello)
	}
	if list, _ := desk.wait("players"); !strings.Contains(list, `"player":"tab-desk-01"`) || !strings.Contains(list, `"state":"idle"`) {
		t.Fatalf("first list %s", list)
	}
	phone := openEvents(t, srv.URL, lr.Token, "tab-phone-1", "phonedevice1")
	phone.wait("hello")
	if list, _ := desk.wait("players"); !strings.Contains(list, "tab-phone-1") {
		t.Fatalf("the phone not listed: %s", list)
	}
	if rec := do(t, h, "PUT", "/api/v1/devices/phonedevice1", tok, map[string]string{"name": "  我的手機  "}); rec.Code != 204 {
		t.Fatalf("rename %d", rec.Code)
	}
	if list, _ := desk.wait("players"); !strings.Contains(list, `"device_name":"我的手機"`) {
		t.Fatalf("named: %s", list)
	}
	report := map[string]any{"device": "phonedevice1", "device_name": "Android · Chrome", "seq": 1, "state": "playing", "title": "Undine",
		"duration_ms": 200000, "position_ms": 1000, "asset_id": 5, "qid": 2, "queue": "q-1", "count": 3, "volume": 0.7}
	do(t, h, "PUT", "/api/v1/presence/players/tab-phone-1", lr.Token, report)
	if list, _ := desk.wait("players"); !strings.Contains(list, `"title":"Undine"`) || !strings.Contains(list, `"device_name":"我的手機"`) ||
		!strings.Contains(list, `"online":true`) || !strings.Contains(list, `"queue":"q-1"`) {
		t.Fatalf("playing: %s", list)
	}

	// The desk asks the phone for its queue; the phone answers.
	res := post(t, srv.URL, tok, "/api/v1/players/tab-phone-1/commands", map[string]any{"from": "tab-desk-01", "op": "snapshot"})
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("send %d", res.StatusCode)
	}
	cmd, _ := phone.wait("command")
	var c struct {
		Seq     int64
		Op      string
		From    string
		Expires int64
	}
	json.Unmarshal([]byte(cmd), &c)
	if c.Op != "snapshot" || c.From != "tab-desk-01" || c.Expires < time.Now().UnixMilli() {
		t.Fatalf("command %s", cmd)
	}
	snap := map[string]any{"queue": []map[string]any{{"qid": 2, "assetId": 5, "title": "Undine"}}, "index": 0}
	if res := post(t, srv.URL, lr.Token, "/api/v1/players/tab-phone-1/acks", map[string]any{"seq": c.Seq, "ok": true, "data": snap}); res.StatusCode != 204 {
		t.Fatalf("ack %d", res.StatusCode)
	}
	if ack, _ := desk.wait("ack"); !strings.Contains(ack, `"ok":true`) || !strings.Contains(ack, `"title":"Undine"`) {
		t.Fatalf("answer %s", ack)
	}
	if res := post(t, srv.URL, lr.Token, "/api/v1/players/tab-phone-1/acks", map[string]any{"seq": c.Seq, "ok": true}); res.StatusCode != http.StatusGone {
		t.Fatalf("answered twice: %d", res.StatusCode)
	}
	for _, bad := range []map[string]any{{"from": "tab-desk-01", "op": "format"}, {"from": "x", "op": "pause"}} {
		if res := post(t, srv.URL, tok, "/api/v1/players/tab-phone-1/commands", bad); res.StatusCode != http.StatusBadRequest {
			t.Fatalf("%v: %d", bad, res.StatusCode)
		}
	}
	// The whole library or a smart playlist is picked on the device that plays it (review #204).
	for _, op := range []string{"shuffle", "smart"} {
		if res := post(t, srv.URL, tok, "/api/v1/players/tab-phone-1/commands", map[string]any{"from": "tab-desk-01", "op": op}); res.StatusCode != http.StatusAccepted {
			t.Fatalf("%s: %d", op, res.StatusCode)
		}
	}
	if res := post(t, srv.URL, tok, "/api/v1/players/tab-none-01/commands", map[string]any{"from": "tab-desk-01", "op": "pause"}); res.StatusCode != http.StatusConflict {
		t.Fatalf("not connected: %d", res.StatusCode)
	}

	// The phone logs out: its stream ends, and the desk sees it gone.
	if res := post(t, srv.URL, lr.Token, "/api/v1/logout", nil); res.StatusCode >= 300 {
		t.Fatalf("logout %d", res.StatusCode)
	}
	if _, ok := phone.wait("never"); ok {
		t.Fatal("the stream went on")
	}
	if list, _ := desk.wait("players"); strings.Contains(list, "tab-phone-1") {
		t.Fatalf("still listed: %s", list)
	}
	if res := post(t, srv.URL, tok, "/api/v1/players/tab-phone-1/commands", map[string]any{"from": "tab-desk-01", "op": "pause"}); res.StatusCode != http.StatusConflict {
		t.Fatalf("sent after logout: %d", res.StatusCode)
	}
}
