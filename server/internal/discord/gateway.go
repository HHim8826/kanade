package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// The gateway: a WebSocket on which Discord says hello (op 10, with how often to beat), the client
// identifies (op 2) with the link's token, beats (op 1, answered by op 11) and, once ready (op 0
// READY), sets its presence (op 3). Discord may ask to reconnect (op 7) or say the session is no
// good (op 9); a refused token closes with 4004.

const (
	opDispatch       = 0
	opHeartbeat      = 1
	opIdentify       = 2
	opPresence       = 3
	opReconnect      = 7
	opInvalidSession = 9
	opHello          = 10
	opHeartbeatAck   = 11
)

// Presence is what to show: a status (online, idle or dnd) and an activity (nil: none).
type Presence struct {
	Status   string
	Activity map[string]any
}

// latest holds the presence to show, for a connection to take whenever it is ready.
type latest struct {
	mu      sync.Mutex
	p       *Presence
	changed chan struct{}
}

func newLatest() *latest { return &latest{changed: make(chan struct{}, 1)} }

func (l *latest) set(p Presence) {
	l.mu.Lock()
	l.p = &p
	l.mu.Unlock()
	select {
	case l.changed <- struct{}{}:
	default:
	}
}

func (l *latest) get() *Presence {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.p
}

type Gateway struct {
	URL    string        // wss://gateway.discord.gg/?v=10&encoding=json
	MinGap time.Duration // between presence updates: Discord limits them
	Log    *slog.Logger
}

func NewGateway() *Gateway {
	return &Gateway{URL: "wss://gateway.discord.gg/?v=10&encoding=json", MinGap: 5 * time.Second, Log: slog.New(slog.DiscardHandler)}
}

type frame struct {
	Op int             `json:"op"`
	S  *int64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
	D  json.RawMessage `json:"d"`
}

var errReconnect = errors.New("Discord asked to connect again")

// connect keeps one connection with token, showing what want holds, until ctx ends (nil) or the
// connection does (an error; ErrAuth when Discord refuses the token). ready is called once Discord
// has accepted it.
func (g *Gateway) connect(ctx context.Context, token string, want *latest, ready func()) error {
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	conn, _, err := websocket.Dial(dctx, g.URL, &websocket.DialOptions{HTTPHeader: http.Header{"User-Agent": {"Kanade (https://github.com/HHim8826/kanade)"}}})
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("%w (%v)", ErrUnavailable, err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(16 << 20)
	frames := make(chan frame, 16)
	readErr := make(chan error, 1)
	go func() {
		for {
			_, b, err := conn.Read(ctx)
			if err != nil {
				readErr <- err
				return
			}
			var f frame
			if json.Unmarshal(b, &f) != nil {
				continue
			}
			select {
			case frames <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	send := func(op int, d any) error {
		b, _ := json.Marshal(map[string]any{"op": op, "d": d})
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return conn.Write(wctx, websocket.MessageText, b)
	}
	closed := func(err error) error {
		switch websocket.CloseStatus(err) {
		case 4004:
			return ErrAuth
		case -1:
			return fmt.Errorf("%w (%v)", ErrUnavailable, err)
		default:
			return fmt.Errorf("%w (closed: %v)", ErrUnavailable, err)
		}
	}

	var interval time.Duration
	select {
	case <-ctx.Done():
		conn.Close(websocket.StatusNormalClosure, "")
		return nil
	case err := <-readErr:
		return closed(err)
	case f := <-frames:
		var hello struct {
			Interval int64 `json:"heartbeat_interval"`
		}
		if f.Op != opHello || json.Unmarshal(f.D, &hello) != nil || hello.Interval <= 0 {
			return fmt.Errorf("%w (no hello)", ErrUnavailable)
		}
		interval = time.Duration(hello.Interval) * time.Millisecond
	case <-time.After(30 * time.Second):
		return fmt.Errorf("%w (no hello)", ErrUnavailable)
	}
	if err := send(opIdentify, map[string]any{"token": "Bearer " + token, "intents": 0,
		"properties": map[string]string{"os": "linux", "browser": "Kanade", "device": "Kanade"}}); err != nil {
		return closed(err)
	}

	var seq *int64
	beat := time.NewTimer(time.Duration(rand.Float64() * float64(interval)))
	defer beat.Stop()
	acked := true
	isReady := false
	var sent *Presence
	var sentAt time.Time
	flush := time.NewTimer(time.Hour)
	flush.Stop()
	defer flush.Stop()
	show := func() error {
		p := want.get()
		if !isReady || p == nil || sent == p {
			return nil
		}
		if wait := g.MinGap - time.Since(sentAt); wait > 0 {
			flush.Reset(wait)
			return nil
		}
		acts := []any{}
		if p.Activity != nil {
			acts = append(acts, p.Activity)
		}
		if err := send(opPresence, map[string]any{"since": 0, "activities": acts, "status": p.Status, "afk": false}); err != nil {
			return err
		}
		sent, sentAt = p, time.Now()
		return nil
	}
	for {
		var err error
		select {
		case <-ctx.Done():
			conn.Close(websocket.StatusNormalClosure, "")
			return nil
		case err := <-readErr:
			return closed(err)
		case f := <-frames:
			if f.S != nil {
				seq = f.S
			}
			switch f.Op {
			case opDispatch:
				if f.T == "READY" {
					isReady = true
					if ready != nil {
						ready()
					}
					err = show()
				}
			case opHeartbeat:
				err = send(opHeartbeat, seq)
			case opHeartbeatAck:
				acked = true
			case opReconnect:
				return errReconnect
			case opInvalidSession:
				return fmt.Errorf("%w (session refused)", ErrUnavailable)
			}
		case <-beat.C:
			if !acked {
				return fmt.Errorf("%w (no heartbeat answer)", ErrUnavailable)
			}
			acked = false
			err = send(opHeartbeat, seq)
			beat.Reset(interval)
		case <-want.changed:
			err = show()
		case <-flush.C:
			err = show()
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return closed(err)
		}
	}
}
