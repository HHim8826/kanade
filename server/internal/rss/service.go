package rss

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/HHim8826/kanade/server/internal/db"
)

const (
	DefaultInterval = 30   // minutes (plan §3)
	MinInterval     = 10   //
	maxInterval     = 1440 //
	maxBackoff      = 6 * time.Hour
	keepItems       = 1000 // per source
	maxAutoPerPoll  = 5    // a rule that suddenly matches everything does not flood the queue
	maxTorrentBytes = 10 << 20
)

// Downloader starts BitTorrent downloads; auto picks the suggested files without asking.
type Downloader interface {
	Add(ctx context.Context, uri string, torrent []byte, auto bool) (int64, error)
}

type Service struct {
	db   *sql.DB
	dl   Downloader
	http *http.Client
	ua   string
	log  *slog.Logger
	wake chan struct{}

	mu      sync.Mutex
	polling map[int64]*sync.Mutex // one poll at a time per source: background and "refresh now" take turns
}

// lockSource serializes polls of one source.
func (s *Service) lockSource(id int64) func() {
	s.mu.Lock()
	if s.polling == nil {
		s.polling = map[int64]*sync.Mutex{}
	}
	m := s.polling[id]
	if m == nil {
		m = &sync.Mutex{}
		s.polling[id] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
}

func New(d *sql.DB, dl Downloader, contact string, log *slog.Logger) *Service {
	return &Service{db: d, dl: dl, log: log, wake: make(chan struct{}, 1),
		ua: fmt.Sprintf("Kanade/0.2 ( %s )", contact),
		http: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DialContext: safeDial, Proxy: http.ProxyFromEnvironment},
			CheckRedirect: checkRedirect}}
}

type sourceKey struct{}

// checkRedirect lets a source's login and cookie follow a redirect only to a site allowed to have
// them; Go's own rule (any subdomain, and http after https) is wider.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	req.Header.Del("Authorization")
	req.Header.Del("Cookie")
	if src, ok := req.Context().Value(sourceKey{}).(*Source); ok {
		src.credentials(req)
	}
	return nil
}

// safeDial keeps feed fetches off this machine's own services (aria2's RPC, the app itself).
func safeDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if ip.IP.IsLoopback() || ip.IP.IsUnspecified() {
			return nil, errors.New("feeds on this machine are not allowed")
		}
	}
	return (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
}

var (
	ErrNotFound = errors.New("no such source")
	ErrInvalid  = errors.New("invalid source")
)

// Source is a feed as the client sees it: the password and cookie are only reported as set.
type Source struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	URL          string `json:"url"`
	IntervalMin  int    `json:"interval_min"`
	Enabled      bool   `json:"enabled"`
	AutoDownload bool   `json:"auto_download"`
	Include      string `json:"include"`
	Exclude      string `json:"exclude"`
	AuthUser     string `json:"auth_user"`
	AuthOrigins  string `json:"auth_origins"` // other sites that get the login and cookie, one per line
	HasPassword  bool   `json:"has_password"`
	HasCookie    bool   `json:"has_cookie"`
	Searchable   bool   `json:"searchable"` // the URL takes a q= search term
	Baseline     bool   `json:"baseline"`   // the next fetch only records what is there
	NextPollAt   int64  `json:"next_poll_at"`
	LastPollAt   int64  `json:"last_poll_at"`
	LastOKAt     int64  `json:"last_ok_at"`
	Failures     int    `json:"failures"`
	LastError    string `json:"last_error,omitempty"`
	Items        int    `json:"items"`
	Matched      int    `json:"matched"` // items the include rules match (computed)
	pass, cookie string
	etag, lm     string
	updatedAt    int64 // changes with every edit: a poll judges what it fetched by the settings of now
}

const sourceCols = `id, name, url, interval_min, enabled, auto_download, include_rules, exclude_rules, auth_user, auth_origins, auth_pass, cookie,
	etag, last_modified, baseline, next_poll_at, last_poll_at, last_ok_at, failures, last_error, updated_at,
	(SELECT count(*) FROM rss_items i WHERE i.source_id = rss_sources.id)`

func scanSource(sc interface{ Scan(...any) error }) (*Source, error) {
	var s Source
	err := sc.Scan(&s.ID, &s.Name, &s.URL, &s.IntervalMin, &s.Enabled, &s.AutoDownload, &s.Include, &s.Exclude, &s.AuthUser,
		&s.AuthOrigins, &s.pass, &s.cookie, &s.etag, &s.lm, &s.Baseline, &s.NextPollAt, &s.LastPollAt, &s.LastOKAt, &s.Failures, &s.LastError, &s.updatedAt, &s.Items)
	if err != nil {
		return nil, err
	}
	s.HasPassword, s.HasCookie = s.pass != "", s.cookie != ""
	if u, err := url.Parse(s.URL); err == nil {
		s.Searchable = u.Query().Has("q")
	}
	return &s, nil
}

func (s *Service) Sources(ctx context.Context) ([]Source, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sourceCols+` FROM rss_sources ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Source{}
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *src)
	}
	return out, rows.Err()
}

func (s *Service) source(ctx context.Context, id int64) (*Source, error) {
	src, err := scanSource(s.db.QueryRowContext(ctx, `SELECT `+sourceCols+` FROM rss_sources WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return src, err
}

func (s *Service) Source(ctx context.Context, id int64) (*Source, error) { return s.source(ctx, id) }

// SourceInput creates or changes a source; nil fields stay as they are. An empty password or
// cookie in an edit keeps the stored one; Clear* removes it.
type SourceInput struct {
	Name         *string `json:"name"`
	URL          *string `json:"url"`
	IntervalMin  *int    `json:"interval_min"`
	Enabled      *bool   `json:"enabled"`
	AutoDownload *bool   `json:"auto_download"`
	Include      *string `json:"include"`
	Exclude      *string `json:"exclude"`
	AuthUser     *string `json:"auth_user"`
	AuthOrigins  *string `json:"auth_origins"`
	AuthPass     *string `json:"auth_pass"`
	Cookie       *string `json:"cookie"`
	ClearAuth    bool    `json:"clear_auth"`
	ClearCookie  bool    `json:"clear_cookie"`
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func checkURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", invalid("the address must be an http or https URL")
	}
	return raw, nil
}

func cleanRules(s string) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.Join(strings.Fields(l), " "); l != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}

// apply merges an input into a source, checking it.
func (in SourceInput) apply(src *Source) error {
	if in.Name != nil {
		src.Name = strings.TrimSpace(*in.Name)
	}
	if in.URL != nil {
		u, err := checkURL(*in.URL)
		if err != nil {
			return err
		}
		if u != src.URL {
			src.etag, src.lm = "", ""
		}
		src.URL = u
	}
	if in.IntervalMin != nil {
		src.IntervalMin = *in.IntervalMin
	}
	if in.Enabled != nil {
		src.Enabled = *in.Enabled
	}
	if in.AutoDownload != nil {
		if *in.AutoDownload && !src.AutoDownload {
			src.Baseline = true // only what appears from now on is downloaded
		}
		src.AutoDownload = *in.AutoDownload
	}
	if in.Include != nil {
		src.Include = cleanRules(*in.Include)
	}
	if in.Exclude != nil {
		src.Exclude = cleanRules(*in.Exclude)
	}
	if in.AuthUser != nil {
		src.AuthUser = strings.TrimSpace(*in.AuthUser)
	}
	if in.AuthOrigins != nil {
		var list []string
		for _, l := range strings.Fields(*in.AuthOrigins) {
			u, err := url.Parse(l)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return invalid("%q is not a site address like https://dl.example.org", l)
			}
			list = append(list, origin(u))
		}
		src.AuthOrigins = strings.Join(list, "\n")
	}
	if in.AuthPass != nil && *in.AuthPass != "" {
		src.pass = *in.AuthPass
	}
	if in.Cookie != nil && strings.TrimSpace(*in.Cookie) != "" {
		src.cookie = strings.TrimSpace(*in.Cookie)
	}
	if in.ClearAuth {
		src.AuthUser, src.pass = "", ""
	}
	if in.ClearCookie {
		src.cookie = ""
	}
	switch {
	case src.Name == "" || utf8.RuneCountInString(src.Name) > 200:
		return invalid("a name of up to 200 characters is needed")
	case src.URL == "":
		return invalid("the address is needed")
	case src.IntervalMin < MinInterval || src.IntervalMin > maxInterval:
		return invalid("the interval is %d to %d minutes", MinInterval, maxInterval)
	case src.AutoDownload && !hasRules(src.Include):
		return invalid("automatic download needs at least one include rule")
	}
	return nil
}

// Create adds a source and fetches it right away; that first fetch is the baseline.
func (s *Service) Create(ctx context.Context, in SourceInput) (*Source, error) {
	src := &Source{IntervalMin: DefaultInterval, Enabled: true, Baseline: true}
	if err := in.apply(src); err != nil {
		return nil, err
	}
	now := db.Now()
	r, err := s.db.ExecContext(ctx, `INSERT INTO rss_sources (name, url, interval_min, enabled, auto_download, include_rules, exclude_rules,
		auth_user, auth_origins, auth_pass, cookie, baseline, next_poll_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?)`,
		src.Name, src.URL, src.IntervalMin, src.Enabled, src.AutoDownload, src.Include, src.Exclude, src.AuthUser, src.AuthOrigins, src.pass, src.cookie,
		now, now, now)
	if err != nil {
		return nil, err
	}
	id, _ := r.LastInsertId()
	s.Wake()
	return s.source(ctx, id)
}

func (s *Service) Update(ctx context.Context, id int64, in SourceInput) (*Source, error) {
	src, err := s.source(ctx, id)
	if err != nil {
		return nil, err
	}
	wasEnabled, wasAuto := src.Enabled, src.AutoDownload
	if err := in.apply(src); err != nil {
		return nil, err
	}
	if wasAuto && !src.AutoDownload { // what waited for auto-download is let go; turned on again, only new items count
		if _, err := s.db.ExecContext(ctx, `UPDATE rss_items SET auto_state = '' WHERE source_id = ? AND auto_state = 'pending'`, id); err != nil {
			return nil, err
		}
	}
	next := src.NextPollAt
	if src.Enabled && !wasEnabled {
		next = db.Now()
	}
	_, err = s.db.ExecContext(ctx, `UPDATE rss_sources SET name = ?, url = ?, interval_min = ?, enabled = ?, auto_download = ?,
		include_rules = ?, exclude_rules = ?, auth_user = ?, auth_origins = ?, auth_pass = ?, cookie = ?, etag = ?, last_modified = ?, baseline = ?,
		next_poll_at = ?, updated_at = max(?, updated_at + 1) WHERE id = ?`,
		src.Name, src.URL, src.IntervalMin, src.Enabled, src.AutoDownload, src.Include, src.Exclude, src.AuthUser, src.AuthOrigins, src.pass, src.cookie,
		src.etag, src.lm, src.Baseline, next, db.Now(), id)
	if err != nil {
		return nil, err
	}
	s.Wake()
	return s.source(ctx, id)
}

func (s *Service) Delete(ctx context.Context, id int64) error {
	r, err := s.db.ExecContext(ctx, `DELETE FROM rss_sources WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run polls due sources one at a time until ctx ends.
func (s *Service) Run(ctx context.Context) {
	for {
		var id, next int64
		err := s.db.QueryRowContext(ctx, `SELECT id, next_poll_at FROM rss_sources WHERE enabled = 1 ORDER BY next_poll_at, id LIMIT 1`).
			Scan(&id, &next)
		wait := 5 * time.Minute
		if err == nil {
			if d := time.Until(time.UnixMilli(next)); d <= 0 {
				if _, err := s.Poll(ctx, id); err != nil && ctx.Err() == nil {
					s.log.Info("rss fetch failed", "source", id, "err", err)
				}
				continue
			} else if d < wait {
				wait = d
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-time.After(wait):
		}
	}
}

// PollResult is what one fetch brought.
type PollResult struct {
	Fetched    int `json:"fetched"`
	New        int `json:"new"`
	Downloaded int `json:"downloaded"` // started by auto-download
}

// Poll fetches a source now, records new items and, when auto-download is on and the fetch is
// not a baseline, queues the new items its rules include and starts what is queued.
//
// Polls of one source take turns, and what a fetch brought is judged by the source's settings as
// they are when it returns (review #22): a changed address discards it; auto-download turned off
// meanwhile downloads nothing; one turned on meanwhile keeps its baseline for the next fetch.
func (s *Service) Poll(ctx context.Context, id int64) (*PollResult, error) {
	defer s.lockSource(id)()
	src, err := s.source(ctx, id)
	if err != nil {
		return nil, err
	}
	now := db.Now()
	entries, etag, lm, notModified, ferr := s.fetch(ctx, src, src.URL, true)
	cur, err := s.source(ctx, id)
	if err != nil {
		return nil, err // deleted meanwhile
	}
	if cur.URL != src.URL { // the answer is from the old address: look again at the new one
		s.db.ExecContext(ctx, `UPDATE rss_sources SET next_poll_at = ? WHERE id = ?`, db.Now(), id)
		s.Wake()
		return &PollResult{}, ferr
	}
	if ferr != nil {
		// Exponential backoff: one interval after the first failure, doubling, at most 6 hours.
		backoff := time.Duration(cur.IntervalMin) * time.Minute << min(cur.Failures, 10)
		backoff = min(backoff, maxBackoff)
		s.db.ExecContext(ctx, `UPDATE rss_sources SET last_poll_at = ?, failures = failures + 1, last_error = ?, next_poll_at = ? WHERE id = ?`,
			now, ferr.Error(), now+backoff.Milliseconds(), id)
		res := &PollResult{}
		s.runPending(ctx, cur, res) // what is queued can still start
		return res, ferr
	}
	changed := cur.updatedAt != src.updatedAt
	baseline := src.Baseline || cur.Baseline // items present while a baseline was due are never downloaded
	res := &PollResult{Fetched: len(entries)}
	if !notModified {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			pending := ""
			if cur.AutoDownload && !baseline && e.Download != "" && Match(cur.Include, cur.Exclude, e.Title) == "included" {
				pending = "pending"
			}
			r, err := tx.ExecContext(ctx, `INSERT INTO rss_items (source_id, guid, title, page, download, info_hash, size, seeders,
				published_at, first_seen_at, baseline, auto_state) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (source_id, guid) DO NOTHING`,
				id, e.GUID, e.Title, e.Page, e.Download, e.InfoHash, e.Size, e.Seeders, e.Published, now, baseline, pending)
			if err != nil {
				tx.Rollback()
				return nil, err
			}
			if n, _ := r.RowsAffected(); n > 0 {
				res.New++
			} else if e.Seeders >= 0 { // a known item: only its live numbers change
				tx.ExecContext(ctx, `UPDATE rss_items SET seeders = ? WHERE source_id = ? AND guid = ?`, e.Seeders, id, e.GUID)
			}
		}
		// Keep the newest items, every item a download came from, and what waits for auto-download.
		if _, err := tx.ExecContext(ctx, `DELETE FROM rss_items WHERE source_id = ? AND download_id IS NULL AND auto_state != 'pending'
			AND id NOT IN (SELECT id FROM rss_items WHERE source_id = ? ORDER BY first_seen_at DESC, id DESC LIMIT ?)`, id, id, keepItems); err != nil {
			tx.Rollback()
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
	next := now + time.Duration(float64(cur.IntervalMin)*float64(time.Minute)*(0.9+0.2*rand.Float64())).Milliseconds()
	if changed {
		next = db.Now() // settings changed during the fetch: look again with them
		s.Wake()
	}
	// A baseline set during the fetch stays for the next one.
	s.db.ExecContext(ctx, `UPDATE rss_sources SET etag = ?, last_modified = ?, baseline = CASE WHEN updated_at = ? THEN 0 ELSE baseline END,
		last_poll_at = ?, last_ok_at = ?, failures = 0, last_error = '', next_poll_at = ? WHERE id = ?`,
		etag, lm, src.updatedAt, now, now, next, id)
	s.runPending(ctx, cur, res)
	return res, nil
}

const autoMaxTries = 8

// runPending starts the source's queued auto-downloads, at most maxAutoPerPoll a poll, oldest
// first. A failure waits and tries again later (doubling from 10 minutes, at most 6 hours); after
// autoMaxTries it is shown as failed. Items the rules no longer pick leave the queue.
func (s *Service) runPending(ctx context.Context, src *Source, res *PollResult) {
	if !src.AutoDownload {
		return
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, auto_tries FROM rss_items WHERE source_id = ? AND auto_state = 'pending' AND auto_next <= ?
		ORDER BY first_seen_at, id LIMIT 50`, src.ID, db.Now())
	if err != nil {
		return
	}
	type queued struct {
		id    int64
		tries int
	}
	var list []queued
	for rows.Next() {
		var q queued
		if rows.Scan(&q.id, &q.tries) == nil {
			list = append(list, q)
		}
	}
	rows.Close()
	for _, q := range list {
		if res.Downloaded >= maxAutoPerPoll {
			return
		}
		it, err := s.item(ctx, q.id)
		if err != nil || it == nil {
			continue
		}
		if it.Downloaded {
			s.db.ExecContext(ctx, `UPDATE rss_items SET auto_state = 'done', auto_error = '' WHERE id = ?`, q.id)
			continue
		}
		if Match(src.Include, src.Exclude, it.Title) != "included" || it.Download == "" {
			s.db.ExecContext(ctx, `UPDATE rss_items SET auto_state = '' WHERE id = ?`, q.id)
			continue
		}
		if _, err := s.startDownload(ctx, src, it, true); err != nil {
			s.log.Warn("rss auto-download", "item", q.id, "err", err)
			state, wait := "pending", min(10*time.Minute<<min(q.tries, 10), maxBackoff)
			if q.tries+1 >= autoMaxTries {
				state = "failed"
			}
			s.db.ExecContext(ctx, `UPDATE rss_items SET auto_state = ?, auto_tries = auto_tries + 1, auto_next = ?, auto_error = ? WHERE id = ?`,
				state, db.Now()+wait.Milliseconds(), err.Error(), q.id)
			continue
		}
		s.db.ExecContext(ctx, `UPDATE rss_items SET auto_state = 'done', auto_error = '' WHERE id = ?`, q.id)
		res.Downloaded++
	}
}

// fetch gets and parses a feed. conditional sends the stored ETag and Last-Modified.
func (s *Service) fetch(ctx context.Context, src *Source, rawURL string, conditional bool) (entries []Entry, etag, lm string, notModified bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", "", false, err
	}
	req = s.authorize(req, src)
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml;q=0.9, */*;q=0.5")
	if conditional {
		if src.etag != "" {
			req.Header.Set("If-None-Match", src.etag)
		}
		if src.lm != "" {
			req.Header.Set("If-Modified-Since", src.lm)
		}
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, "", "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, src.etag, src.lm, true, nil
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, "", "", false, fmt.Errorf("the feed answered HTTP %d", resp.StatusCode)
	}
	entries, err = Parse(resp.Body)
	return entries, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), false, err
}

// authorize prepares a request for a source: the login and cookie go only to the source's own site
// (same scheme, host and port) and the sites listed for it, and keep to that on redirects (review
// #17). A torrent link on another site gets neither.
func (s *Service) authorize(req *http.Request, src *Source) *http.Request {
	req.Header.Set("User-Agent", s.ua)
	src.credentials(req)
	return req.WithContext(context.WithValue(req.Context(), sourceKey{}, src))
}

func (src *Source) credentials(req *http.Request) {
	if !src.trusts(req.URL) {
		return
	}
	if src.AuthUser != "" || src.pass != "" {
		req.SetBasicAuth(src.AuthUser, src.pass)
	}
	if src.cookie != "" {
		req.Header.Set("Cookie", src.cookie)
	}
}

// trusts reports whether a site may receive the source's login and cookie.
func (src *Source) trusts(u *url.URL) bool {
	o := origin(u)
	if su, err := url.Parse(src.URL); err == nil && origin(su) == o {
		return true
	}
	for _, l := range strings.Split(src.AuthOrigins, "\n") {
		if l != "" && l == o {
			return true
		}
	}
	return false
}

// origin is scheme://host[:port], lower-cased, without a default port.
func origin(u *url.URL) string {
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host
}

// Item is a feed item with its standing against the source's rules and the downloads.
type Item struct {
	ID       int64  `json:"id,omitempty"` // 0 for a live search result
	SourceID int64  `json:"source_id"`
	Source   string `json:"source"`
	Entry
	FirstSeen  int64  `json:"first_seen_at,omitempty"`
	Match      string `json:"match"`                 // included | excluded | ""
	DownloadID int64  `json:"download_id,omitempty"` // the download started from it, or of the same torrent
	Downloaded bool   `json:"downloaded"`
	AutoState  string `json:"auto_state,omitempty"` // pending | done | failed (review #23)
	AutoError  string `json:"auto_error,omitempty"`
}

const itemSQL = `SELECT i.id, i.source_id, s.name, i.guid, i.title, i.page, i.download, i.info_hash, i.size, i.seeders,
	i.published_at, i.first_seen_at, s.include_rules, s.exclude_rules, coalesce(i.download_id, 0), i.auto_state, i.auto_error
	FROM rss_items i JOIN rss_sources s ON s.id = i.source_id`

func (s *Service) scanItems(ctx context.Context, rows *sql.Rows) ([]Item, error) {
	defer rows.Close()
	out := []Item{}
	for rows.Next() {
		var it Item
		var include, exclude string
		if err := rows.Scan(&it.ID, &it.SourceID, &it.Source, &it.GUID, &it.Title, &it.Page, &it.Download, &it.InfoHash, &it.Size,
			&it.Seeders, &it.Published, &it.FirstSeen, &include, &exclude, &it.DownloadID, &it.AutoState, &it.AutoError); err != nil {
			return nil, err
		}
		it.Match = Match(include, exclude, it.Title)
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		s.markDownloaded(ctx, &out[i])
	}
	return out, nil
}

// markDownloaded finds a download of the same torrent, from any source or added by hand.
func (s *Service) markDownloaded(ctx context.Context, it *Item) {
	if it.DownloadID == 0 && (it.InfoHash != "" || it.Download != "") {
		s.db.QueryRowContext(ctx, `SELECT id FROM downloads WHERE (? != '' AND lower(info_hash) = ?) OR (? != '' AND source = ?)
			ORDER BY id DESC LIMIT 1`, it.InfoHash, it.InfoHash, it.Download, it.Download).Scan(&it.DownloadID)
	}
	it.Downloaded = it.DownloadID != 0
}

func (s *Service) item(ctx context.Context, id int64) (*Item, error) {
	rows, err := s.db.QueryContext(ctx, itemSQL+` WHERE i.id = ?`, id)
	if err != nil {
		return nil, err
	}
	list, err := s.scanItems(ctx, rows)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

// ItemQuery filters stored items: by source, by words in the title (normalized like the
// library search), only those the rules include; newest first, paged by item ID.
type ItemQuery struct {
	Source int64
	Q      string
	Only   string // "included" to see only rule matches
	Limit  int
	Before int64
}

func (s *Service) Items(ctx context.Context, q ItemQuery) ([]Item, error) {
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 100
	}
	where, args := []string{"1 = 1"}, []any{}
	if q.Source != 0 {
		where, args = append(where, "i.source_id = ?"), append(args, q.Source)
	}
	if q.Before > 0 {
		where, args = append(where, "i.id < ?"), append(args, q.Before)
	}
	rows, err := s.db.QueryContext(ctx, itemSQL+` WHERE `+strings.Join(where, " AND ")+` ORDER BY i.id DESC`, args...)
	if err != nil {
		return nil, err
	}
	all, err := s.scanItems(ctx, rows)
	if err != nil {
		return nil, err
	}
	out := []Item{}
	for _, it := range all {
		if (q.Q == "" || rulesMatch(q.Q, it.Title)) && (q.Only == "" || it.Match == q.Only) {
			out = append(out, it)
			if len(out) == q.Limit {
				break
			}
		}
	}
	return out, nil
}

// Search asks a source that takes a q= term for this term, live; nothing is stored (plan §3: the
// site's search, not a promise of complete history).
func (s *Service) Search(ctx context.Context, id int64, term string) ([]Item, error) {
	src, err := s.source(ctx, id)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(src.URL)
	if err != nil || !u.Query().Has("q") {
		return nil, invalid("this source has no search")
	}
	v := u.Query()
	v.Set("q", strings.TrimSpace(term))
	u.RawQuery = v.Encode()
	entries, _, _, _, err := s.fetch(ctx, src, u.String(), false)
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(entries))
	for _, e := range entries {
		it := Item{SourceID: src.ID, Source: src.Name, Entry: e, Match: Match(src.Include, src.Exclude, e.Title)}
		s.db.QueryRowContext(ctx, `SELECT coalesce(download_id, 0) FROM rss_items WHERE source_id = ? AND guid = ?`, src.ID, e.GUID).
			Scan(&it.DownloadID)
		s.markDownloaded(ctx, &it)
		out = append(out, it)
	}
	return out, nil
}

// Download starts a download from a stored item, or from a live search result (by its link), the
// user choosing the files.
func (s *Service) Download(ctx context.Context, itemID int64) (int64, error) {
	it, err := s.item(ctx, itemID)
	if err != nil {
		return 0, err
	}
	if it == nil {
		return 0, ErrNotFound
	}
	src, err := s.source(ctx, it.SourceID)
	if err != nil {
		return 0, err
	}
	return s.startDownload(ctx, src, it, false)
}

// DownloadLink starts a download from a live search result of a source.
func (s *Service) DownloadLink(ctx context.Context, sourceID int64, link string) (int64, error) {
	src, err := s.source(ctx, sourceID)
	if err != nil {
		return 0, err
	}
	return s.startDownload(ctx, src, &Item{Entry: Entry{Download: link}}, false)
}

// startDownload fetches a .torrent with the source's credentials (private trackers need them) and
// hands it to the downloader; magnets go as they are.
func (s *Service) startDownload(ctx context.Context, src *Source, it *Item, auto bool) (int64, error) {
	link := strings.TrimSpace(it.Download)
	var torrent []byte
	switch {
	case isMagnet(link):
	case isWeb(link):
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
		if err != nil {
			return 0, err
		}
		resp, err := s.http.Do(s.authorize(req, src))
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("the torrent link answered HTTP %d", resp.StatusCode)
		}
		if torrent, err = io.ReadAll(io.LimitReader(resp.Body, maxTorrentBytes+1)); err != nil {
			return 0, err
		}
		if len(torrent) > maxTorrentBytes {
			return 0, errors.New("the .torrent file is too large")
		}
	default:
		return 0, errors.New("this item has no torrent or magnet link; open its page instead")
	}
	id, err := s.dl.Add(ctx, link, torrent, auto)
	if err != nil {
		return 0, err
	}
	if it.ID != 0 {
		s.db.ExecContext(ctx, `UPDATE rss_items SET download_id = ? WHERE id = ?`, id, it.ID)
	}
	return id, nil
}
