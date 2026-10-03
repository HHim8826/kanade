package library

import (
	"context"
	"errors"
	"testing"
)

func TestMakeAlbums(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	assets := map[string]int64{}
	tracks := map[string]int64{}
	for _, name := range []string{"DUE01", "DUE02", "DUE03"} {
		assets[name] = verifiedAsset(t, s, name)
		r, err := s.Publish(ctx, assets[name], EntryInput{Title: name, Kind: "spoken"})
		if err != nil {
			t.Fatal(err)
		}
		tracks[name] = r.TrackID
	}
	tagged := Tagged{Album: "Due COUR.1"}
	entry := func(name string, n int) NewEntry {
		return NewEntry{TrackID: tracks[name], AssetID: assets[name], Disc: 1, Track: n}
	}
	if _, err := s.MakeAlbums(ctx, []NewAlbum{{Title: " ", Tagged: tagged, Entries: []NewEntry{entry("DUE01", 1)}}}); err == nil {
		t.Fatal("blank title accepted")
	}
	g, err := s.MakeAlbums(ctx, []NewAlbum{{Title: "ARIA The STATION Due COUR.1", Tagged: tagged,
		Entries: []NewEntry{entry("DUE01", 1), entry("DUE02", 2), {TrackID: tracks["DUE03"], AssetID: assets["DUE01"]}}}})
	if err != nil || g == 0 {
		t.Fatalf("make: %d %v", g, err)
	}
	albums, _ := s.Albums(ctx, 10, 0, false)
	if len(albums) != 1 || albums[0].Title != "ARIA The STATION Due COUR.1" || albums[0].Tracks != 2 {
		t.Fatalf("albums %+v", albums) // DUE03 with another track's file is left out
	}
	if a, _ := s.Attention(ctx); a.WithoutAlbum != 1 {
		t.Fatalf("attention %+v", a)
	}
	if list, _ := s.Tracks(ctx, 10, 0, NoAlbum); len(list) != 1 || list[0].Title != "DUE03" {
		t.Fatalf("no album %+v", list)
	}
	if list, _ := s.Tracks(ctx, 10, 0, NoArtist); len(list) != 3 {
		t.Fatalf("no artist %+v", list)
	}
	if _, err := s.Tracks(ctx, 10, 0, "nope"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown filter: %v", err)
	}

	// Later imports of the folder land in the same album; the files already there are recognized.
	in := EntryInput{Title: "DUE03", Album: "Due COUR.1", DiscNo: 1, TrackNo: 3, Tagged: &Tagged{Album: "Due COUR.1", Disc: 1, Track: 3},
		AlbumTags: &Tagged{Album: "Due COUR.1"}}
	if r, err := s.Publish(ctx, assets["DUE03"], in); err != nil || r.EntryID == 0 {
		t.Fatalf("later import: %+v %v", r, err)
	}
	in = EntryInput{Title: "DUE01", Album: "Due COUR.1", DiscNo: 1, TrackNo: 1, Tagged: &Tagged{Album: "Due COUR.1", Disc: 1, Track: 1},
		AlbumTags: &Tagged{Album: "Due COUR.1"}}
	if r, err := s.Publish(ctx, assets["DUE01"], in); err != nil || r.Created {
		t.Fatalf("re-import: %+v %v", r, err)
	}
	if albums, _ := s.Albums(ctx, 10, 0, false); len(albums) != 1 || albums[0].Tracks != 3 {
		t.Fatalf("after later import %+v", albums)
	}
	// Nothing to make for tracks already on an album.
	if g2, err := s.MakeAlbums(ctx, []NewAlbum{{Title: "Again", Tagged: Tagged{Album: "Other"}, Entries: []NewEntry{entry("DUE01", 1)}}}); err != nil || g2 != 0 {
		t.Fatalf("again: %d %v", g2, err)
	}

	if _, conflicts, err := s.Undo(ctx, g); err != nil || len(conflicts) != 0 {
		t.Fatalf("undo: %v %+v", err, conflicts)
	}
	if a, _ := s.Attention(ctx); a.WithoutAlbum != 2 {
		t.Fatalf("after undo %+v", a)
	}
}
