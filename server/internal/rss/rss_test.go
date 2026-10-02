package rss

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
}

func (f *fakeDL) Add(ctx context.Context, uri string, torrent []byte, auto bool) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	if !byTitle["New Album FLAC"].Downloaded || byTitle["New Album MP3 FLAC"].Match != "excluded" || byTitle["Old Album FLAC"].Downloaded {
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
	if err != nil || len(res) != 2 || !res[0].Downloaded && !res[1].Downloaded {
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
