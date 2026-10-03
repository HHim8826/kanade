package library

import (
	"context"
	"errors"
	"testing"
)

type fixture struct {
	t *testing.T
	s *Store
}

func (f fixture) song(sha, title string, album string, disc, track int) PublishResult {
	f.t.Helper()
	r, err := f.s.Publish(context.Background(), verifiedAsset(f.t, f.s, sha), EntryInput{Title: title, Artist: "x" + title, Album: album,
		AlbumArtist: "AA " + album, DiscNo: disc, TrackNo: track})
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f fixture) albumOf(entry int64) int64 {
	var id int64
	f.s.db.QueryRow(`SELECT album_id FROM album_entries WHERE id = ?`, entry).Scan(&id)
	return id
}

func (f fixture) shape(albumID int64) (entries int, sections map[int]string, merged int64) {
	d, err := f.s.Album(context.Background(), albumID)
	if err != nil {
		f.t.Fatal(err)
	}
	if d == nil {
		return 0, nil, 0
	}
	return len(d.Entries), d.Sections, d.MergedInto
}

// Several albums become one, each a named section of it (a disc of one of them a section of its
// own); a file it would have twice is not added again; the albums emptied point at it; undo puts
// everything back (review #83, #82).
func TestMergeAlbumsIntoSections(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	f := fixture{t, s}
	a1 := f.song("a1", "A1", "A", 1, 1)
	f.song("a2", "A2", "A", 1, 2)
	b1 := f.song("b1", "B1", "B", 1, 1)
	b2 := f.song("b2", "B2", "B", 2, 1)
	c1 := f.song("a1", "A1", "C", 1, 1) // the same file as A1
	A, B, C := f.albumOf(a1.EntryID), f.albumOf(b1.EntryID), f.albumOf(c1.EntryID)

	req := MergeRequest{Albums: []int64{A, B, C}, Title: "Collection", AlbumArtist: "Various Artists", Sections: true}
	p, err := s.PlanMerge(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if p.Target.ID != 0 || len(p.Moves) != 5 || len(p.Emptied) != 3 || p.Sections[1] != "A" || p.Sections[2] != "B Disc 1" ||
		p.Sections[3] != "B Disc 2" || p.Sections[4] != "C" || !p.Moves[4].Duplicate || p.Moves[3].Disc != 3 {
		t.Fatalf("plan %+v", p)
	}
	album, g, err := s.MergeAlbums(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	n, sections, _ := f.shape(album)
	if n != 4 || len(sections) != 4 || sections[2] != "B Disc 1" {
		t.Fatalf("merged: %d entries, sections %v", n, sections)
	}
	for _, id := range []int64{A, B, C} {
		if n, _, into := f.shape(id); n != 0 || into != album {
			t.Fatalf("album %d after: %d entries, merged into %d", id, n, into)
		}
	}
	if f.albumOf(b2.EntryID) != album {
		t.Fatal("B2 not moved")
	}
	if _, conflicts, err := s.Undo(ctx, g); err != nil || len(conflicts) > 0 {
		t.Fatalf("undo %v %v", conflicts, err)
	}
	for id, want := range map[int64]int{A: 2, B: 2, C: 1, album: 0} {
		if n, sec, into := f.shape(id); n != want || into != 0 || len(sec) != 0 {
			t.Fatalf("album %d after undo: %d entries, merged %d, sections %v", id, n, into, sec)
		}
	}

	// Into one of them, numbers kept: the other album's songs join it.
	if _, _, err := s.MergeAlbums(ctx, MergeRequest{Albums: []int64{B, A}, Into: A}); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := f.shape(A); n != 4 {
		t.Fatalf("into A: %d", n)
	}
	if _, _, into := f.shape(B); into != A {
		t.Fatal("B not pointing at A")
	}
	// Every album is checked first.
	if _, _, err := s.MergeAlbums(ctx, MergeRequest{Albums: []int64{C, 9999}, Title: "X"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing album: %v", err)
	}
	if _, _, err := s.MergeAlbums(ctx, MergeRequest{Albums: []int64{C, B}, Title: "X"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("merged album: %v", err)
	}
	if n, _, into := f.shape(C); n != 1 || into != 0 {
		t.Fatal("C changed by a refused merge")
	}
}

// Details for several albums or songs at once, removing albums, and favorites: all or nothing,
// one edit (review #83).
func TestBatchEdits(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	f := fixture{t, s}
	a := f.song("a", "A", "One", 1, 1)
	b := f.song("b", "B", "Two", 1, 1)
	A, B := f.albumOf(a.EntryID), f.albumOf(b.EntryID)
	g, err := s.EditAlbums(ctx, []int64{A, B}, AlbumFields{AlbumArtist: Str("Ensemble"), Date: Str("2024")})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{A, B} {
		d, _ := s.Album(ctx, id)
		if d.AlbumArtist != "Ensemble" || d.Date != "2024" || d.Entries[0].Artist == "Ensemble" {
			t.Fatalf("album %d: %+v", id, d.AlbumSummary)
		}
	}
	if _, _, err := s.Undo(ctx, g); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Album(ctx, A); d.AlbumArtist != "AA One" {
		t.Fatalf("after undo %q", d.AlbumArtist)
	}
	if _, err := s.EditAlbums(ctx, []int64{A, 777}, AlbumFields{Date: Str("1999")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if d, _ := s.Album(ctx, A); d.Date == "1999" {
		t.Fatal("changed by a refused edit")
	}
	if _, err := s.EditTracks(ctx, []int64{a.TrackID, b.TrackID}, TrackFields{Kind: Str("spoken")}); err != nil {
		t.Fatal(err)
	}
	if tr, _ := s.Track(ctx, b.TrackID); tr.Kind != "spoken" {
		t.Fatalf("kind %s", tr.Kind)
	}
	g, err = s.RemoveAlbums(ctx, []int64{A, B})
	if err != nil {
		t.Fatal(err)
	}
	if n, _, _ := f.shape(A); n != 0 {
		t.Fatal("not removed")
	}
	if tr, _ := s.Track(ctx, a.TrackID); tr == nil {
		t.Fatal("the song went with its album")
	}
	s.Undo(ctx, g)
	if n, _, _ := f.shape(B); n != 1 {
		t.Fatal("undo of the removal")
	}
	if err := s.SetFavorites(ctx, []int64{a.TrackID, b.TrackID}, []int64{A}, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFavorites(ctx, []int64{a.TrackID}, []int64{B, 555}, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown album: %v", err)
	}
	ids, _ := s.FavoriteIDs(ctx)
	if len(ids.Tracks) != 2 || len(ids.Albums) != 1 {
		t.Fatalf("favorites after a refused change %+v", ids)
	}
}

// Songs go into an album: songs of the library join with their file, entries move from where they
// are; into a section of their own, named, after the album's last; undo takes it back.
func TestPlaceSongs(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	f := fixture{t, s}
	loose, _ := s.Publish(ctx, verifiedAsset(t, s, "l"), EntryInput{Title: "Loose", Artist: "x"})
	x1 := f.song("x1", "X1", "X", 1, 1)
	x2 := f.song("x2", "X2", "X", 1, 2)
	X := f.albumOf(x1.EntryID)
	mix, _, err := s.PlaceSongs(ctx, PlaceRequest{Tracks: []int64{loose.TrackID, x1.TrackID}, Title: "Mix"})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := s.Album(ctx, mix)
	if len(d.Entries) != 2 || d.Entries[0].TrackNo != 1 || d.Entries[1].Title != "X1" {
		t.Fatalf("mix %+v", d.Entries)
	}
	if n, _, _ := f.shape(X); n != 2 {
		t.Fatal("a song added elsewhere left its album")
	}
	_, g, err := s.PlaceSongs(ctx, PlaceRequest{Entries: []int64{x2.EntryID}, Album: mix, Section: "Bonus"})
	if err != nil {
		t.Fatal(err)
	}
	d, _ = s.Album(ctx, mix)
	if len(d.Entries) != 3 || d.Entries[2].DiscNo != 2 || d.Sections[2] != "Bonus" {
		t.Fatalf("moved %+v %v", d.Entries, d.Sections)
	}
	if n, _, _ := f.shape(X); n != 1 {
		t.Fatal("the moved entry is still on X")
	}
	s.Undo(ctx, g)
	if n, sec, _ := f.shape(mix); n != 2 || len(sec) != 0 {
		t.Fatalf("after undo %d %v", n, sec)
	}
	// The same file twice is not added.
	if _, _, err := s.PlaceSongs(ctx, PlaceRequest{Tracks: []int64{x1.TrackID}, Album: mix}); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := f.shape(mix); n != 2 {
		t.Fatalf("twice: %d", n)
	}
}
