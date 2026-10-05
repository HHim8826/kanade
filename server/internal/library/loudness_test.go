package library

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"
)

// An album's loudness is its measured songs' taken together by power, each weighed by its length,
// and follows the album as songs move (a merge); failures are counted apart and a measurement is
// never lost to a later failure (review #136).
func TestAlbumLoudness(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	f := fixture{t, s}
	a1, a2, a3 := f.song("l1", "One", "Loud", 1, 1), f.song("l2", "Two", "Loud", 1, 2), f.song("l3", "Three", "Quiet", 1, 1)
	A, B := f.albumOf(a1.EntryID), f.albumOf(a3.EntryID)
	asset := func(entry int64) int64 {
		var id int64
		s.db.QueryRow(`SELECT asset_id FROM album_entries WHERE id = ?`, entry).Scan(&id)
		return id
	}
	if err := s.SetLoudness(ctx, asset(a1.EntryID), -10, -0.5, MethodEBUR128); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLoudness(ctx, asset(a2.EntryID), -20, -6, MethodEBUR128); err != nil {
		t.Fatal(err)
	}
	got, err := s.AlbumLoudness(ctx, []int64{A, B})
	if err != nil {
		t.Fatal(err)
	}
	want := math.Round(10*math.Log10((math.Pow(10, -1)+math.Pow(10, -2))/2)*10) / 10 // −12.6
	if a := got[A]; a.LUFS != want || a.Peak != -0.5 || a.Measured != 2 || a.Songs != 2 {
		t.Fatalf("album A = %+v, want %v LUFS", a, want)
	}
	if _, ok := got[B]; ok {
		t.Fatal("an album with nothing measured has a loudness")
	}
	// A failure is recorded for an unmeasured file, never over a measurement.
	if err := s.LoudnessFailed(ctx, asset(a3.EntryID), "bad"); err != nil {
		t.Fatal(err)
	}
	if err := s.LoudnessFailed(ctx, asset(a1.EntryID), "flaky"); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.AssetLoudness(ctx, []int64{asset(a1.EntryID), asset(a3.EntryID)}); len(l) != 1 || l[asset(a1.EntryID)].LUFS != -10 {
		t.Fatalf("assets = %+v", l)
	}
	st, err := s.LoudnessStatus(ctx)
	if err != nil || st.Measured != 2 || st.Failed != 1 || st.Files != 3 || st.Median == nil || *st.Median != -15 {
		t.Fatalf("status = %+v %v", st, err)
	}
	if u, _ := s.Unmeasured(ctx, 0, 10, false); len(u) != 0 {
		t.Fatalf("unmeasured without failed = %+v", u)
	}
	if u, _ := s.Unmeasured(ctx, 0, 10, true); len(u) != 1 || u[0].AssetID != asset(a3.EntryID) {
		t.Fatalf("unmeasured with failed = %+v", u)
	}
	// Merged into A, the quiet album's song counts there.
	if err := s.SetLoudness(ctx, asset(a3.EntryID), -30, -20, MethodEBUR128); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.MergeAlbums(ctx, MergeRequest{Albums: []int64{A, B}, Into: A}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.AlbumLoudness(ctx, []int64{A})
	if a := got[A]; a.Measured != 3 || a.LUFS >= want {
		t.Fatalf("after the merge, album A = %+v", a)
	}
}

// The median is the middle measurement (or the mean of the middle two), worked out at most once a
// minute however often it is asked for (review #146).
func TestLoudnessMedian(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	f := fixture{t, s}
	set := func(sha string, lufs float64) {
		t.Helper()
		e := f.song(sha, sha, "Album", 1, 1)
		var id int64
		s.db.QueryRow(`SELECT asset_id FROM album_entries WHERE id = ?`, e.EntryID).Scan(&id)
		if err := s.SetLoudness(ctx, id, lufs, 0, MethodEBUR128); err != nil {
			t.Fatal(err)
		}
	}
	if m, err := s.LoudnessMedian(ctx); err != nil || m != nil {
		t.Fatalf("none measured: %v %v", m, err)
	}
	s.medianAt = time.Time{}
	for _, v := range []float64{-20, -8, -14} {
		set(fmt.Sprint(v), v)
	}
	if m, _ := s.LoudnessMedian(ctx); m == nil || *m != -14 {
		t.Fatalf("odd: %v", m)
	}
	set("-9", -9)
	if m, _ := s.LoudnessMedian(ctx); *m != -14 {
		t.Fatalf("worked out again within the minute: %v", *m)
	}
	s.medianAt = s.medianAt.Add(-medianFor)
	if m, _ := s.LoudnessMedian(ctx); *m != -11.5 {
		t.Fatalf("even: %v", *m)
	}
}
