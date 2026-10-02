package identify

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/HHim8826/kanade/server/internal/library"
)

// Proposal is what applying a release to an album would change.
type Proposal struct {
	Release   Candidate        `json:"release"`
	Cover     bool             `json:"cover"`     // the Cover Art Archive has a front cover
	Matched   int              `json:"matched"`   // entries paired with a track of the release
	Unmatched int              `json:"unmatched"` // entries left as they are
	Changes   []ProposedChange `json:"changes"`
}

// ProposedChange is one field that would change. Key identifies it when the user picks changes.
type ProposedChange struct {
	Key     string `json:"key"`
	Target  string `json:"target"`
	ID      int64  `json:"id"`
	Field   string `json:"field"`
	Label   string `json:"label"` // which object: empty for the album, else "1-03 title"
	Old     string `json:"old"`
	New     string `json:"new"`
	Warn    string `json:"warn,omitempty"`
	Default bool   `json:"default"` // checked unless something looks off
}

// CoverKey selects the release's front cover among the changes.
const CoverKey = "cover"

// Propose looks a release up and compares it with an album.
func (m *MusicBrainz) Propose(ctx context.Context, album *library.AlbumDetail, releaseID string) (*Proposal, error) {
	r, err := m.release(ctx, releaseID)
	if err != nil {
		return nil, err
	}
	return propose(album, r), nil
}

// lengthSlack is how far a file's duration may be from the release's track length before the
// pairing is flagged: different masters and gaps differ by a second or two, a wrong track by more.
const lengthSlack = 5000

func propose(album *library.AlbumDetail, r *mbRelease) *Proposal {
	p := &Proposal{Release: r.candidate(), Cover: r.CoverArtArchive.Front, Changes: []ProposedChange{}}
	add := func(target string, id int64, field, label, old, new, warn string) {
		if new == "" || strings.TrimSpace(old) == strings.TrimSpace(new) { // never blank out what the album has
			return
		}
		p.Changes = append(p.Changes, ProposedChange{Key: fmt.Sprintf("%s:%d:%s", target, id, field), Target: target, ID: id,
			Field: field, Label: label, Old: old, New: new, Warn: warn, Default: warn == ""})
	}
	add("album", album.ID, "title", "", album.Title, r.Title, "")
	add("album", album.ID, "album_artist", "", album.AlbumArtist, creditString(r.ArtistCredit), "")
	add("album", album.ID, "date", "", album.Date, r.Date, "")
	add("album", album.ID, "catalog", "", album.Catalog, p.Release.Catalog, "")
	add("album", album.ID, "mb_release", "", album.MBRelease, r.ID, "")

	type pos struct{ disc, track int }
	tracks := map[pos]mbTrack{}
	var order []pos
	for _, m := range r.Media {
		for _, t := range m.Tracks {
			k := pos{max(m.Position, 1), t.Position}
			tracks[k] = t
			order = append(order, k)
		}
	}
	entries := append([]library.Entry(nil), album.Entries...)
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].DiscNo != entries[j].DiscNo {
			return entries[i].DiscNo < entries[j].DiscNo
		}
		return entries[i].TrackNo < entries[j].TrackNo
	})
	// Pair by disc and track number when every entry has one; otherwise, when the counts agree,
	// in order (files numbered only by name, or not at all).
	numbered := true
	seen := map[pos]bool{}
	for _, e := range entries {
		k := pos{max(e.DiscNo, 1), e.TrackNo}
		if e.TrackNo <= 0 || seen[k] {
			numbered = false
		}
		seen[k] = true
	}
	byOrder := !numbered && len(entries) == len(order)
	for i, e := range entries {
		var k pos
		switch {
		case numbered:
			k = pos{max(e.DiscNo, 1), e.TrackNo}
		case byOrder:
			k = order[i]
		default:
			p.Unmatched++
			continue
		}
		t, ok := tracks[k]
		if !ok {
			p.Unmatched++
			continue
		}
		p.Matched++
		label := fmt.Sprintf("%d-%02d %s", max(e.DiscNo, 1), e.TrackNo, e.Title)
		warn := ""
		if d := e.Asset.DurationMS - t.Length; t.Length > 0 && e.Asset.DurationMS > 0 && (d > lengthSlack || d < -lengthSlack) {
			warn = fmt.Sprintf("長度不符：檔案 %s，MusicBrainz %s", clock(e.Asset.DurationMS), clock(t.Length))
		}
		artist := creditString(t.ArtistCredit)
		add("track", e.TrackID, "title", label, e.Title, t.Title, warn)
		add("track", e.TrackID, "artist", label, e.Artist, artist, warn)
		if byOrder {
			add("entry", e.EntryID, "disc_no", label, strconv.Itoa(e.DiscNo), strconv.Itoa(k.disc), warn)
			add("entry", e.EntryID, "track_no", label, strconv.Itoa(e.TrackNo), strconv.Itoa(k.track), warn)
		}
		add("track", e.TrackID, "mb_recording", label, "", t.Recording.ID, warn)
	}
	return p
}

func clock(ms int64) string { return fmt.Sprintf("%d:%02d", ms/60000, ms/1000%60) }

// Selected turns the picked keys of a proposal into library changes.
func (p *Proposal) Selected(keys []string) []library.Change {
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	var out []library.Change
	for _, c := range p.Changes {
		if want[c.Key] {
			out = append(out, library.Change{Target: c.Target, ID: c.ID, Field: c.Field, Value: library.Str(c.New)})
		}
	}
	return out
}
