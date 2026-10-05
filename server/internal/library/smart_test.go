package library

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/HHim8826/kanade/server/internal/db"
)

// Smart playlist rules pick songs by category, album, artist, kind, favorite, plays and when they
// were, all or any of them, in an order and within limits; a smart playlist lists what they pick
// now and takes no items (review #96).
func TestSmartRules(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	f := fixture{t, s}
	a := f.song("a", "A", "Aria Album", 1, 1)
	b := f.song("b", "B", "Other Album", 1, 1)
	c := f.song("c", "C", "Drama Box", 1, 1)
	d := f.song("d", "D", "Other Album", 1, 2)
	s.db.Exec(`UPDATE tracks SET kind = 'spoken' WHERE id = ?`, c.TrackID)
	_, aria, _ := s.Categorize(ctx, CategorizeRequest{Albums: []int64{f.albumOf(a.EntryID)}, Create: "ARIA"})
	s.SetFavorite(ctx, "track", a.TrackID, true)
	now := db.Now()
	day := int64(24 * 3600 * 1000)
	n := 0
	play := func(track int64, ago int64, counted, finished bool) {
		n++
		var asset int64
		s.db.QueryRow(`SELECT asset_id FROM track_assets WHERE track_id = ?`, track).Scan(&asset)
		if _, err := s.db.Exec(`INSERT INTO plays (session, asset_id, track_id, started_at, updated_at, listened_ms, counted, finished)
			VALUES (?, ?, ?, ?, ?, 40000, ?, ?)`, fmt.Sprint("p", n), asset, track, now-ago*day, now-ago*day, counted, finished); err != nil {
			t.Fatal(err)
		}
	}
	play(a.TrackID, 1, true, true)
	play(a.TrackID, 2, true, false)
	play(a.TrackID, 3, true, false)
	play(b.TrackID, 100, true, true)
	play(c.TrackID, 5, false, false)
	names := func(r Rules, exclude ...int64) string {
		t.Helper()
		list, _, err := s.SmartTracks(ctx, r, exclude, 0)
		if err != nil {
			t.Fatal(err)
		}
		out := ""
		for _, x := range list {
			out += x.Title
		}
		return out
	}
	sorted := func(r Rules) string {
		r.Sort = "recent_added"
		got := []byte(names(r))
		slices.Sort(got)
		return string(got)
	}
	for _, tc := range []struct {
		name string
		r    Rules
		want string
	}{
		{"in a category", Rules{Conditions: []Condition{{Field: "category", Op: "is", IDs: []int64{aria.ID}}}}, "A"},
		{"in no category of these", Rules{Conditions: []Condition{{Field: "category", Op: "not", IDs: []int64{aria.ID}}}}, "BCD"},
		{"on an album", Rules{Conditions: []Condition{{Field: "album", Op: "is", IDs: []int64{f.albumOf(b.EntryID)}}}}, "BD"},
		{"artist", Rules{Conditions: []Condition{{Field: "artist", Op: "contains", Value: "xc"}}}, "C"},
		{"drama and talk", Rules{Conditions: []Condition{{Field: "kind", Op: "is", Value: "spoken"}}}, "C"},
		{"favorites", Rules{Conditions: []Condition{{Field: "favorite", Op: "is"}}}, "A"},
		{"played at least twice", Rules{Conditions: []Condition{{Field: "plays", Op: "gte", N: 2}}}, "A"},
		{"played at most once", Rules{Conditions: []Condition{{Field: "plays", Op: "lte", N: 1}}}, "BCD"},
		{"heard in 30 days", Rules{Conditions: []Condition{{Field: "played_within", Op: "is", N: 30}}}, "AC"},
		{"not heard in 30 days", Rules{Conditions: []Condition{{Field: "played_within", Op: "not", N: 30}}}, "BD"},
		{"never played", Rules{Conditions: []Condition{{Field: "never_played", Op: "is"}}}, "D"},
		{"never finished", Rules{Conditions: []Condition{{Field: "finished", Op: "not"}}}, "CD"},
		{"favorite and not heard in 30 days", Rules{Conditions: []Condition{{Field: "favorite", Op: "is"}, {Field: "played_within", Op: "not", N: 30}}}, ""},
		{"favorite or never played", Rules{Match: "any", Conditions: []Condition{{Field: "favorite", Op: "is"}, {Field: "never_played", Op: "is"}}}, "AD"},
	} {
		if got := sorted(tc.r); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := names(Rules{Sort: "least_recent"}); got != "DBCA" {
		t.Errorf("longest unheard first: %s", got)
	}
	if got := names(Rules{Sort: "most_played"})[:1]; got != "A" {
		t.Errorf("most played first: %s", got)
	}
	if got := names(Rules{Sort: "most_played", SortDays: 30})[:2]; got != "AB" && got != "AC" && got != "AD" {
		t.Errorf("most played in 30 days: %s", got)
	}
	if got := names(Rules{Sort: "album"}); got != "ACBD" { // Aria Album, Drama Box, Other Album (1, 2); album artists "AA …"
		t.Errorf("album order: %s", got)
	}
	if got := names(Rules{Sort: "least_recent", Limit: 2}); got != "DB" {
		t.Errorf("at most two: %s", got)
	}
	if got := names(Rules{Sort: "least_recent", Minutes: 1}); got != "D" { // 60 s files: one fits a minute
		t.Errorf("at most a minute: %s", got)
	}
	if got := names(Rules{Sort: "least_recent"}, d.TrackID, b.TrackID); got != "CA" {
		t.Errorf("leaving out the songs just played: %s", got)
	}
	for _, bad := range []Rules{
		{Conditions: []Condition{{Field: "mood", Op: "is"}}},
		{Conditions: []Condition{{Field: "favorite", Op: "gte"}}},
		{Conditions: []Condition{{Field: "category", Op: "is"}}},
		{Conditions: []Condition{{Field: "played_within", Op: "is", N: 0}}},
		{Sort: "loudest"},
		{Match: "some"},
	} {
		if err := bad.Check(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}

	// As a playlist: it lists what the rules pick now, and takes no items.
	id, err := s.CreateSmartPlaylist(ctx, "久未重聽", "", Rules{Conditions: []Condition{{Field: "played_within", Op: "not", N: 30}}, Sort: "least_recent"})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.Playlist(ctx, id)
	if !p.Smart || p.Rules == nil || len(p.Items) != 2 || p.Items[0].Title != "D" || p.Matches != 2 || p.Tracks != 2 {
		t.Fatalf("smart playlist %+v", p)
	}
	if _, err := s.AddToPlaylist(ctx, id, []PlaylistAdd{{TrackID: a.TrackID}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("items added to a smart playlist: %v", err)
	}
	play(d.TrackID, 0, true, true) // D heard now: it leaves the list
	if p, _ := s.Playlist(ctx, id); len(p.Items) != 1 || p.Items[0].Title != "B" {
		t.Fatalf("after hearing D: %+v", p.Items)
	}
	if err := s.SetPlaylistRules(ctx, id, Rules{Conditions: []Condition{{Field: "favorite", Op: "is"}}}); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.Playlists(ctx, false); len(list) != 1 || !list[0].Smart || list[0].Tracks != 1 {
		t.Fatalf("listed %+v", list)
	}
	plain, _ := s.CreatePlaylist(ctx, "plain", "")
	if err := s.SetPlaylistRules(ctx, plain, Rules{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rules on an ordinary playlist: %v", err)
	}
	// What songs can be added to (review #162): the ordinary ones only.
	if list, _ := s.Playlists(ctx, true); len(list) != 1 || list[0].ID != plain {
		t.Fatalf("plain only: %+v", list)
	}
	// The list's count is the playlist's, and its cover the same each time, also in random order.
	if err := s.SetPlaylistRules(ctx, id, Rules{Sort: "random"}); err != nil {
		t.Fatal(err)
	}
	full, _ := s.Playlist(ctx, id)
	for range 5 {
		list, _ := s.Playlists(ctx, false)
		for _, l := range list {
			if l.ID == id && (l.Tracks != full.Tracks || l.Tracks == 0) {
				t.Fatalf("listed %d songs, the playlist has %d", l.Tracks, full.Tracks)
			}
		}
	}
}

// Played on and on, a smart playlist whose rules match only songs just played or queued goes round
// again, those played longest ago first and never the song queued or playing last, unless it is the
// only one; rules that match nothing give nothing (review #105).
func TestSmartNextGoesRoundAgain(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	f := fixture{t, s}
	a := f.song("a", "A", "Album", 1, 1)
	b := f.song("b", "B", "Album", 1, 2)
	c := f.song("c", "C", "Album", 1, 3)
	album := Rules{Conditions: []Condition{{Field: "album", Op: "is", IDs: []int64{f.albumOf(a.EntryID)}}}, Sort: "album"}
	titles := func(r Rules, n int, not ...int64) string {
		t.Helper()
		list, err := s.SmartNext(ctx, r, not, n)
		if err != nil {
			t.Fatal(err)
		}
		out := ""
		for _, x := range list {
			out += x.Title
		}
		return out
	}
	for _, tc := range []struct {
		name string
		r    Rules
		n    int
		not  []int64
		want string
	}{
		{"first pick", album, 3, nil, "ABC"},
		{"songs left", album, 3, []int64{a.TrackID}, "BC"},
		{"C playing, all played: round again", album, 3, []int64{c.TrackID, b.TrackID, a.TrackID}, "AB"},
		{"C queued, B playing", album, 2, []int64{c.TrackID, b.TrackID, a.TrackID}, "AB"},
		{"played more than once", album, 3, []int64{a.TrackID, c.TrackID, b.TrackID, a.TrackID}, "BC"},
		{"only the song playing", Rules{Conditions: []Condition{{Field: "artist", Op: "contains", Value: "xa"}}}, 2, []int64{a.TrackID, b.TrackID}, "A"},
		{"rules match nothing", Rules{Conditions: []Condition{{Field: "artist", Op: "contains", Value: "nobody"}}}, 2, []int64{a.TrackID}, ""},
	} {
		if got := titles(tc.r, tc.n, tc.not...); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
