// Package bangumi looks works (anime, games, books…) up in Bangumi (bgm.tv) on request (review
// #94): searching its subjects, reading one, and fetching its picture. It needs no account; the API
// asks for a User-Agent that names the application, and requests are spaced out. Nothing here
// changes the library: the user picks the work to link.
package bangumi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Types are Bangumi's kinds of subject.
const (
	Book  = 1
	Anime = 2
	Music = 3
	Game  = 4
	Real  = 6
)

var (
	ErrUnavailable = errors.New("Bangumi did not answer; try again in a minute")
	ErrNotFound    = errors.New("no such subject in Bangumi")
	ErrImage       = errors.New("not a picture of Bangumi's")
)

type Client struct {
	Base string // https://api.bgm.tv
	Site string // https://bgm.tv, for links to a subject
	ua   string
	http *http.Client
	Gap  time.Duration // between requests to the API

	mu     sync.Mutex
	last   time.Time
	cache  map[string]cached // answers by request, for a while
	images map[int64]string  // subjects' pictures seen lately, for their candidates' thumbnails
}

type cached struct {
	body []byte
	at   time.Time
}

const (
	cacheTTL  = time.Hour
	maxCached = 200
	maxImages = 2000
	maxAnswer = 4 << 20
)

// New makes a client; version is Kanade's, for the User-Agent Bangumi asks for.
func New(version string) *Client {
	return &Client{
		Base:   "https://api.bgm.tv",
		Site:   "https://bgm.tv",
		ua:     "HHim8826/kanade/" + version + " (https://github.com/HHim8826/kanade)",
		http:   &http.Client{Timeout: 20 * time.Second},
		Gap:    500 * time.Millisecond,
		cache:  map[string]cached{},
		images: map[int64]string{},
	}
}

// Subject is a subject as Bangumi gives it. Rating fields are zero when it has none.
type Subject struct {
	ID       int64  `json:"id"`
	Type     int    `json:"type"`
	Name     string `json:"name"`
	NameCN   string `json:"name_cn"`
	Summary  string `json:"summary"`
	Date     string `json:"date"`
	Platform string `json:"platform"`
	NSFW     bool   `json:"nsfw"`
	Images   *struct {
		Large  string `json:"large"`
		Common string `json:"common"`
		Medium string `json:"medium"`
		Small  string `json:"small"`
		Grid   string `json:"grid"`
	} `json:"images"`
	Rating *struct {
		Rank  int     `json:"rank"`
		Total int     `json:"total"`
		Score float64 `json:"score"`
	} `json:"rating"`
}

// Image is the subject's picture, at the largest size Bangumi has; empty when it has none.
func (s *Subject) Image() string {
	if s.Images == nil {
		return ""
	}
	for _, u := range []string{s.Images.Large, s.Images.Common, s.Images.Medium} {
		if u != "" {
			return u
		}
	}
	return ""
}

// URL is the subject's page on the site.
func (c *Client) URL(id int64) string { return c.Site + "/subject/" + strconv.FormatInt(id, 10) }

// Page is one page of a search.
type Page struct {
	Total    int       `json:"total"`
	Subjects []Subject `json:"data"`
}

// PageSize is how many subjects a search page has.
const PageSize = 20

// Search finds subjects matching keyword, of the given types (all when none), from offset.
func (c *Client) Search(ctx context.Context, keyword string, types []int, offset int) (*Page, error) {
	body := map[string]any{"keyword": keyword, "sort": "match"}
	if len(types) > 0 {
		body["filter"] = map[string]any{"type": types}
	}
	b, _ := json.Marshal(body)
	u := fmt.Sprintf("%s/v0/search/subjects?limit=%d&offset=%d", c.Base, PageSize, max(offset, 0))
	data, err := c.do(ctx, http.MethodPost, u, b)
	if err != nil {
		return nil, err
	}
	var p Page
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("%w (%v)", ErrUnavailable, err)
	}
	if p.Subjects == nil {
		p.Subjects = []Subject{}
	}
	for i := range p.Subjects {
		c.seen(&p.Subjects[i])
	}
	return &p, nil
}

// Subject reads one subject.
func (c *Client) Subject(ctx context.Context, id int64) (*Subject, error) {
	data, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/v0/subjects/%d", c.Base, id), nil)
	if err != nil {
		return nil, err
	}
	var s Subject
	if err := json.Unmarshal(data, &s); err != nil || s.ID == 0 {
		return nil, fmt.Errorf("%w (an answer that is no subject)", ErrUnavailable)
	}
	c.seen(&s)
	return &s, nil
}

func (c *Client) seen(s *Subject) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.images) >= maxImages {
		clear(c.images)
	}
	c.images[s.ID] = s.Image()
}

// ImageOf is the picture of a subject, from one seen lately or else read again.
func (c *Client) ImageOf(ctx context.Context, id int64) (string, error) {
	c.mu.Lock()
	u, ok := c.images[id]
	c.mu.Unlock()
	if ok {
		return u, nil
	}
	s, err := c.Subject(ctx, id)
	if err != nil {
		return "", err
	}
	return s.Image(), nil
}

// OpenImage reads one of Bangumi's pictures; any other address is refused, so it is no way to
// make the server fetch what a client names.
func (c *Client) OpenImage(ctx context.Context, rawURL string) (io.ReadCloser, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || !(u.Hostname() == "bgm.tv" || strings.HasSuffix(u.Hostname(), ".bgm.tv")) {
		return nil, ErrImage
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("%w (HTTP %d)", ErrUnavailable, resp.StatusCode)
	}
	return resp.Body, nil
}

// wait spaces requests out.
func (c *Client) wait(ctx context.Context) error {
	c.mu.Lock()
	next := c.last.Add(c.Gap)
	if now := time.Now(); next.Before(now) {
		next = now
	}
	c.last = next
	c.mu.Unlock()
	t := time.NewTimer(time.Until(next))
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) do(ctx context.Context, method, rawURL string, body []byte) ([]byte, error) {
	key := method + " " + rawURL + " " + string(body)
	c.mu.Lock()
	if h, ok := c.cache[key]; ok && time.Since(h.at) < cacheTTL {
		c.mu.Unlock()
		return h.body, nil
	}
	c.mu.Unlock()
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w (%v)", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%w (HTTP %d)", ErrUnavailable, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", ErrUnavailable, err)
	}
	if len(data) > maxAnswer {
		return nil, fmt.Errorf("%w (answer too large)", ErrUnavailable)
	}
	c.mu.Lock()
	if len(c.cache) >= maxCached {
		clear(c.cache)
	}
	c.cache[key] = cached{data, time.Now()}
	c.mu.Unlock()
	return data, nil
}

// Forget drops what is kept of a subject, so the next read asks Bangumi again.
func (c *Client) Forget(id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cache, fmt.Sprintf("%s %s/v0/subjects/%d ", http.MethodGet, c.Base, id))
	delete(c.images, id)
}

var subjectLink = regexp.MustCompile(`^(?:https?://)?(?:www\.)?(?:bgm\.tv|bangumi\.tv|chii\.in)/subject/(\d+)(?:[/?#].*)?$`)

// Ref is the subject a link to one (bgm.tv, bangumi.tv or chii.in) or its plain number names.
func Ref(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if m := subjectLink.FindStringSubmatch(s); m != nil {
		s = m[1]
	}
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil && id > 0 && id < 1<<40
}
