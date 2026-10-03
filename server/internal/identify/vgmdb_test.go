package identify

import (
	"errors"
	"strings"
	"testing"

	"github.com/HHim8826/kanade/server/internal/library"
)

func dueAlbum() *VGMdbAlbum {
	return &VGMdbAlbum{ID: 12345, Title: " ARIA The STATION Due COUR.2 ", AlbumArtist: "葉月絵理乃, 大原さやか", Date: "2007-03-21",
		Catalog: "FCCC-0002", Cover: "https://medium-media.vgm.io/albums/45/12345/12345-1.jpg",
		Discs: []VGMdbDisc{{Tracks: []VGMdbTrack{{Title: "第14回", LengthMS: 1800000}, {Title: "第15回", LengthMS: 1500000}}}}}
}

func TestProposeVGMdb(t *testing.T) {
	v := dueAlbum()
	if err := v.Check(); err != nil || v.Title != "ARIA The STATION Due COUR.2" {
		t.Fatalf("check: %v %q", err, v.Title)
	}
	// Files numbered on from the first set (DUE14, DUE15) pair in order and are renumbered; the
	// artist goes to the track without one only.
	a := &library.AlbumDetail{ID: 7, Title: "ARIA The STATION Due COUR.2", Entries: []library.Entry{
		entry(1, 11, 1, 14, "DUE14", 1801000), entry(2, 12, 1, 15, "DUE15", 1300000)}}
	a.Entries[1].Artist = "Erino Hazuki"
	p := ProposeVGMdb(a, v)
	if p.Matched != 2 || p.Unmatched != 0 || !p.Cover || p.Release.ID != "12345" || p.Release.Catalog != "FCCC-0002" {
		t.Fatalf("proposal %+v", p)
	}
	got := map[string]ProposedChange{}
	for _, c := range p.Changes {
		got[c.Key] = c
	}
	for key, want := range map[string]string{"album:7:album_artist": "葉月絵理乃, 大原さやか", "album:7:date": "2007-03-21",
		"album:7:catalog": "FCCC-0002", "track:11:title": "第14回", "track:11:artist": "葉月絵理乃, 大原さやか", "entry:1:track_no": "1",
		"entry:2:track_no": "2", "track:12:title": "第15回"} {
		if got[key].New != want {
			t.Errorf("%s = %q, want %q", key, got[key].New, want)
		}
	}
	if _, ok := got["track:12:artist"]; ok {
		t.Error("replaced a track's own artist")
	}
	if _, ok := got["album:7:title"]; ok {
		t.Error("proposed the same title")
	}
	if c := got["track:12:title"]; c.Warn == "" || !strings.Contains(c.Warn, "VGMdb") || c.Default {
		t.Errorf("length mismatch not flagged: %+v", c)
	}
	for _, f := range []string{"mb_release", "mb_recording"} {
		for _, c := range p.Changes {
			if c.Field == f {
				t.Errorf("proposed %s", f)
			}
		}
	}
	if c := got["album:7:date"]; !c.Default || c.Warn != "" { // one of two tracks off: the album still fits
		t.Errorf("album change flagged: %+v", c)
	}
	a.Entries[0].Asset.DurationMS = 100000 // both off now: probably another album
	for _, c := range ProposeVGMdb(a, v).Changes {
		if c.Target == "album" && (c.Default || c.Warn == "") {
			t.Errorf("album change checked though most lengths are off: %+v", c)
		}
	}
	if u := v.coverURL(); u != "https://media.vgm.io/albums/45/12345/12345-1.jpg" {
		t.Errorf("cover url %q", u)
	}
}

func TestVGMdbCheck(t *testing.T) {
	for name, edit := range map[string]func(v *VGMdbAlbum){
		"no title":    func(v *VGMdbAlbum) { v.Title = " " },
		"other host":  func(v *VGMdbAlbum) { v.Cover = "https://example.com/albums/1.jpg" },
		"plain http":  func(v *VGMdbAlbum) { v.Cover = "http://media.vgm.io/albums/1.jpg" },
		"not albums":  func(v *VGMdbAlbum) { v.Cover = "https://media.vgm.io/artists/1.jpg" },
		"with a port": func(v *VGMdbAlbum) { v.Cover = "https://media.vgm.io:8443/albums/1.jpg" },
		"bad date":    func(v *VGMdbAlbum) { v.Date = "Mar 21, 2007" },
		"no discs":    func(v *VGMdbAlbum) { v.Discs = nil },
		"no tracks":   func(v *VGMdbAlbum) { v.Discs = []VGMdbDisc{{}} },
		"long title":  func(v *VGMdbAlbum) { v.Title = strings.Repeat("あ", 501) },
		"length":      func(v *VGMdbAlbum) { v.Discs[0].Tracks[0].LengthMS = -1 },
	} {
		v := dueAlbum()
		edit(v)
		if err := v.Check(); !errors.Is(err, ErrBadVGMdb) {
			t.Errorf("%s: %v", name, err)
		}
	}
	v := dueAlbum()
	v.Cover = ""
	if err := v.Check(); err != nil {
		t.Errorf("no cover: %v", err)
	}
}
