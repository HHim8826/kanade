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
	// In a collection's subject (a slim one) these stand for the rating and the summary.
	Score        float64 `json:"score"`
	Rank         int     `json:"rank"`
	ShortSummary string  `json:"short_summary"`
	Tags         []Tag   `json:"tags"` // the most used, first
}

// Tag is a tag people put on a subject, and how many did.
type Tag struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
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
	var p Page
	if _, err := c.do(ctx, http.MethodPost, u, b, func(data []byte) bool {
		var check struct {
			Total *int            `json:"total"`
			Data  json.RawMessage `json:"data"`
		}
		p = Page{}
		return json.Unmarshal(data, &check) == nil && check.Total != nil && json.Unmarshal(data, &p) == nil
	}); err != nil {
		return nil, err
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
	var s Subject
	if _, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/v0/subjects/%d", c.Base, id), nil, func(data []byte) bool {
		s = Subject{}
		return json.Unmarshal(data, &s) == nil && s.ID == id
	}); err != nil {
		return nil, err
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

// do asks the API, or answers as it did a while ago. An answer is taken, and kept, only when ok
// says it is one: an error page sent as a success is not kept for the next asking (review #179).
func (c *Client) do(ctx context.Context, method, rawURL string, body []byte, ok func([]byte) bool) ([]byte, error) {
	key := method + " " + rawURL + " " + string(body)
	c.mu.Lock()
	if h, found := c.cache[key]; found && time.Since(h.at) < cacheTTL && ok(h.body) {
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
	if !ok(data) {
		return nil, fmt.Errorf("%w (an answer that is not what was asked for)", ErrUnavailable)
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

// ---- the user's own account (OAuth) and collections ----

// Collection types, as Bangumi numbers them (for music: 想聽, 聽過, 在聽, 擱置, 拋棄).
const (
	Wish    = 1
	Done    = 2
	Doing   = 3
	OnHold  = 4
	Dropped = 5
)

// ErrAuth is a link Bangumi no longer accepts: it must be made again.
var ErrAuth = errors.New("Bangumi no longer accepts this link")

// App is the Bangumi application an account is linked with.
type App struct {
	ID, Secret, RedirectURI string
}

func (a App) Ready() bool { return a.ID != "" && a.Secret != "" && a.RedirectURI != "" }

type Token struct {
	Access, Refresh string
	Expires         time.Time
	UserID          int64
}

// AuthorizeURL is where the person agrees to the link.
func (c *Client) AuthorizeURL(app App, state string) string {
	q := url.Values{"client_id": {app.ID}, "response_type": {"code"}, "redirect_uri": {app.RedirectURI}, "state": {state}}
	return c.Site + "/oauth/authorize?" + q.Encode()
}

func (c *Client) token(ctx context.Context, app App, form url.Values) (*Token, error) {
	form.Set("client_id", app.ID)
	form.Set("client_secret", app.Secret)
	form.Set("redirect_uri", app.RedirectURI)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Site+"/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var r struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		UserID       any    `json:"user_id"`
		Error        string `json:"error"`
		Description  string `json:"error_description"`
	}
	json.Unmarshal(body, &r)
	switch {
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized:
		return nil, fmt.Errorf("%w: %s %s", ErrAuth, r.Error, r.Description)
	case resp.StatusCode != http.StatusOK || r.AccessToken == "":
		return nil, fmt.Errorf("%w (HTTP %d)", ErrUnavailable, resp.StatusCode)
	}
	t := &Token{Access: r.AccessToken, Refresh: r.RefreshToken, Expires: time.Now().Add(time.Duration(r.ExpiresIn) * time.Second)}
	switch v := r.UserID.(type) { // a number, or a string of one
	case float64:
		t.UserID = int64(v)
	case string:
		t.UserID, _ = strconv.ParseInt(v, 10, 64)
	}
	return t, nil
}

// Exchange trades the code Bangumi sent back for tokens.
func (c *Client) Exchange(ctx context.Context, app App, code string) (*Token, error) {
	return c.token(ctx, app, url.Values{"grant_type": {"authorization_code"}, "code": {code}})
}

// Refresh renews the tokens before they expire.
func (c *Client) Refresh(ctx context.Context, app App, refresh string) (*Token, error) {
	return c.token(ctx, app, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
}

// authed asks the API as the linked person: never cached, never spaced with others' requests
// beyond the usual gap.
func (c *Client) authed(ctx context.Context, method, rawURL, access string, body any) ([]byte, int, error) {
	if err := c.wait(ctx); err != nil {
		return nil, 0, err
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+access)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, fmt.Errorf("%w (%v)", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("%w (%v)", ErrUnavailable, err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, resp.StatusCode, ErrAuth
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return nil, resp.StatusCode, fmt.Errorf("%w (HTTP %d)", ErrUnavailable, resp.StatusCode)
	}
	return data, resp.StatusCode, nil
}

// Me is the linked person.
type Me struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Nickname string `json:"nickname"`
}

func (c *Client) Me(ctx context.Context, access string) (*Me, error) {
	data, code, err := c.authed(ctx, http.MethodGet, c.Base+"/v0/me", access, nil)
	if err != nil {
		return nil, err
	}
	var m Me
	if code != http.StatusOK || json.Unmarshal(data, &m) != nil || m.Username == "" {
		return nil, fmt.Errorf("%w (HTTP %d)", ErrUnavailable, code)
	}
	return &m, nil
}

// Collection is a person's collecting of a subject.
type Collection struct {
	SubjectID int64    `json:"subject_id"`
	Type      int      `json:"type"`
	Rate      int      `json:"rate"`
	Comment   string   `json:"comment"`
	Private   bool     `json:"private"`
	Tags      []string `json:"tags"`
	UpdatedAt string   `json:"updated_at"`
	Subject   *Subject `json:"subject,omitempty"`
}

// Collection is how username collected a subject; nil when they have not.
func (c *Client) Collection(ctx context.Context, access, username string, subject int64) (*Collection, error) {
	data, code, err := c.authed(ctx, http.MethodGet, fmt.Sprintf("%s/v0/users/%s/collections/%d", c.Base, url.PathEscape(username), subject), access, nil)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound {
		return nil, nil
	}
	var col Collection
	if code != http.StatusOK || json.Unmarshal(data, &col) != nil {
		return nil, fmt.Errorf("%w (HTTP %d)", ErrUnavailable, code)
	}
	if col.Tags == nil {
		col.Tags = []string{}
	}
	return &col, nil
}

// CollectionChange is what to set of a collection; nil fields stay as they are.
type CollectionChange struct {
	Type    *int      `json:"type,omitempty"`
	Rate    *int      `json:"rate,omitempty"`
	Comment *string   `json:"comment,omitempty"`
	Private *bool     `json:"private,omitempty"`
	Tags    *[]string `json:"tags,omitempty"`
}

// ErrRefused is Bangumi refusing a change, with why.
type ErrRefused struct{ Message string }

func (e *ErrRefused) Error() string { return "Bangumi refused the change: " + e.Message }

// SetCollection collects a subject, or changes how it is collected.
func (c *Client) SetCollection(ctx context.Context, access string, subject int64, ch CollectionChange) error {
	data, code, err := c.authed(ctx, http.MethodPost, fmt.Sprintf("%s/v0/users/-/collections/%d", c.Base, subject), access, ch)
	if err != nil {
		return err
	}
	if code == http.StatusNoContent || code == http.StatusOK || code == http.StatusAccepted {
		return nil
	}
	var e struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	}
	json.Unmarshal(data, &e)
	return &ErrRefused{Message: strings.TrimSpace(e.Title + " " + e.Description)}
}

// CollectionPage is a page of a person's collections.
type CollectionPage struct {
	Total int          `json:"total"`
	Data  []Collection `json:"data"`
}

// Collections are username's collections of subjects of a type (0: all), of a collection type (0:
// all), from offset.
func (c *Client) Collections(ctx context.Context, access, username string, subjectType, collectionType, limit, offset int) (*CollectionPage, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)}}
	if subjectType > 0 {
		q.Set("subject_type", strconv.Itoa(subjectType))
	}
	if collectionType > 0 {
		q.Set("type", strconv.Itoa(collectionType))
	}
	data, code, err := c.authed(ctx, http.MethodGet, fmt.Sprintf("%s/v0/users/%s/collections?%s", c.Base, url.PathEscape(username), q.Encode()), access, nil)
	if err != nil {
		return nil, err
	}
	var p CollectionPage
	if code != http.StatusOK || json.Unmarshal(data, &p) != nil {
		return nil, fmt.Errorf("%w (HTTP %d)", ErrUnavailable, code)
	}
	for i := range p.Data {
		if p.Data[i].Tags == nil {
			p.Data[i].Tags = []string{}
		}
		if p.Data[i].Subject != nil {
			c.seen(p.Data[i].Subject)
		}
	}
	if p.Data == nil {
		p.Data = []Collection{}
	}
	return &p, nil
}
