package library

import (
	"context"
	"fmt"
	"testing"
)

func TestKanaToRomaji(t *testing.T) {
	for kana, want := range map[string]string{
		"ほしのあおい": "hoshinoaoi", "やなぎなぎ": "yanaginagi", "ユウキ": "yuki", "シャーロット": "sharotto",
		"きゃりーぱみゅぱみゅ": "kyaripamyupamyu", "まっちゃ": "matcha", "ティファ": "tifa", "しんいち": "shinichi",
	} {
		if got := romaji(kanaToRomaji(kana)); got != want {
			t.Errorf("%s: %q, want %q", kana, got, want)
		}
	}
}

// Names of one artist written two ways are proposed as one; others are not (D13).
func TestAlike(t *testing.T) {
	for _, c := range []struct {
		a, b, want string
	}{
		{"Nagi Yanagi", "やなぎなぎ", "reading"},
		{"Yanagi Nagi", "ヤナギナギ", "reading"},
		{"Yūki Kaji", "Kaji Yuuki", "order"},
		{"ClariS", "claris", "case"},
		{"ＡＢＣ", "abc", "case"},
		{"やなぎ なぎ", "ヤナギナギ", "case"},
		{"Aoi Hoshino", "星野あおい", "given"},
		{"Aoi Hoshino", "Sora Kudo", ""},
		{"星野あおい", "工藤そら", ""},
		{"Aoi", "あい", ""},
		{"Mika", "ミカ", "reading"},
	} {
		if got := alike(keysOf(c.a), keysOf(c.b)); got != c.want {
			t.Errorf("%s / %s: %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

// Merging says one name is another artist: its songs, whole or as one of several names, and songs
// that come later, are that artist's; the tags stay; search finds it by either name; undo takes it
// back. A pair the user says is two is not proposed again.
func TestMergeArtists(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	one, _ := s.Publish(ctx, verifiedAsset(t, s, "m1"), EntryInput{Title: "Morning", Artist: "Aoi Hoshino", Album: "One"})
	two, _ := s.Publish(ctx, verifiedAsset(t, s, "m2"), EntryInput{Title: "Evening", Artist: "星野あおい", Album: "Two"})
	duet, _ := s.Publish(ctx, verifiedAsset(t, s, "m3"), EntryInput{Title: "Night", Artist: "Aoi Hoshino & Sora Kudo", Album: "Two"})
	parts := Split("Aoi Hoshino & Sora Kudo")
	if _, err := s.DecideSplits(ctx, []SplitDecision{{Name: "Aoi Hoshino & Sora Kudo", Parts: &parts}}); err != nil {
		t.Fatal(err)
	}
	pairs, left, err := s.SameArtists(ctx)
	if err != nil || len(pairs) != 1 || left != 0 || pairs[0].Reason != "given" || pairs[0].Sure {
		t.Fatalf("pairs %+v, left %d: %v", pairs, left, err)
	}
	drop, keep := pairs[0].A, pairs[0].B // Aoi Hoshino has two songs
	if drop.Name != "Aoi Hoshino" || drop.Tracks != 2 || keep.Name != "星野あおい" {
		t.Fatalf("pair %+v", pairs[0])
	}
	g, err := s.MergeArtists(ctx, drop.ID, keep.ID)
	if err != nil {
		t.Fatal(err)
	}
	credited := func(id int64) string {
		d, _ := s.Track(ctx, id)
		var names []string
		for _, c := range d.Credits {
			if c.Role == RoleArtist {
				names = append(names, c.Name)
			}
		}
		return fmt.Sprint(names)
	}
	for id, want := range map[int64]string{one.TrackID: "[星野あおい]", two.TrackID: "[星野あおい]", duet.TrackID: "[星野あおい Sora Kudo]"} {
		if got := credited(id); got != want {
			t.Fatalf("track %d: %s, want %s", id, got, want)
		}
	}
	if d, _ := s.Track(ctx, one.TrackID); d.Artist != "Aoi Hoshino" {
		t.Fatalf("the tag changed: %q", d.Artist)
	}
	later, _ := s.Publish(ctx, verifiedAsset(t, s, "m4"), EntryInput{Title: "Dawn", Artist: "Aoi Hoshino", Album: "Three"})
	if got := credited(later.TrackID); got != "[星野あおい]" {
		t.Fatalf("a song imported later: %s", got)
	}
	if res, _ := s.Search(ctx, "Hoshino", 10); len(res.Artists) != 1 || res.Artists[0].ID != keep.ID {
		t.Fatalf("searching the other name: %+v", res.Artists)
	}
	if list, _ := s.ArtistsBy(ctx, 50, 0, "", ""); len(list) != 2 {
		t.Fatalf("artists listed: %+v", list)
	}
	if pairs, _, _ := s.SameArtists(ctx); len(pairs) != 0 {
		t.Fatalf("still proposed: %+v", pairs)
	}
	if _, err := s.MergeArtists(ctx, keep.ID, drop.ID); err == nil {
		t.Fatal("merged back into a name merged into it")
	}

	if _, conflicts, err := s.Undo(ctx, g); err != nil || len(conflicts) != 0 {
		t.Fatalf("undo: %+v %v", conflicts, err)
	}
	for id, want := range map[int64]string{one.TrackID: "[Aoi Hoshino]", duet.TrackID: "[Aoi Hoshino Sora Kudo]", later.TrackID: "[Aoi Hoshino]"} {
		if got := credited(id); got != want {
			t.Fatalf("after undo, track %d: %s, want %s", id, got, want)
		}
	}
	if err := s.NotSame(ctx, "星野あおい", "Aoi Hoshino"); err != nil {
		t.Fatal(err)
	}
	if pairs, left, _ := s.SameArtists(ctx); len(pairs) != 0 || left != 1 {
		t.Fatalf("said to be two: %+v, left %d", pairs, left)
	}
	if err := s.ProposeSameAgain(ctx); err != nil {
		t.Fatal(err)
	}
	if pairs, _, _ := s.SameArtists(ctx); len(pairs) != 1 {
		t.Fatalf("proposed again: %+v", pairs)
	}
}

// The lists' orders and filters (review 2.2).
func TestListOrders(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	s.Publish(ctx, verifiedAsset(t, s, "o1"), EntryInput{Title: "Beta", Artist: "Zed", Album: "Old", AlbumArtist: "Zed", Date: "2001"})
	s.Publish(ctx, verifiedAsset(t, s, "o2"), EntryInput{Title: "Alpha", Artist: "Amy", Album: "New", AlbumArtist: "Amy", Date: "2024"})
	s.Publish(ctx, verifiedAsset(t, s, "o3"), EntryInput{Title: "Gamma", Artist: "Amy", Album: "Undated", AlbumArtist: "Bob"})
	albums := func(sort, search string) string {
		list, err := s.AlbumsBy(ctx, AlbumQuery{Limit: 10, Sort: sort, Search: search})
		if err != nil {
			return err.Error()
		}
		var out []string
		for _, a := range list {
			out = append(out, a.Title)
		}
		return fmt.Sprint(out)
	}
	for _, c := range [][3]string{
		{"", "", "[New Undated Old]"}, {"title", "", "[New Old Undated]"}, {"recent", "", "[Undated New Old]"},
		{"date", "", "[New Old Undated]"}, {"", "ol", "[Old]"},
	} {
		if got := albums(c[0], c[1]); got != c[2] {
			t.Errorf("albums %q %q: %s, want %s", c[0], c[1], got, c[2])
		}
	}
	if _, err := s.AlbumsBy(ctx, AlbumQuery{Limit: 10, Sort: "nope"}); err == nil {
		t.Error("an unknown order")
	}
	tracks := func(sort, search string) string {
		list, _ := s.TracksBy(ctx, TrackQuery{Limit: 10, Sort: sort, Search: search})
		var out []string
		for _, a := range list {
			out = append(out, a.Title)
		}
		return fmt.Sprint(out)
	}
	for _, c := range [][3]string{{"", "", "[Alpha Beta Gamma]"}, {"artist", "", "[Alpha Gamma Beta]"}, {"recent", "", "[Gamma Alpha Beta]"}, {"", "amy", "[Alpha Gamma]"}} {
		if got := tracks(c[0], c[1]); got != c[2] {
			t.Errorf("tracks %q %q: %s, want %s", c[0], c[1], got, c[2])
		}
	}
	list, _ := s.ArtistsBy(ctx, 10, 0, "tracks", "")
	if len(list) != 2 || list[0].Name != "Amy" || list[0].Tracks != 2 {
		t.Errorf("artists by songs: %+v", list)
	}
	if list, _ := s.ArtistsBy(ctx, 10, 0, "", "ze"); len(list) != 1 || list[0].Name != "Zed" {
		t.Errorf("artists found: %+v", list)
	}
}
