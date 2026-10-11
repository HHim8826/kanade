package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/HHim8826/kanade/server/internal/presence"
)

// Devices (review #138): an account's web players, which another of its devices can take the
// playback over from ("continue here") or control. Each player keeps an event stream open, down
// which come the account's devices as they change and the commands other players send it; it
// answers each command with an ack, which goes back to the player that sent it.

// maxControl is how large a command or an answer may be: a queue of 500 songs, twice (the order
// played and the list's own).
const maxControl = 4 << 20

// playerEvents is player pid's event stream: hello (the server's time, to judge when a command
// is late), players (the account's devices: at once and at each change), command and ack.
func (s *Server) playerEvents(w http.ResponseWriter, r *http.Request) {
	tab, device := r.PathValue("pid"), r.URL.Query().Get("device")
	if !playerID.MatchString(tab) || !playerID.MatchString(device) {
		writeError(w, http.StatusBadRequest, errors.New("bad player"))
		return
	}
	user := userID(r)
	c, err := s.presence.Connect(user, sessionID(r), tab, device, r.URL.Query().Get("name"))
	if errors.Is(err, presence.ErrTaken) {
		writeError(w, http.StatusConflict, err)
		return
	} else if err != nil {
		writeError(w, http.StatusTooManyRequests, err)
		return
	}
	defer s.presence.Disconnect(c)
	changes, unwatch := s.presence.Watch(user)
	defer unwatch()
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	send := func(name string, v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	ctx := r.Context()
	if !send("hello", map[string]any{"now": time.Now().UnixMilli(), "player": tab}) || !send("players", s.devices(ctx, user)) {
		return
	}
	// A comment now and then keeps proxies (Cloudflare closes a quiet one after 100 s) and the
	// browser sure the stream is alive.
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.Done():
			return
		case e := <-c.Events():
			if !send(e.Name, e.Data) {
				return
			}
		case <-changes:
			if !send("players", s.devices(ctx, user)) {
				return
			}
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

// listPlayers is the account's devices, as the event stream sends them.
func (s *Server) listPlayers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.devices(r.Context(), userID(r)))
}

func (s *Server) devices(ctx context.Context, user int64) []presence.Now {
	return s.named(ctx, user, s.presence.Devices(user))
}

// named gives players the names their browsers were given.
func (s *Server) named(ctx context.Context, user int64, list []presence.Now) []presence.Now {
	rows, err := s.db.QueryContext(ctx, `SELECT device, name FROM devices WHERE user_id = ?`, user)
	if err != nil {
		return list
	}
	defer rows.Close()
	names := map[string]string{}
	for rows.Next() {
		var d, n string
		if rows.Scan(&d, &n) == nil {
			names[d] = n
		}
	}
	for i := range list {
		if n := names[list[i].Device]; n != "" {
			list[i].DeviceName = n
		}
	}
	return list
}

// sendCommand sends player pid a command from another player of the account (from: the one to
// answer); 409 when pid is not connected.
func (s *Server) sendCommand(w http.ResponseWriter, r *http.Request) {
	var body struct {
		From string          `json:"from"`
		Op   string          `json:"op"`
		Args json.RawMessage `json:"args"`
	}
	if err := readJSONMax(r, &body, maxControl); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !presence.Ops[body.Op] || !playerID.MatchString(body.From) {
		writeError(w, http.StatusBadRequest, errors.New("bad command"))
		return
	}
	cmd, err := s.presence.Send(userID(r), body.From, r.PathValue("pid"), body.Op, body.Args)
	switch {
	case errors.Is(err, presence.ErrOffline):
		writeError(w, http.StatusConflict, err)
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, err)
	default:
		writeJSON(w, http.StatusAccepted, map[string]any{"id": cmd.ID, "seq": cmd.Seq})
	}
}

// ackCommand takes player pid's answer to a command it was sent.
func (s *Server) ackCommand(w http.ResponseWriter, r *http.Request) {
	var a presence.Ack
	if err := readJSONMax(r, &a, maxControl); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	a.Error = clipText(a.Error, 200)
	if !s.presence.Ack(userID(r), r.PathValue("pid"), a) {
		writeError(w, http.StatusGone, errors.New("no such command waiting"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// renameDevice names one of the account's browsers; an empty name gives it back its own.
func (s *Server) renameDevice(w http.ResponseWriter, r *http.Request) {
	device := r.PathValue("device")
	var body struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &body); err != nil || !playerID.MatchString(device) {
		writeError(w, http.StatusBadRequest, errors.New("bad device or name"))
		return
	}
	name := clipText(body.Name, 40)
	var err error
	if name == "" {
		_, err = s.db.ExecContext(r.Context(), `DELETE FROM devices WHERE user_id = ? AND device = ?`, userID(r), device)
	} else {
		_, err = s.db.ExecContext(r.Context(), `INSERT INTO devices (user_id, device, name) VALUES (?, ?, ?)
			ON CONFLICT (user_id, device) DO UPDATE SET name = excluded.name`, userID(r), device, name)
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	s.presence.Poke(userID(r))
	w.WriteHeader(http.StatusNoContent)
}

func clipText(s string, n int) string {
	s = strings.TrimSpace(strings.ToValidUTF8(s, ""))
	if utf8.RuneCountInString(s) > n {
		s = string([]rune(s)[:n])
	}
	return s
}

func readJSONMax(r *http.Request, v any, n int64) error {
	if err := json.NewDecoder(io.LimitReader(r.Body, n)).Decode(v); err != nil {
		return errors.New("invalid JSON body: " + err.Error())
	}
	return nil
}
