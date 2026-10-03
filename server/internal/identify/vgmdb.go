package identify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/HHim8826/kanade/server/internal/library"
)

// VGMdb (vgmdb.net) has no API, and it answers requests that do not come from a browser with a
// Cloudflare challenge. So the server never reads its pages: the user opens an album page in their
// own browser and pastes it into the client, which reads the album out of it and sends it here to be
// compared. Only the cover image is fetched here, from VGMdb's image host, when the user applies it.

// VGMdbAlbum is what the client read from an album page.
type VGMdbAlbum struct {
	ID          int64       `json:"id"` // from the page's links; 0 when it was not found
	Title       string      `json:"title"`
	AlbumArtist string      `json:"album_artist"` // the credit the user picked, e.g. the performers
	Date        string      `json:"date"`         // YYYY, YYYY-MM or YYYY-MM-DD
	Catalog     string      `json:"catalog"`
	Cover       string      `json:"cover"` // an image on media.vgm.io (or its medium and thumb hosts)
	Discs       []VGMdbDisc `json:"discs"`
}

type VGMdbDisc struct {
	Tracks []VGMdbTrack `json:"tracks"`
}

type VGMdbTrack struct {
	Title    string `json:"title"`
	LengthMS int64  `json:"length_ms"` // 0 when the page gives none
}

var (
	ErrBadVGMdb = errors.New("not an album read from VGMdb")
	vgmdbDate   = regexp.MustCompile(`^\d{4}(-\d{2}(-\d{2})?)?$`)
	vgmdbHosts  = map[string]bool{"media.vgm.io": true, "medium-media.vgm.io": true, "thumb-media.vgm.io": true}
)

// Check validates an album from the client and trims its text.
func (v *VGMdbAlbum) Check() error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrBadVGMdb, fmt.Sprintf(format, args...))
	}
	clean := func(s *string, max int, what string) error {
		*s = strings.TrimSpace(*s)
		if !utf8.ValidString(*s) || utf8.RuneCountInString(*s) > max {
			return bad("%s is too long", what)
		}
		return nil
	}
	for _, f := range []struct {
		s    *string
		max  int
		what string
	}{{&v.Title, 500, "title"}, {&v.AlbumArtist, 500, "album artist"}, {&v.Date, 10, "date"}, {&v.Catalog, 100, "catalog"}, {&v.Cover, 500, "cover"}} {
		if err := clean(f.s, f.max, f.what); err != nil {
			return err
		}
	}
	if v.Title == "" {
		return bad("no title")
	}
	if v.Date != "" && !vgmdbDate.MatchString(v.Date) {
		return bad("date %q", v.Date)
	}
	if v.Cover != "" && v.coverURL() == "" {
		return bad("the cover is not on VGMdb's image host")
	}
	if v.ID < 0 || len(v.Discs) == 0 || len(v.Discs) > 99 {
		return bad("expected 1 to 99 discs")
	}
	n := 0
	for i := range v.Discs {
		d := &v.Discs[i]
		if len(d.Tracks) > 999 {
			return bad("too many tracks")
		}
		n += len(d.Tracks)
		for j := range d.Tracks {
			t := &d.Tracks[j]
			if err := clean(&t.Title, 500, "track title"); err != nil {
				return err
			}
			if t.LengthMS < 0 || t.LengthMS > 24*3600*1000 {
				return bad("track length")
			}
		}
	}
	if n == 0 || n > 2000 {
		return bad("expected 1 to 2000 tracks")
	}
	return nil
}

// coverURL is the full-size image of the cover, or "" when it is not on VGMdb's image host.
func (v *VGMdbAlbum) coverURL() string {
	u, err := url.Parse(v.Cover)
	if err != nil || u.Scheme != "https" || !vgmdbHosts[u.Host] || u.User != nil || u.Port() != "" ||
		!strings.HasPrefix(u.Path, "/albums/") || strings.Contains(u.Path, "..") {
		return ""
	}
	return "https://media.vgm.io" + u.EscapedPath()
}

// ProposeVGMdb compares an album read from VGMdb with an album of the library. VGMdb lists no
// artist per track, so the album artist is offered for tracks that have no artist.
func ProposeVGMdb(album *library.AlbumDetail, v *VGMdbAlbum) *Proposal {
	r := &mbRelease{Title: v.Title, Date: v.Date}
	if v.AlbumArtist != "" {
		r.ArtistCredit = []credit{{Name: v.AlbumArtist}}
	}
	if v.Catalog != "" {
		r.LabelInfo = append(r.LabelInfo, struct {
			CatalogNumber string `json:"catalog-number"`
			Label         *struct {
				Name string `json:"name"`
			} `json:"label"`
		}{CatalogNumber: v.Catalog})
	}
	for i, d := range v.Discs {
		var tracks []mbTrack
		for j, t := range d.Tracks {
			tracks = append(tracks, mbTrack{Position: j + 1, Title: t.Title, Length: t.LengthMS, ArtistCredit: r.ArtistCredit})
		}
		r.Media = append(r.Media, struct {
			Position   int       `json:"position"`
			Format     string    `json:"format"`
			TrackCount int       `json:"track-count"`
			Tracks     []mbTrack `json:"tracks"`
		}{Position: i + 1, TrackCount: len(tracks), Tracks: tracks})
	}
	p := propose(album, r, "VGMdb")
	kept := p.Changes[:0]
	for _, c := range p.Changes {
		if c.Target == "track" && c.Field == "artist" && strings.TrimSpace(c.Old) != "" {
			continue // the track's own artist stays: VGMdb does not say who sings each track
		}
		kept = append(kept, c)
	}
	p.Changes = kept
	if v.ID > 0 {
		p.Release.ID = strconv.FormatInt(v.ID, 10)
	}
	p.Release.Format = fmt.Sprintf("%d 張", len(v.Discs))
	p.Cover = v.coverURL() != ""
	return p
}

// VGMdbCover downloads the album's cover from VGMdb's image host.
func (m *MusicBrainz) VGMdbCover(ctx context.Context, v *VGMdbAlbum) ([]byte, error) {
	u := v.coverURL()
	if u == "" {
		return nil, fmt.Errorf("%w: no cover", ErrBadVGMdb)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", m.ua)
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("VGMdb's image host did not answer (%v)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("VGMdb cover: HTTP %d", resp.StatusCode)
	}
	const limit = 16 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if len(body) > limit {
		return nil, errors.New("the VGMdb cover is larger than 16 MB")
	}
	return body, nil
}
