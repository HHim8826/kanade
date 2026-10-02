package identify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/library"
)

const releaseJSON = `{"id":"11111111-2222-3333-4444-555555555555","title":"ARIA The NATURAL Original Soundtrack",
 "date":"2006-06-21","country":"JP","artist-credit":[{"name":"Choro Club","joinphrase":" feat. "},{"name":"Senoo","joinphrase":""}],
 "label-info":[{"catalog-number":"VICL-61905","label":{"name":"Victor"}}],"release-group":{"primary-type":"Album"},
 "cover-art-archive":{"front":true},
 "media":[{"position":1,"format":"CD","track-count":2,"tracks":[
   {"position":1,"title":"Euforia","length":316000,"artist-credit":[{"name":"葉月絵理乃","joinphrase":""}],"recording":{"id":"aaaaaaaa-0000-0000-0000-000000000001"}},
   {"position":2,"title":"雨降花","length":200000,"artist-credit":[{"name":"Choro Club","joinphrase":""}],"recording":{"id":"aaaaaaaa-0000-0000-0000-000000000002"}}]}]}`

func fakeMB(t *testing.T) (*MusicBrainz, *[]string) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.UserAgent(), "Kanade/") || !strings.Contains(r.UserAgent(), "https://music.example") {
			t.Errorf("user agent %q", r.UserAgent())
		}
		seen = append(seen, r.URL.String())
		switch {
		case r.URL.Path == "/ws/2/artist":
			w.Write([]byte(`{"artists":[{"id":"99999999-0000-0000-0000-000000000000","score":100}]}`))
		case r.URL.Path == "/ws/2/release":
			w.Write([]byte(`{"releases":[` + strings.Replace(releaseJSON, `"title"`, `"score":100,"track-count":2,"title"`, 1) + `]}`))
		case strings.HasPrefix(r.URL.Path, "/ws/2/release/"):
			w.Write([]byte(releaseJSON))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	m := New("https://music.example")
	m.Base, m.CoverArt, m.interval = srv.URL+"/ws/2", srv.URL, 50*time.Millisecond
	return m, &seen
}

func TestSearchQueryAndCandidate(t *testing.T) {
	m, seen := fakeMB(t)
	start := time.Now()
	c, err := m.Search(context.Background(), Query{Title: `ARIA "The" NATURAL`, Artist: "Various Artists", Catalog: "VICL-61905"})
	if err != nil || len(c) != 1 {
		t.Fatalf("search: %+v %v", c, err)
	}
	if c[0].Catalog != "VICL-61905" || c[0].Artist != "Choro Club feat. Senoo" || c[0].Format != "CD" || c[0].Label != "Victor" {
		t.Fatalf("candidate %+v", c[0])
	}
	q := (*seen)[0]
	if !strings.Contains(q, "catno%3A%22VICL%5C-61905%22") || strings.Contains(q, "artist%3A") || !strings.Contains(q, "release%3A%28ARIA+%5C%22The%5C%22+NATURAL%29") {
		t.Fatalf("query %s", q) // quotes escaped; "Various Artists" is not searched for
	}
	m.Search(context.Background(), Query{Title: "other"}) // a second request waits for the rate limit
	m.Search(context.Background(), Query{Title: "other"}) // and a repeat comes from the cache
	if len(*seen) != 2 || time.Since(start) < 50*time.Millisecond {
		t.Fatalf("requests %d in %v", len(*seen), time.Since(start))
	}

	// With an artist and a year, the artist is looked up and its releases of that year added;
	// the same release found both ways is listed once.
	c, err = m.Search(context.Background(), Query{Title: "Euforia", Artist: "Makino Yui", Year: "2006.04", Tracks: 2})
	if err != nil || len(c) != 1 {
		t.Fatalf("search by artist: %+v %v", c, err)
	}
	last := (*seen)[len(*seen)-1]
	if len(*seen) != 5 || !strings.Contains(last, "arid%3A99999999-0000-0000-0000-000000000000+AND+date%3A2006") {
		t.Fatalf("requests %v", *seen)
	}
}

func entry(id, track int64, disc, no int, title string, ms int64) library.Entry {
	return library.Entry{EntryID: id, TrackID: track, DiscNo: disc, TrackNo: no, Title: title, Asset: library.AssetBrief{DurationMS: ms}}
}

func TestProposal(t *testing.T) {
	m, _ := fakeMB(t)
	album := &library.AlbumDetail{AlbumSummary: library.AlbumSummary{ID: 7, Title: "ARIA The NATURAL OST", AlbumArtist: "Choro Club"},
		Entries: []library.Entry{entry(1, 11, 1, 1, "Euforia", 316300), entry(2, 12, 1, 2, "Amefuri Hana", 250000)}}
	p, err := m.Propose(context.Background(), album, "11111111-2222-3333-4444-555555555555")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ProposedChange{}
	for _, c := range p.Changes {
		got[c.Key] = c
	}
	if c := got["album:7:title"]; c.New != "ARIA The NATURAL Original Soundtrack" || !c.Default {
		t.Fatalf("album title %+v", c)
	}
	if _, ok := got["track:11:title"]; ok {
		t.Fatal("unchanged title proposed")
	}
	if c := got["track:11:artist"]; c.New != "葉月絵理乃" || !c.Default {
		t.Fatalf("artist %+v", c)
	}
	if c := got["track:12:title"]; c.New != "雨降花" || c.Default || !strings.Contains(c.Warn, "長度不符") {
		t.Fatalf("track 2 should be flagged: %+v", c)
	}
	if p.Matched != 2 || !p.Cover {
		t.Fatalf("proposal %+v", p)
	}
	sel := p.Selected([]string{"album:7:title", "track:11:artist", "nope"})
	if len(sel) != 2 || *sel[0].Value != "ARIA The NATURAL Original Soundtrack" {
		t.Fatalf("selected %+v", sel)
	}

	// Without track numbers but with the same count, entries pair in order.
	album.Entries = []library.Entry{entry(1, 11, 1, 0, "a", 0), entry(2, 12, 1, 0, "b", 0)}
	p, _ = m.Propose(context.Background(), album, "11111111-2222-3333-4444-555555555555")
	if p.Matched != 2 {
		t.Fatalf("order pairing %+v", p)
	}
	for _, c := range p.Changes {
		if c.Key == "entry:2:track_no" && c.New != "2" {
			t.Fatalf("track number %+v", c)
		}
	}
	if _, err := m.Propose(context.Background(), album, "../../etc"); err == nil {
		t.Fatal("bad release ID accepted")
	}
}
