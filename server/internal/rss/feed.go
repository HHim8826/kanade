// Package rss polls RSS and Atom sources of torrents (plan §3, P2-5): parsing, de-duplication,
// keyword rules and optional automatic download.
package rss

import (
	"encoding/base32"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/htmlindex"
)

// Limits for one fetch (plan §3: bounded response size and item count).
const (
	maxFeedBytes = 5 << 20
	maxEntries   = 500
)

// Entry is one item of a feed.
type Entry struct {
	GUID      string `json:"guid"`
	Title     string `json:"title"`
	Page      string `json:"page,omitempty"`     // the item's web page
	Download  string `json:"download,omitempty"` // a .torrent URL or magnet link; "" when there is only a page
	InfoHash  string `json:"info_hash,omitempty"`
	Size      int64  `json:"size,omitempty"` // bytes; 0 when unknown
	Seeders   int    `json:"seeders"`        // -1 when unknown
	Published int64  `json:"published_at,omitempty"`
}

type enclosure struct {
	URL    string `xml:"url,attr"`
	Type   string `xml:"type,attr"`
	Length int64  `xml:"length,attr"`
}

type xmlItem struct {
	Title      string      `xml:"title"`
	Link       string      `xml:"link"`
	GUID       string      `xml:"guid"`
	PubDate    string      `xml:"pubDate"`
	DCDate     string      `xml:"http://purl.org/dc/elements/1.1/ date"`
	Enclosures []enclosure `xml:"enclosure"`
	// Nyaa (https://nyaa.si/xmlns/nyaa) and the ezRSS torrent namespace used by many trackers.
	NyaaHash      string `xml:"https://nyaa.si/xmlns/nyaa infoHash"`
	NyaaSize      string `xml:"https://nyaa.si/xmlns/nyaa size"`
	NyaaSeeders   string `xml:"https://nyaa.si/xmlns/nyaa seeders"`
	TorrentHash   string `xml:"http://xmlns.ezrss.it/0.1/ infoHash"`
	TorrentMagnet string `xml:"http://xmlns.ezrss.it/0.1/ magnetURI"`
	TorrentLength int64  `xml:"http://xmlns.ezrss.it/0.1/ contentLength"`
	TorrentSeeds  string `xml:"http://xmlns.ezrss.it/0.1/ seeds"`
}

type xmlEntry struct {
	Title string `xml:"title"`
	ID    string `xml:"id"`
	Links []struct {
		Href   string `xml:"href,attr"`
		Rel    string `xml:"rel,attr"`
		Type   string `xml:"type,attr"`
		Length int64  `xml:"length,attr"`
	} `xml:"link"`
	Updated   string `xml:"updated"`
	Published string `xml:"published"`
}

type xmlFeed struct {
	XMLName xml.Name
	Channel struct {
		Items []xmlItem `xml:"item"`
	} `xml:"channel"`
	Items   []xmlItem  `xml:"item"`  // RSS 1.0 keeps items outside the channel
	Entries []xmlEntry `xml:"entry"` // Atom
}

var ErrTooLarge = fmt.Errorf("the feed is larger than %d MB", maxFeedBytes>>20)

// Parse reads an RSS 2.0, RSS 1.0 or Atom document, keeping at most 500 entries.
func Parse(r io.Reader) ([]Entry, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxFeedBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFeedBytes {
		return nil, ErrTooLarge
	}
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	dec.Strict = false
	dec.CharsetReader = func(label string, in io.Reader) (io.Reader, error) {
		enc, err := htmlindex.Get(label)
		if err != nil {
			return nil, err
		}
		return enc.NewDecoder().Reader(in), nil
	}
	var f xmlFeed
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("not an RSS or Atom feed: %w", err)
	}
	switch f.XMLName.Local {
	case "rss", "feed", "RDF":
	default:
		return nil, errors.New("not an RSS or Atom feed")
	}
	out := []Entry{}
	for _, it := range append(f.Channel.Items, f.Items...) {
		out = append(out, fromItem(it))
	}
	for _, e := range f.Entries {
		out = append(out, fromAtom(e))
	}
	if len(out) > maxEntries {
		out = out[:maxEntries]
	}
	return out, nil
}

func fromItem(it xmlItem) Entry {
	e := Entry{Title: clean(it.Title), Seeders: -1}
	link := strings.TrimSpace(it.Link)
	guid := strings.TrimSpace(it.GUID)
	for _, enc := range it.Enclosures {
		u := strings.TrimSpace(enc.URL)
		if e.Download == "" && (strings.Contains(enc.Type, "bittorrent") || isTorrent(u) || isMagnet(u)) {
			e.Download = u
			if enc.Length > 0 {
				e.Size = enc.Length
			}
		}
	}
	if e.Download == "" && (isTorrent(link) || isMagnet(link)) {
		e.Download = link
	}
	if e.Download == "" && isMagnet(it.TorrentMagnet) {
		e.Download = strings.TrimSpace(it.TorrentMagnet)
	}
	switch {
	case isWeb(guid) && guid != e.Download:
		e.Page = guid
	case isWeb(link) && link != e.Download:
		e.Page = link
	}
	e.InfoHash = normHash(first(it.NyaaHash, it.TorrentHash))
	if e.InfoHash == "" {
		e.InfoHash = magnetHash(e.Download)
	}
	if e.Download == "" && e.InfoHash != "" { // a hash alone is enough for a magnet link
		e.Download = "magnet:?xt=urn:btih:" + e.InfoHash + "&dn=" + url.QueryEscape(e.Title)
	}
	if n := parseSize(it.NyaaSize); n > 0 {
		e.Size = n
	} else if it.TorrentLength > 0 {
		e.Size = it.TorrentLength
	}
	if n, err := strconv.Atoi(strings.TrimSpace(first(it.NyaaSeeders, it.TorrentSeeds))); err == nil {
		e.Seeders = n
	}
	e.Published = parseTime(first(it.PubDate, it.DCDate))
	e.GUID = first(guid, link, e.Download, e.Title+"|"+strings.TrimSpace(it.PubDate))
	return e
}

func fromAtom(a xmlEntry) Entry {
	e := Entry{Title: clean(a.Title), Seeders: -1}
	for _, l := range a.Links {
		h := strings.TrimSpace(l.Href)
		switch {
		case e.Download == "" && (l.Rel == "enclosure" || strings.Contains(l.Type, "bittorrent")) && (isTorrent(h) || isMagnet(h) || strings.Contains(l.Type, "bittorrent")):
			e.Download = h
			if l.Length > 0 {
				e.Size = l.Length
			}
		case e.Page == "" && (l.Rel == "" || l.Rel == "alternate") && isWeb(h):
			if isTorrent(h) || isMagnet(h) {
				if e.Download == "" {
					e.Download = h
				}
			} else {
				e.Page = h
			}
		}
	}
	e.InfoHash = magnetHash(e.Download)
	e.Published = parseTime(first(a.Published, a.Updated))
	e.GUID = first(strings.TrimSpace(a.ID), e.Page, e.Download, e.Title)
	return e
}

func first(v ...string) string {
	for _, s := range v {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

func clean(s string) string { return strings.Join(strings.Fields(s), " ") }

func isWeb(s string) bool { return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") }

func isMagnet(s string) bool { return strings.HasPrefix(strings.TrimSpace(s), "magnet:?") }

func isTorrent(s string) bool {
	u, err := url.Parse(strings.TrimSpace(s))
	return err == nil && isWeb(s) && strings.HasSuffix(strings.ToLower(u.Path), ".torrent")
}

var (
	hexHash    = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	base32Hash = regexp.MustCompile(`^[A-Za-z2-7]{32}$`)
	btihParam  = regexp.MustCompile(`(?i)urn:btih:([0-9a-z]+)`)
)

// normHash turns a hex or base32 info hash into lowercase hex.
func normHash(s string) string {
	s = strings.TrimSpace(s)
	switch {
	case hexHash.MatchString(s):
		return strings.ToLower(s)
	case base32Hash.MatchString(s):
		if b, err := base32.StdEncoding.DecodeString(strings.ToUpper(s)); err == nil {
			return hex.EncodeToString(b)
		}
	}
	return ""
}

func magnetHash(link string) string {
	if !isMagnet(link) {
		return ""
	}
	if m := btihParam.FindStringSubmatch(link); m != nil {
		return normHash(m[1])
	}
	return ""
}

var sizeRe = regexp.MustCompile(`(?i)^\s*([0-9]+(?:\.[0-9]+)?)\s*([KMGT]?)(I?)B\s*$`)

// parseSize reads "155.3 MiB", "3.1 GiB" or "700 MB".
func parseSize(s string) int64 {
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	v, _ := strconv.ParseFloat(m[1], 64)
	base := 1000.0
	if m[3] != "" {
		base = 1024
	}
	exp := strings.Index("KMGT", strings.ToUpper(m[2])) + 1
	for i := 0; i < exp; i++ {
		v *= base
	}
	return int64(v)
}

func parseTime(s string) int64 {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC1123Z, time.RFC1123, time.RFC3339, "Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST",
		"2006-01-02T15:04:05Z0700", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}
