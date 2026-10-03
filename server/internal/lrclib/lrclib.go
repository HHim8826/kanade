// Package lrclib looks lyrics up in LRCLIB (lrclib.net), an open database of synced and plain
// lyrics that needs no key. Only a song's title and artist are sent, and only for
// a song whose lyrics someone is looking at.
package lrclib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/unicode/norm"
)

type Client struct {
	Base   string // https://lrclib.net/api
	ua     string
	http   *http.Client
	second time.Duration // a second of Retry-After (shorter in tests)

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	body []byte
	at   time.Time
}

const cacheTTL = time.Hour

// New makes a client; contact goes in the User-Agent, as LRCLIB asks.
func New(contact string) *Client {
	return &Client{Base: "https://lrclib.net/api", ua: fmt.Sprintf("Kanade/0.2 ( %s )", contact),
		http: &http.Client{Timeout: 20 * time.Second}, second: time.Second, cache: map[string]cached{}}
}

const (
	// busyTries is how many times a request LRCLIB was too busy for is sent again: it answers about
	// one uncached search in four with 503 "ServerOverloaded" and Retry-After: 1.
	busyTries = 3
	// longestWait is the longest Retry-After (seconds) waited for; a longer one fails at once.
	longestWait = 5
)

var (
	ErrUnavailable = errors.New("LRCLIB did not answer; try again later")
	ErrNotFound    = errors.New("LRCLIB has no such lyrics")
)

// Lyrics is one LRCLIB record.
type Lyrics struct {
	ID           int64   `json:"id"`
	Title        string  `json:"trackName"`
	Artist       string  `json:"artistName"`
	Album        string  `json:"albumName"`
	Duration     float64 `json:"duration"` // seconds
	Instrumental bool    `json:"instrumental"`
	Plain        string  `json:"plainLyrics"`
	Synced       string  `json:"syncedLyrics"`
}

// Text is the synced lyrics when there are, else the plain ones.
func (l *Lyrics) Text() string {
	if strings.TrimSpace(l.Synced) != "" {
		return l.Synced
	}
	return l.Plain
}

// get fetches path and reads the answer with decode. Only an answer decode accepts is cached: an
// error page sent with 200 is asked again next time, not kept for an hour (review #80).
func (c *Client) get(ctx context.Context, path string, q url.Values, decode func([]byte) error) error {
	u := c.Base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	c.mu.Lock()
	if e, ok := c.cache[u]; ok && time.Since(e.at) < cacheTTL {
		c.mu.Unlock()
		return decode(e.body)
	}
	c.mu.Unlock()
	body, err := c.fetch(ctx, u)
	if err != nil {
		return err
	}
	if err := decode(body); err != nil {
		return fmt.Errorf("LRCLIB: unreadable answer: %w", err)
	}
	c.mu.Lock()
	if len(c.cache) >= 200 {
		clear(c.cache)
	}
	c.cache[u] = cached{body, time.Now()}
	c.mu.Unlock()
	return nil
}

// fetch asks LRCLIB, and again after the wait it asks for when it is busy.
func (c *Client) fetch(ctx context.Context, u string) ([]byte, error) {
	for try := 0; ; try++ {
		body, wait, err := c.fetchOnce(ctx, u)
		if wait == 0 || try == busyTries {
			return body, err
		}
		t := time.NewTimer(time.Duration(wait) * c.second)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, fmt.Errorf("%w (%v)", ErrUnavailable, ctx.Err())
		case <-t.C:
		}
	}
}

// fetchOnce sends one request; wait > 0 is how many seconds to wait before asking again.
func (c *Client) fetchOnce(ctx context.Context, u string) (body []byte, wait int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%w (%v)", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, 0, ErrNotFound
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return nil, busyWait(resp), ErrUnavailable
	case resp.StatusCode != http.StatusOK:
		return nil, 0, fmt.Errorf("LRCLIB: HTTP %d", resp.StatusCode)
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, 4<<20+1))
	if err != nil {
		return nil, 0, err
	}
	if len(body) > 4<<20 {
		return nil, 0, errors.New("LRCLIB answer too large")
	}
	return body, 0, nil
}

// busyWait is the wait before asking again after a busy answer (429, 502–504): its Retry-After
// when that is short, a second without one; 0 (not again) for another error or a long wait.
func busyWait(resp *http.Response) int {
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
	default:
		return 0
	}
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 1
	}
	n, err := strconv.Atoi(v)
	if err != nil || n > longestWait {
		return 0
	}
	return max(n, 1)
}

// Get fetches one record by its ID.
func (c *Client) Get(ctx context.Context, id int64) (*Lyrics, error) {
	var l Lyrics
	err := c.get(ctx, "/get/"+strconv.FormatInt(id, 10), nil, func(body []byte) error {
		l = Lyrics{}
		if err := json.Unmarshal(body, &l); err != nil {
			return err
		}
		if l.ID != id {
			return fmt.Errorf("asked for record %d, got %d", id, l.ID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &l, nil
}

func (c *Client) search(ctx context.Context, q url.Values) ([]Lyrics, error) {
	var list []Lyrics
	err := c.get(ctx, "/search", q, func(body []byte) error {
		list = nil
		if !strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
			return errors.New("not a list")
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return err
		}
		for _, l := range list {
			if l.ID <= 0 {
				return errors.New("a record without an ID")
			}
		}
		return nil
	})
	return list, err
}

// Song is what is known of the song lyrics are looked for.
type Song struct {
	Title, Artist, Album string
	DurationMS           int64
}

// Candidate is a record with how well it fits the song.
type Candidate struct {
	ID           int64   `json:"id"`
	Title        string  `json:"title"`
	Artist       string  `json:"artist"`
	Album        string  `json:"album"`
	Duration     float64 `json:"duration"`
	Synced       bool    `json:"synced"`
	Instrumental bool    `json:"instrumental"`
	Preview      string  `json:"preview"` // the first lines, without time tags
	// Exact: the same title and artist (when the song has one) and a length within two seconds,
	// close enough to use without asking.
	Exact bool `json:"exact"`
	off   float64
}

// Find searches by title and artist (then title alone when that finds nothing), and ranks what it
// finds: records with words before instrumental ones, exact matches first, then synced lyrics,
// then the closest length. At most ten.
func (c *Client) Find(ctx context.Context, s Song) ([]Candidate, error) {
	if strings.TrimSpace(s.Title) == "" {
		return nil, errors.New("the song has no title")
	}
	q := url.Values{"track_name": {s.Title}}
	if s.Artist != "" {
		q.Set("artist_name", s.Artist)
	}
	list, err := c.search(ctx, q)
	if err == nil && len(list) == 0 && s.Artist != "" {
		list, err = c.search(ctx, url.Values{"track_name": {s.Title}})
	}
	if err != nil {
		return nil, err
	}
	return rank(s, list), nil
}

func rank(s Song, list []Lyrics) []Candidate {
	out := make([]Candidate, 0, len(list))
	seen := map[int64]bool{}
	for _, l := range list {
		if seen[l.ID] || (l.Text() == "" && !l.Instrumental) {
			continue
		}
		seen[l.ID] = true
		off := math.Inf(1)
		if s.DurationMS > 0 && l.Duration > 0 {
			off = math.Abs(l.Duration - float64(s.DurationMS)/1000)
		}
		c := Candidate{ID: l.ID, Title: l.Title, Artist: l.Artist, Album: l.Album, Duration: l.Duration,
			Synced: strings.TrimSpace(l.Synced) != "", Instrumental: l.Instrumental, Preview: preview(l.Text()), off: off}
		c.Exact = off <= 2 && same(l.Title, s.Title) && (s.Artist == "" || same(l.Artist, s.Artist))
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Instrumental != b.Instrumental { // records with words first
			return b.Instrumental
		}
		if a.Exact != b.Exact {
			return a.Exact
		}
		if a.Synced != b.Synced {
			return a.Synced
		}
		return a.off < b.off
	})
	return out[:min(len(out), 10)]
}

// same compares names loosely: width, case and spacing aside.
func same(a, b string) bool {
	f := func(s string) string { return strings.Join(strings.Fields(strings.ToLower(norm.NFKC.String(s))), " ") }
	return f(a) == f(b)
}

// preview is the first lines of lyrics, without their time tags.
func preview(text string) string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		for strings.HasPrefix(line, "[") {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				break
			}
			line = line[end+1:]
		}
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
		if len(lines) == 4 {
			break
		}
	}
	return strings.Join(lines, "\n")
}
