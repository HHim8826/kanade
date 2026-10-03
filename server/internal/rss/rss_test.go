package rss

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/HHim8826/kanade/server/internal/db"
)

func TestParseNyaa(t *testing.T) {
	f, _ := os.Open("testdata/nyaa.xml") // a real Nyaa feed, trimmed to two items
	defer f.Close()
	list, err := Parse(f)
	if err != nil || len(list) != 2 {
		t.Fatalf("parse: %d %v", len(list), err)
	}
	e := list[1]
	if !strings.HasPrefix(e.Title, "[2025.06.25] ARIA 20周年") || e.Download != "https://nyaa.si/download/1986115.torrent" ||
		e.Page != "https://nyaa.si/view/1986115" || e.GUID != "https://nyaa.si/view/1986115" ||
		e.InfoHash != "a03b25bb691951c4f8c2f00a58e9d0e883415185" || e.Seeders != 14 ||
		e.Size != 3328599654 || e.Published == 0 {
		t.Fatalf("entry %+v", e)
	}
}

func TestParseOtherFeeds(t *testing.T) {
	atom := `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"><entry><title>Album  [FLAC]</title><id>urn:x:1</id>
		<link rel="alternate" href="https://site.example/post/1"/><link rel="enclosure" type="application/x-bittorrent" href="https://site.example/1.torrent" length="1234"/>
		<updated>2026-01-02T03:04:05Z</updated></entry></feed>`
	list, err := Parse(strings.NewReader(atom))
	if err != nil || len(list) != 1 || list[0].Download != "https://site.example/1.torrent" || list[0].Page != "https://site.example/post/1" ||
		list[0].Title != "Album [FLAC]" || list[0].Size != 1234 || list[0].Published == 0 {
		t.Fatalf("atom %+v %v", list, err)
	}

	// A magnet with a base32 hash; an item with only a page; an ezRSS hash without a link; Shift-JIS.
	rss := "<?xml version=\"1.0\" encoding=\"Shift_JIS\"?><rss version=\"2.0\" xmlns:torrent=\"http://xmlns.ezrss.it/0.1/\"><channel>" +
		"<item><title>\x83A\x83\x8B\x83o\x83\x80</title><link>magnet:?xt=urn:btih:UCWTGOWSDG6EQAZJWN3RCSLOD5FLNHOU&amp;dn=x</link></item>" +
		"<item><title>page only</title><link>https://blog.example/post</link></item>" +
		"<item><title>hash only</title><guid>g3</guid><torrent:infoHash>A03B25BB691951C4F8C2F00A58E9D0E883415185</torrent:infoHash>" +
		"<torrent:contentLength>99</torrent:contentLength></item></channel></rss>"
	list, err = Parse(strings.NewReader(rss))
	if err != nil || len(list) != 3 {
		t.Fatalf("rss %+v %v", list, err)
	}
	if list[0].Title != "アルバム" || list[0].InfoHash != "a0ad333ad219bc480329b37711496e1f4ab69dd4" || list[0].GUID == "" {
		t.Fatalf("magnet item %+v", list[0])
	}
	if list[1].Download != "" || list[1].Page != "https://blog.example/post" {
		t.Fatalf("page-only item %+v", list[1])
	}
	if !strings.HasPrefix(list[2].Download, "magnet:?xt=urn:btih:a03b25bb") || list[2].Size != 99 {
		t.Fatalf("hash-only item %+v", list[2])
	}
	if _, err := Parse(strings.NewReader("<html><body>login</body></html>")); err == nil {
		t.Fatal("an HTML page was accepted")
	}
	if n := parseSize("700 MB"); n != 700_000_000 {
		t.Fatalf("size %d", n)
	}
}

func TestRules(t *testing.T) {
	title := "[2025.06.25] ARIA The BEST 2005-2025 [FLAC 96kHz/24bit]"
	cases := []struct {
		include, exclude, want string
	}{
		{"aria flac", "", "included"},
		{"aria mp3\nbest 96khz", "", "included"}, // any line
		{"aria mp3", "", ""},
		{"ａｒｉａ　ＦＬＡＣ", "", "included"},      // full width, case
		{"aria", "mp3\n24bit", "excluded"}, // exclusion wins
		{"", "", ""},
	}
	for _, c := range cases {
		if got := Match(c.include, c.exclude, title); got != c.want {
			t.Errorf("Match(%q, %q) = %q, want %q", c.include, c.exclude, got, c.want)
		}
	}
	if Match("ありあ", "", "アリア ベスト") != "included" {
		t.Error("hiragana rule does not match katakana")
	}
}

// fakeDL records downloads as the real downloader would: a row in downloads.
type fakeDL struct {
	mu    sync.Mutex
	db    *sql.DB
	added []string
	auto  []bool
	fail  int // the next this many Adds fail, like a full disk
	// retried are the downloads Retry started again.
	retried []int64
	// gate, when set, holds each Add until it is closed; entered tells that an Add arrived.
	gate, entered chan struct{}
}

func (f *fakeDL) Retry(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.db.ExecContext(ctx, `UPDATE downloads SET state = 'queued' WHERE id = ? AND state = 'failed'`, id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return errors.New("not possible in the current state")
	}
	f.retried = append(f.retried, id)
	return nil
}

func (f *fakeDL) Add(ctx context.Context, uri string, torrent []byte, auto bool) (int64, error) {
	if f.gate != nil {
		f.entered <- struct{}{}
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail > 0 {
		f.fail--
		return 0, errors.New("the disk is nearly full")
	}
	f.added = append(f.added, uri)
	f.auto = append(f.auto, auto)
	r, err := f.db.ExecContext(ctx, `INSERT INTO downloads (source, state, dir, auto_select, created_at, updated_at)
		VALUES (?, 'metadata', '', ?, 0, 0)`, uri, auto)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

type feedServer struct {
	mu    sync.Mutex
	items []string // titles, newest first
	hits  int
	cond  int // conditional requests answered 304
	fail  bool
	auth  string
}

func (f *feedServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	f.auth = r.Header.Get("Cookie")
	if strings.HasSuffix(r.URL.Path, ".torrent") {
		w.Write([]byte("d4:infod4:name1:xee"))
		return
	}
	if f.fail {
		http.Error(w, "down", http.StatusBadGateway)
		return
	}
	etag := `"` + strings.Join(f.items, "|") + `"`
	if r.Header.Get("If-None-Match") == etag {
		f.cond++
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	var b strings.Builder
	b.WriteString(`<rss version="2.0"><channel>`)
	q := r.URL.Query().Get("q")
	for i, title := range f.items {
		if q != "" && !strings.Contains(title, q) {
			continue
		}
		b.WriteString("<item><title>" + title + "</title><guid>g-" + title + "</guid><link>http://" + r.Host + "/" +
			strings.ReplaceAll(title, " ", "_") + ".torrent</link><description>" + string(rune('0'+i)) + "</description></item>")
	}
	b.WriteString(`</channel></rss>`)
	w.Write([]byte(b.String()))
}

func newService(t *testing.T) (*Service, *fakeDL, *feedServer, string) {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	dl := &fakeDL{db: d}
	s := New(d, dl, "https://music.example/", slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.http = http.DefaultClient // the test feed is on this machine
	fs := &feedServer{items: []string{"Old Album FLAC", "Older Album MP3"}}
	srv := httptest.NewServer(fs)
	t.Cleanup(srv.Close)
	return s, dl, fs, srv.URL + "/rss?page=rss&q="
}

func str(s string) *string { return &s }
func yes(b bool) *bool     { return &b }

func TestPollBaselineAndAutoDownload(t *testing.T) {
	ctx := context.Background()
	s, dl, fs, feed := newService(t)
	if _, err := s.Create(ctx, SourceInput{Name: str("Test"), URL: str(feed), AutoDownload: yes(true)}); err == nil {
		t.Fatal("auto-download without include rules was accepted")
	}
	if _, err := s.Create(ctx, SourceInput{Name: str("Bad"), URL: str("ftp://x/feed")}); err == nil {
		t.Fatal("a non-http address was accepted")
	}
	src, err := s.Create(ctx, SourceInput{Name: str("Test"), URL: str(feed), Include: str("album flac"), Exclude: str("mp3"),
		AutoDownload: yes(true), Cookie: str("pass=secret")})
	if err != nil || !src.Searchable || !src.HasCookie || src.IntervalMin != DefaultInterval {
		t.Fatalf("create %+v %v", src, err)
	}

	// The first fetch is the baseline: recorded, nothing downloaded.
	r, err := s.Poll(ctx, src.ID)
	if err != nil || r.New != 2 || r.Downloaded != 0 || len(dl.added) != 0 || fs.auth != "pass=secret" {
		t.Fatalf("baseline %+v %v %v", r, err, dl.added)
	}
	// Unchanged feed: a conditional request answered 304.
	if r, _ := s.Poll(ctx, src.ID); r.New != 0 || fs.cond != 1 {
		t.Fatalf("conditional %+v cond=%d", r, fs.cond)
	}
	// New items: the included one is downloaded, automatically; the excluded one is not.
	fs.items = append([]string{"New Album FLAC", "New Album MP3 FLAC", "Unrelated"}, fs.items...)
	r, _ = s.Poll(ctx, src.ID)
	if r.New != 3 || r.Downloaded != 1 || len(dl.added) != 1 || !strings.HasSuffix(dl.added[0], "New_Album_FLAC.torrent") || !dl.auto[0] {
		t.Fatalf("new items %+v %v", r, dl.added)
	}
	items, _ := s.Items(ctx, ItemQuery{Source: src.ID})
	if len(items) != 5 || items[0].Title != "Unrelated" {
		t.Fatalf("items %+v", items)
	}
	byTitle := map[string]Item{}
	for _, it := range items {
		byTitle[it.Title] = it
	}
	if byTitle["New Album FLAC"].DownloadState != "metadata" || byTitle["New Album FLAC"].Downloaded ||
		byTitle["New Album MP3 FLAC"].Match != "excluded" || byTitle["Old Album FLAC"].DownloadID != 0 {
		t.Fatalf("marks %+v", byTitle)
	}
	if only, _ := s.Items(ctx, ItemQuery{Only: "included"}); len(only) != 2 {
		t.Fatalf("included %+v", only)
	}
	if found, _ := s.Items(ctx, ItemQuery{Q: "new flac"}); len(found) != 2 {
		t.Fatalf("filter %+v", found)
	}

	// Manual download of an old item; the same torrent is then marked downloaded.
	id, err := s.Download(ctx, byTitle["Old Album FLAC"].ID)
	if err != nil || id != 2 || dl.auto[1] {
		t.Fatalf("manual %d %v", id, err)
	}
	// Turning auto-download off and on again makes the next fetch a baseline.
	s.Update(ctx, src.ID, SourceInput{AutoDownload: yes(false)})
	s.Update(ctx, src.ID, SourceInput{AutoDownload: yes(true)})
	fs.items = append([]string{"Another Album FLAC"}, fs.items...)
	if r, _ := s.Poll(ctx, src.ID); r.New != 1 || r.Downloaded != 0 {
		t.Fatalf("after re-enabling %+v", r)
	}

	// Live search on the site.
	res, err := s.Search(ctx, src.ID, "New")
	if err != nil || len(res) != 2 || res[0].DownloadID == 0 && res[1].DownloadID == 0 {
		t.Fatalf("search %+v %v", res, err)
	}
}

func TestPollFailureBacksOff(t *testing.T) {
	ctx := context.Background()
	s, _, fs, feed := newService(t)
	src, _ := s.Create(ctx, SourceInput{Name: str("Test"), URL: str(feed), IntervalMin: ptr(10)})
	fs.fail = true
	if _, err := s.Poll(ctx, src.ID); err == nil {
		t.Fatal("a failing feed polled fine")
	}
	s.Poll(ctx, src.ID)
	got, _ := s.Source(ctx, src.ID)
	wait := got.NextPollAt - got.LastPollAt
	if got.Failures != 2 || got.LastError == "" || wait != 20*60*1000 {
		t.Fatalf("after failures %+v wait %d", got, wait)
	}
	fs.fail = false
	s.Poll(ctx, src.ID)
	if got, _ = s.Source(ctx, src.ID); got.Failures != 0 || got.LastError != "" {
		t.Fatalf("after recovery %+v", got)
	}
}

func ptr(n int) *int { return &n }

// A source's login and cookie reach only its own site and the sites listed for it, also through
// redirects (review #17).
func TestCredentialsStayWithTheirSite(t *testing.T) {
	ctx := context.Background()
	s, _, _, feed := newService(t)
	s.http = &http.Client{CheckRedirect: checkRedirect}
	type seen struct{ auth, cookie bool }
	var mu sync.Mutex
	got := map[string]seen{}
	torrent := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_, _, ok := r.BasicAuth()
			mu.Lock()
			got[name] = seen{ok, r.Header.Get("Cookie") != ""}
			mu.Unlock()
			w.Write([]byte("d4:infod4:name1:xee"))
		}
	}
	other := httptest.NewServer(torrent("other"))
	defer other.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/own.torrent", torrent("own"))
	mux.HandleFunc("/away", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x.torrent", http.StatusFound)
	})
	own := httptest.NewServer(mux)
	defer own.Close()
	_ = feed
	src, err := s.Create(ctx, SourceInput{Name: str("Private"), URL: str(own.URL + "/rss"), AuthUser: str("me"), AuthPass: str("secret"),
		Cookie: str("session=abc")})
	if err != nil {
		t.Fatal(err)
	}
	check := func(link, name string, want seen) {
		t.Helper()
		mu.Lock()
		delete(got, name)
		mu.Unlock()
		s.db.Exec(`UPDATE downloads SET state = 'canceled'`) // else the torrent is not fetched again (review #50)
		s.DownloadLink(ctx, src.ID, link)
		mu.Lock()
		defer mu.Unlock()
		if got[name] != want {
			t.Fatalf("%s: got %+v, want %+v", link, got[name], want)
		}
	}
	check(own.URL+"/own.torrent", "own", seen{true, true})
	check(other.URL+"/x.torrent", "other", seen{false, false}) // a link to another site
	check(own.URL+"/away", "other", seen{false, false})        // a redirect to another site
	// Listed explicitly, the other site gets them.
	if _, err := s.Update(ctx, src.ID, SourceInput{AuthOrigins: str(other.URL + "/anything")}); err != nil {
		t.Fatal(err)
	}
	check(other.URL+"/x.torrent", "other", seen{true, true})
	check(own.URL+"/away", "other", seen{true, true})
	if _, err := s.Update(ctx, src.ID, SourceInput{AuthOrigins: str("ftp://nope")}); err == nil {
		t.Fatal("a non-web origin was accepted")
	}
	// Same host, another scheme or port, is another site.
	u, _ := url.Parse("https://site.example/rss")
	for raw, want := range map[string]bool{"https://SITE.example:443/a": true, "http://site.example/a": false,
		"https://site.example:8443/a": false, "https://cdn.site.example/a": false} {
		x, _ := url.Parse(raw)
		if (origin(u) == origin(x)) != want {
			t.Errorf("%s: same site = %v", raw, !want)
		}
	}
}

// A fetch that returns after the settings changed is judged by the new settings (review #22).
func TestPollUsesSettingsOfNow(t *testing.T) {
	ctx := context.Background()
	s, dl, fs, feed := newService(t)
	gate := make(chan struct{})
	arrived := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".torrent") {
			arrived <- struct{}{}
			<-gate
		}
		fs.ServeHTTP(w, r)
	}))
	defer srv.Close()
	gated := srv.URL + "/rss"
	src, _ := s.Create(ctx, SourceInput{Name: str("Gated"), URL: str(feed), Include: str("flac"), AutoDownload: yes(true)})
	s.Poll(ctx, src.ID) // baseline
	s.Update(ctx, src.ID, SourceInput{URL: str(gated)})
	go func() { <-arrived; gate <- struct{}{} }()
	s.Poll(ctx, src.ID) // the new address is known now
	fs.items = append([]string{"Fresh Album FLAC"}, fs.items...)

	// Auto-download turned off while the fetch is out: nothing is downloaded.
	done := make(chan struct{})
	go func() { s.Poll(ctx, src.ID); close(done) }()
	<-arrived
	s.Update(ctx, src.ID, SourceInput{AutoDownload: yes(false)})
	gate <- struct{}{}
	<-done
	if len(dl.added) != 0 {
		t.Fatalf("downloaded after auto-download was turned off: %v", dl.added)
	}

	// Turned on again while a fetch is out: that fetch does not use up the new baseline.
	fs.items = append([]string{"Later Album FLAC"}, fs.items...)
	done = make(chan struct{})
	go func() { s.Poll(ctx, src.ID); close(done) }()
	<-arrived
	s.Update(ctx, src.ID, SourceInput{AutoDownload: yes(true)})
	gate <- struct{}{}
	<-done
	if cur, _ := s.source(ctx, src.ID); !cur.Baseline || len(dl.added) != 0 {
		t.Fatalf("baseline %v, downloads %v", cur.Baseline, dl.added)
	}

	// The address changed while the fetch was out: its items are not kept.
	before, _ := s.Items(ctx, ItemQuery{Source: src.ID})
	fs.items = append([]string{"Wrong Site Album FLAC"}, fs.items...)
	done = make(chan struct{})
	go func() { s.Poll(ctx, src.ID); close(done) }()
	<-arrived
	s.Update(ctx, src.ID, SourceInput{URL: str(feed)})
	gate <- struct{}{}
	<-done
	if after, _ := s.Items(ctx, ItemQuery{Source: src.ID}); len(after) != len(before) {
		t.Fatalf("items from the old address kept: %d -> %d", len(before), len(after))
	}
}

// More matches than one poll may start, and a passing failure, wait in the queue instead of being
// dropped (review #23).
func TestAutoDownloadQueue(t *testing.T) {
	ctx := context.Background()
	s, dl, fs, feed := newService(t)
	src, _ := s.Create(ctx, SourceInput{Name: str("Q"), URL: str(feed), Include: str("flac"), AutoDownload: yes(true)})
	s.Poll(ctx, src.ID) // baseline
	for i := 0; i < 6; i++ {
		fs.items = append([]string{fmt.Sprintf("Album %d FLAC", i)}, fs.items...)
	}
	if r, _ := s.Poll(ctx, src.ID); r.New != 6 || r.Downloaded != 5 {
		t.Fatalf("first poll %+v", r)
	}
	if r, _ := s.Poll(ctx, src.ID); r.New != 0 || r.Downloaded != 1 || fs.cond == 0 { // a 304, and the sixth starts
		t.Fatalf("second poll %+v (304s %d)", r, fs.cond)
	}

	// A failure (a full disk) keeps the item queued with the reason, and it starts later.
	fs.items = append([]string{"Disk Album FLAC"}, fs.items...)
	dl.fail = 1
	if r, _ := s.Poll(ctx, src.ID); r.Downloaded != 0 {
		t.Fatalf("started despite the failure %+v", r)
	}
	items, _ := s.Items(ctx, ItemQuery{Source: src.ID, Q: "disk"})
	if len(items) != 1 || items[0].AutoState != "pending" || items[0].AutoError == "" {
		t.Fatalf("queued %+v", items)
	}
	s.db.Exec(`UPDATE rss_items SET auto_next = 0`) // its wait is over
	if r, _ := s.Poll(ctx, src.ID); r.Downloaded != 1 {
		t.Fatalf("retry %+v", r)
	}
	if items, _ := s.Items(ctx, ItemQuery{Source: src.ID, Q: "disk"}); items[0].AutoState != "done" || items[0].DownloadID == 0 {
		t.Fatalf("after retry %+v", items[0])
	}

	// Turning auto-download off drops what is queued.
	fs.items = append([]string{"Off Album FLAC"}, fs.items...)
	dl.fail = 1
	s.Poll(ctx, src.ID)
	s.Update(ctx, src.ID, SourceInput{AutoDownload: yes(false)})
	if items, _ := s.Items(ctx, ItemQuery{Source: src.ID, Q: "off"}); items[0].AutoState != "" {
		t.Fatalf("still queued %+v", items[0])
	}
}

// An item's download counts as done only when it finished; a failed one is retried in place and a
// canceled one replaced, from the stored item and from a live search alike; a download under way
// is not started twice (review #50).
func TestItemsShowTheirDownloadState(t *testing.T) {
	ctx := context.Background()
	s, dl, fs, feed := newService(t)
	fs.items = []string{"Failed FLAC", "Canceled FLAC", "Running FLAC", "Done FLAC"}
	src, _ := s.Create(ctx, SourceInput{Name: str("S"), URL: str(feed)})
	s.Poll(ctx, src.ID)
	items, _ := s.Items(ctx, ItemQuery{Source: src.ID})
	by := map[string]Item{}
	for _, it := range items {
		by[it.Title] = it
	}
	for title, state := range map[string]string{"Failed FLAC": "failed", "Canceled FLAC": "canceled", "Running FLAC": "downloading", "Done FLAC": "completed"} {
		id, err := s.Download(ctx, by[title].ID)
		if err != nil {
			t.Fatal(err)
		}
		s.db.Exec(`UPDATE downloads SET state = ?, info_hash = ? WHERE id = ?`, state, "hash-"+state, id)
	}
	check := func(list []Item) {
		t.Helper()
		for _, it := range list {
			want := map[string]bool{"Done FLAC": true}[it.Title]
			if it.Downloaded != want || it.DownloadState == "" {
				t.Fatalf("%s: downloaded %v, state %q", it.Title, it.Downloaded, it.DownloadState)
			}
		}
	}
	items, _ = s.Items(ctx, ItemQuery{Source: src.ID})
	check(items)
	live, _ := s.Search(ctx, src.ID, "FLAC")
	check(live)
	added := len(dl.added)
	// Running: no second download. Failed: retried in place. Canceled: a new download.
	if id, err := s.Download(ctx, by["Running FLAC"].ID); err != nil || len(dl.added) != added || id != by["Running FLAC"].DownloadID && id == 0 {
		t.Fatalf("running started again: %d %v", id, err)
	}
	failedID := func() int64 { it, _ := s.item(ctx, by["Failed FLAC"].ID); return it.DownloadID }()
	if id, err := s.Download(ctx, by["Failed FLAC"].ID); err != nil || id != failedID || len(dl.retried) != 1 || len(dl.added) != added {
		t.Fatalf("failed: %d %v retried %v", id, err, dl.retried)
	}
	if id, err := s.DownloadLink(ctx, src.ID, by["Canceled FLAC"].Download); err != nil || len(dl.added) != added+1 {
		t.Fatalf("canceled: %d %v", id, err)
	} else if it, _ := s.item(ctx, by["Canceled FLAC"].ID); it.DownloadID != id || it.DownloadState != "metadata" {
		t.Fatalf("the new download is not the item's: %+v", it)
	}
	// An earlier success stands before a later failed attempt of the same torrent.
	s.db.Exec(`INSERT INTO downloads (source, state, dir, info_hash, created_at, updated_at) VALUES ('x', 'failed', '', 'hash-completed', 0, 0)`)
	if it, _ := s.item(ctx, by["Done FLAC"].ID); !it.Downloaded {
		t.Fatalf("a later failure hid the success: %+v", it)
	}
}

// The auto-download queue does not take a failed or canceled download for a success: a failed one
// is retried, a canceled one leaves the queue and is not restarted by itself (review #50).
func TestQueueAndFailedDownloads(t *testing.T) {
	ctx := context.Background()
	s, dl, fs, feed := newService(t)
	src, _ := s.Create(ctx, SourceInput{Name: str("Q"), URL: str(feed), Include: str("flac"), AutoDownload: yes(true)})
	s.Poll(ctx, src.ID) // baseline
	fs.items = append([]string{"Again FLAC", "Stopped FLAC"}, fs.items...)
	for _, title := range []string{"Again FLAC", "Stopped FLAC"} { // downloads of the same torrents, added before
		state := map[string]string{"Again FLAC": "failed", "Stopped FLAC": "canceled"}[title]
		s.db.Exec(`INSERT INTO downloads (source, state, dir, created_at, updated_at) VALUES (?, ?, '', 0, 0)`,
			strings.TrimSuffix(feed, "/rss?page=rss&q=")+"/"+strings.ReplaceAll(title, " ", "_")+".torrent", state)
	}
	if r, _ := s.Poll(ctx, src.ID); r.Downloaded != 1 || len(dl.retried) != 1 || len(dl.added) != 0 {
		t.Fatalf("queue %+v retried %v added %v", r, dl.retried, dl.added)
	}
	items, _ := s.Items(ctx, ItemQuery{Source: src.ID, Q: "stopped"})
	if items[0].AutoState != "" || items[0].DownloadState != "canceled" {
		t.Fatalf("canceled %+v", items[0])
	}
}

// Turning auto-download off while the queue drains stops the starts that follow (review #22).
func TestDisableAutoDuringQueueDrain(t *testing.T) {
	ctx := context.Background()
	s, dl, fs, feed := newService(t)
	src, _ := s.Create(ctx, SourceInput{Name: str("Q"), URL: str(feed), Include: str("flac"), AutoDownload: yes(true)})
	s.Poll(ctx, src.ID) // baseline
	fs.items = append([]string{"One FLAC", "Two FLAC", "Three FLAC"}, fs.items...)
	dl.gate, dl.entered = make(chan struct{}), make(chan struct{}, 4)
	done := make(chan *PollResult)
	go func() { r, _ := s.Poll(ctx, src.ID); done <- r }()
	<-dl.entered // the first Add is under way
	if _, err := s.Update(ctx, src.ID, SourceInput{AutoDownload: yes(false)}); err != nil {
		t.Fatal(err)
	}
	close(dl.gate)
	r := <-done
	if r.Downloaded != 1 || len(dl.added) != 1 {
		t.Fatalf("kept starting after auto-download was turned off: %+v %v", r, dl.added)
	}
}
