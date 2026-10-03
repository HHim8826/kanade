package library

import (
	"context"
	"testing"
)

// Random songs come from the whole library, each song as likely as any other however many albums
// or versions it has, only playable ones of the kind asked, not the ones just played unless nothing
// else is left (reviews #72, #73).
func TestRandomTracks(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	// A: on ten albums. B: no album. C: spoken. D: its only file is missing from Drive.
	var a int64
	for i := 0; i < 10; i++ {
		r, err := s.Publish(ctx, verifiedAsset(t, s, "a"), EntryInput{Title: "A", Artist: "x", Album: "Album " + string(rune('0'+i)), TrackNo: 1})
		if err != nil {
			t.Fatal(err)
		}
		a = r.TrackID
	}
	b, _ := s.Publish(ctx, verifiedAsset(t, s, "b"), EntryInput{Title: "B", Artist: "x"})
	e, _ := s.Publish(ctx, verifiedAsset(t, s, "e"), EntryInput{Title: "E", Artist: "y", Album: "E", TrackNo: 1})
	c, _ := s.Publish(ctx, verifiedAsset(t, s, "c"), EntryInput{Title: "C", Artist: "x", Kind: "spoken"})
	d, _ := s.Publish(ctx, verifiedAsset(t, s, "d"), EntryInput{Title: "D", Artist: "x"})
	s.ObserveDriveFile(ctx, DriveObservation{ID: "drive-d"})

	seen := map[int64]int{}
	for i := 0; i < 1500; i++ {
		list, err := s.RandomTracks(ctx, 1, "music", nil)
		if err != nil || len(list) != 1 {
			t.Fatalf("%v %v", list, err)
		}
		seen[list[0].ID]++
	}
	if seen[c.TrackID] != 0 || seen[d.TrackID] != 0 || len(seen) != 3 {
		t.Fatalf("picked %v", seen)
	}
	for _, id := range []int64{a, b.TrackID, e.TrackID} { // a third each, give or take
		if n := seen[id]; n < 400 || n > 600 {
			t.Fatalf("song %d picked %d times of 1500: %v", id, n, seen)
		}
	}
	all, _ := s.RandomTracks(ctx, 10, "", nil)
	if len(all) != 4 {
		t.Fatalf("all kinds: %d", len(all))
	}
	if list, _ := s.RandomTracks(ctx, 5, "music", []int64{a, b.TrackID}); len(list) != 1 || list[0].ID != e.TrackID {
		t.Fatalf("leaving out the ones just played: %+v", list)
	}
	// Everything was just played: anything but the very last one.
	if list, _ := s.RandomTracks(ctx, 5, "music", []int64{a, b.TrackID, e.TrackID}); len(list) != 2 || list[0].ID == e.TrackID || list[1].ID == e.TrackID {
		t.Fatalf("all just played: %+v", list)
	}
	if list, _ := s.RandomTracks(ctx, 5, "spoken", []int64{c.TrackID}); len(list) != 1 || list[0].ID != c.TrackID {
		t.Fatalf("a library of one: %+v", list)
	}
}
