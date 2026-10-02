// Package identify looks albums up in MusicBrainz on request and turns a chosen release into
// proposed changes (plan §4 "assisted identification": candidates and differences are shown, the
// user picks what to apply, nothing is changed on its own).
package identify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MusicBrainz is a small client for the public web service. It needs no key, but asks for a
// User-Agent that names the application and a contact, and at most one request per second.
type MusicBrainz struct {
	Base     string // https://musicbrainz.org/ws/2
	CoverArt string // https://coverartarchive.org
	Log      *slog.Logger
	ua       string
	http     *http.Client
	interval time.Duration

	mu    sync.Mutex
	last  time.Time
	cache map[string]cached
}

type cached struct {
	body []byte
	at   time.Time
}

const cacheTTL = time.Hour

// New makes a client; contact is a URL or address MusicBrainz can reach the operator at.
func New(contact string) *MusicBrainz {
	return &MusicBrainz{
		Log:      slog.New(slog.DiscardHandler),
		Base:     "https://musicbrainz.org/ws/2",
		CoverArt: "https://coverartarchive.org",
		ua:       fmt.Sprintf("Kanade/0.2 ( %s )", contact),
		http:     &http.Client{Timeout: 20 * time.Second},
		interval: 1100 * time.Millisecond,
		cache:    map[string]cached{},
	}
}

var ErrUnavailable = errors.New("MusicBrainz did not answer; try again in a minute")

// wait spaces requests out to the service's rate limit.
func (m *MusicBrainz) wait(ctx context.Context) error {
	m.mu.Lock()
	next := m.last.Add(m.interval)
	if now := time.Now(); next.Before(now) {
		next = now
	}
	m.last = next
	m.mu.Unlock()
	t := time.NewTimer(time.Until(next))
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// get fetches a URL with the rate limit; limit caps the body size. JSON answers are cached for an
// hour (a search is usually followed by a lookup and an apply); images are not, to keep RAM small.
func (m *MusicBrainz) get(ctx context.Context, rawURL string, limit int64, keep bool) ([]byte, error) {
	m.mu.Lock()
	if c, ok := m.cache[rawURL]; ok && time.Since(c.at) < cacheTTL {
		m.mu.Unlock()
		return c.body, nil
	}
	m.mu.Unlock()
	if err := m.wait(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", m.ua)
	req.Header.Set("Accept", "application/json")
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errNotFound
	case resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return nil, ErrUnavailable
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("MusicBrainz: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("MusicBrainz answer too large")
	}
	if keep {
		m.mu.Lock()
		if len(m.cache) >= 100 {
			clear(m.cache)
		}
		m.cache[rawURL] = cached{body, time.Now()}
		m.mu.Unlock()
	}
	return body, nil
}

var errNotFound = errors.New("not found in MusicBrainz")

type credit struct {
	Name       string `json:"name"`
	JoinPhrase string `json:"joinphrase"`
}

func creditString(c []credit) string {
	var b strings.Builder
	for _, x := range c {
		b.WriteString(x.Name + x.JoinPhrase)
	}
	return strings.TrimSpace(b.String())
}

type mbTrack struct {
	Position     int      `json:"position"`
	Title        string   `json:"title"`
	Length       int64    `json:"length"` // ms; 0 when unknown
	ArtistCredit []credit `json:"artist-credit"`
	Recording    struct {
		ID string `json:"id"`
	} `json:"recording"`
}

type mbRelease struct {
	ID             string   `json:"id"`
	Score          int      `json:"score"`
	Title          string   `json:"title"`
	Disambiguation string   `json:"disambiguation"`
	Date           string   `json:"date"`
	Country        string   `json:"country"`
	Barcode        string   `json:"barcode"`
	Status         string   `json:"status"`
	TrackCount     int      `json:"track-count"`
	ArtistCredit   []credit `json:"artist-credit"`
	LabelInfo      []struct {
		CatalogNumber string `json:"catalog-number"`
		Label         *struct {
			Name string `json:"name"`
		} `json:"label"`
	} `json:"label-info"`
	Media []struct {
		Position   int       `json:"position"`
		Format     string    `json:"format"`
		TrackCount int       `json:"track-count"`
		Tracks     []mbTrack `json:"tracks"`
	} `json:"media"`
	ReleaseGroup struct {
		PrimaryType string `json:"primary-type"`
	} `json:"release-group"`
	CoverArtArchive struct {
		Front bool `json:"front"`
	} `json:"cover-art-archive"`
}

// Candidate is one release offered for an album.
type Candidate struct {
	ID             string `json:"id"`
	Score          int    `json:"score"`
	Title          string `json:"title"`
	Artist         string `json:"artist"`
	Date           string `json:"date"`
	Country        string `json:"country"`
	Label          string `json:"label"`
	Catalog        string `json:"catalog"`
	Barcode        string `json:"barcode"`
	Format         string `json:"format"` // e.g. "2×CD"
	Tracks         int    `json:"tracks"`
	Type           string `json:"type"`
	Disambiguation string `json:"disambiguation"`
}

func (r *mbRelease) candidate() Candidate {
	c := Candidate{ID: r.ID, Score: r.Score, Title: r.Title, Artist: creditString(r.ArtistCredit), Date: r.Date,
		Country: r.Country, Barcode: r.Barcode, Tracks: r.TrackCount, Type: r.ReleaseGroup.PrimaryType,
		Disambiguation: r.Disambiguation}
	for _, l := range r.LabelInfo {
		if c.Catalog == "" && l.CatalogNumber != "" {
			c.Catalog = l.CatalogNumber
		}
		if c.Label == "" && l.Label != nil {
			c.Label = l.Label.Name
		}
	}
	formats := map[string]int{}
	var order []string
	tracks := 0
	for _, m := range r.Media {
		if formats[m.Format] == 0 {
			order = append(order, m.Format)
		}
		formats[m.Format]++
		tracks += m.TrackCount
	}
	var parts []string
	for _, f := range order {
		if f == "" {
			f = "?"
		}
		if n := formats[f]; n > 1 {
			parts = append(parts, fmt.Sprintf("%d×%s", n, f))
		} else {
			parts = append(parts, f)
		}
	}
	c.Format = strings.Join(parts, " + ")
	if c.Tracks == 0 {
		c.Tracks = tracks
	}
	return c
}

var luceneSpecial = regexp.MustCompile(`([+\-&|!(){}\[\]^"~*?:\\/])`)

func quote(s string) string { return `"` + luceneSpecial.ReplaceAllString(s, `\$1`) + `"` }

// terms makes a field query in which any word may match, ranked by how many do: tags from rips
// rarely match a release title exactly ("Aria The Natural Op - Euforia" is the single "Euforia").
func terms(field, s string) string {
	var out []string
	for _, w := range strings.Fields(s) {
		if w = luceneSpecial.ReplaceAllString(w, `\$1`); w != `\-` && w != "" {
			out = append(out, w)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return field + ":(" + strings.Join(out, " ") + ")"
}

// Query is what an album search sends: only these names and numbers leave the server.
type Query struct {
	Title   string `json:"title"`
	Artist  string `json:"artist"`
	Catalog string `json:"catalog"`
	Year    string `json:"year,omitempty"`   // from the album's date
	Tracks  int    `json:"tracks,omitempty"` // songs on the album
}

var leadingYear = regexp.MustCompile(`^\d{4}`)

// Search finds releases for an album in up to three requests: releases by title words (and
// artist), or by catalog number; then the artist looked up on its own, which also matches its
// aliases and sort name, and that artist's releases from the album's year (or with as many tracks).
// Romaji tags of a Japanese release ("Euforia" by "Makino Yui") only meet the release
// (「ユーフォリア」 by 牧野由依) that second way. Matching track count and year rank first.
func (m *MusicBrainz) Search(ctx context.Context, q Query) ([]Candidate, error) {
	var parts []string
	artist := strings.TrimSpace(q.Artist)
	if strings.EqualFold(artist, "Various Artists") {
		artist = ""
	}
	if p := terms("release", q.Title); p != "" {
		if a := terms("artist", artist); a != "" {
			p += " AND " + a
		}
		parts = append(parts, "("+p+")")
	}
	if c := strings.TrimSpace(q.Catalog); c != "" {
		parts = append(parts, "catno:"+quote(c))
	}
	if len(parts) == 0 {
		return nil, errors.New("nothing to search for")
	}
	found, err := m.searchReleases(ctx, strings.Join(parts, " OR "))
	if err != nil {
		return nil, err
	}
	year := leadingYear.FindString(strings.TrimSpace(q.Year))
	if artist != "" && (year != "" || q.Tracks > 0) { // a failure here only means fewer candidates
		id, err := m.artistID(ctx, artist)
		if err != nil {
			m.Log.Warn("musicbrainz artist lookup", "err", err)
		}
		if id != "" {
			by := "arid:" + id + " AND tracks:" + strconv.Itoa(q.Tracks)
			if year != "" {
				by = "arid:" + id + " AND date:" + year
			}
			more, err := m.searchReleases(ctx, by)
			if err != nil {
				m.Log.Warn("musicbrainz releases by artist", "err", err)
			}
			found = append(found, more...)
		}
	}
	seen := map[string]bool{}
	out := []Candidate{}
	for _, c := range found {
		if !seen[c.ID] {
			seen[c.ID] = true
			out = append(out, c)
		}
	}
	rank := func(c Candidate) int {
		r := 0
		if q.Tracks > 0 && c.Tracks == q.Tracks {
			r += 2
		}
		if year != "" && strings.HasPrefix(c.Date, year) {
			r++
		}
		return r
	}
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) > rank(out[j]) })
	if len(out) > 15 {
		out = out[:15]
	}
	return out, nil
}

func (m *MusicBrainz) searchReleases(ctx context.Context, query string) ([]Candidate, error) {
	u := m.Base + "/release?" + url.Values{"query": {query}, "fmt": {"json"}, "limit": {"10"}}.Encode()
	body, err := m.get(ctx, u, 4<<20, true)
	if err != nil {
		return nil, err
	}
	var res struct {
		Releases []mbRelease `json:"releases"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("MusicBrainz search: %w", err)
	}
	out := make([]Candidate, 0, len(res.Releases))
	for i := range res.Releases {
		out = append(out, res.Releases[i].candidate())
	}
	return out, nil
}

// artistID is the MusicBrainz artist a name most likely means, or "" when no match is close.
func (m *MusicBrainz) artistID(ctx context.Context, name string) (string, error) {
	u := m.Base + "/artist?" + url.Values{"query": {strings.TrimSpace(name)}, "fmt": {"json"}, "limit": {"1"}}.Encode()
	body, err := m.get(ctx, u, 1<<20, true)
	if err != nil {
		return "", err
	}
	var res struct {
		Artists []struct {
			ID    string `json:"id"`
			Score int    `json:"score"`
		} `json:"artists"`
	}
	if err := json.Unmarshal(body, &res); err != nil || len(res.Artists) == 0 || res.Artists[0].Score < 90 {
		return "", err
	}
	return res.Artists[0].ID, nil
}

var mbid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// release fetches one release with its tracks.
func (m *MusicBrainz) release(ctx context.Context, id string) (*mbRelease, error) {
	if !mbid.MatchString(id) {
		return nil, errors.New("not a MusicBrainz release ID")
	}
	body, err := m.get(ctx, m.Base+"/release/"+id+"?inc=recordings+artist-credits+labels+release-groups&fmt=json", 4<<20, true)
	if err != nil {
		return nil, err
	}
	var r mbRelease
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("MusicBrainz release: %w", err)
	}
	return &r, nil
}

// FrontCover downloads a release's front cover (500 px) from the Cover Art Archive.
func (m *MusicBrainz) FrontCover(ctx context.Context, id string) ([]byte, error) {
	if !mbid.MatchString(id) {
		return nil, errors.New("not a MusicBrainz release ID")
	}
	return m.get(ctx, m.CoverArt+"/release/"+id+"/front-500", 16<<20, false)
}
