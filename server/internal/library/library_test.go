package library

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/HHim8826/kanade/server/internal/db"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return New(d)
}

func verifiedAsset(t *testing.T, s *Store, sha string) int64 {
	t.Helper()
	ctx := context.Background()
	a, err := s.CreateAsset(ctx, Asset{SHA256: sha, Size: 1000, Format: "flac", Codec: "flac", DurationMS: 60000})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkVerified(ctx, a.ID, "drive-"+sha); err != nil {
		t.Fatal(err)
	}
	return a.ID
}

func TestSameFileTwiceIsOneAssetAndNoNewEntry(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	a1, err := s.CreateAsset(ctx, Asset{SHA256: "aa", Size: 1000, Format: "flac", Codec: "flac"})
	if err != nil {
		t.Fatal(err)
	}
	a2, err := s.CreateAsset(ctx, Asset{SHA256: "aa", Size: 1000, Format: "flac", Codec: "flac"})
	if err != nil {
		t.Fatal(err)
	}
	if a1.ID != a2.ID {
		t.Fatalf("duplicate content got two assets: %d, %d", a1.ID, a2.ID)
	}
	s.MarkVerified(ctx, a1.ID, "drive-aa")
	in := EntryInput{Title: "Undine", Artist: "Makino Yui", Album: "Undine", AlbumArtist: "Makino Yui", TrackNo: 1}
	r1, err := s.Publish(ctx, a1.ID, in)
	if err != nil || !r1.Created {
		t.Fatalf("first publish: %+v %v", r1, err)
	}
	r2, err := s.Publish(ctx, a1.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Created || r2.TrackID != r1.TrackID || r2.EntryID != r1.EntryID {
		t.Fatalf("re-import created something: %+v vs %+v", r2, r1)
	}
}

func TestSameFileOnAnotherAlbumSharesAsset(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	asset := verifiedAsset(t, s, "bb")
	single, _ := s.Publish(ctx, asset, EntryInput{Title: "Euforia", Artist: "Makino Yui", Album: "Euforia", AlbumArtist: "Makino Yui", TrackNo: 1})
	best, err := s.Publish(ctx, asset, EntryInput{Title: "Euforia", Artist: "Makino Yui", Album: "ARIA The BEST", AlbumArtist: "Various Artists", TrackNo: 5})
	if err != nil {
		t.Fatal(err)
	}
	if !best.Created || best.EntryID == single.EntryID {
		t.Fatalf("second album entry not created: %+v", best)
	}
	if best.TrackID != single.TrackID {
		t.Fatal("same file should stay the same recording")
	}
	albums, _ := s.Albums(ctx, 10, 0, false)
	if len(albums) != 2 {
		t.Fatalf("albums = %d, want 2", len(albums))
	}
}

func TestSameTitleDifferentFilesStaySeparate(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	studio, _ := s.Publish(ctx, verifiedAsset(t, s, "cc"), EntryInput{Title: "Rainbow", Artist: "ROUND TABLE"})
	live, _ := s.Publish(ctx, verifiedAsset(t, s, "dd"), EntryInput{Title: "Rainbow", Artist: "ROUND TABLE"})
	if studio.TrackID == live.TrackID {
		t.Fatal("different recordings merged by title")
	}
	if studio.EntryID != 0 || live.EntryID != 0 {
		t.Fatal("standalone tracks must not get an invented album")
	}
	tracks, _ := s.Tracks(ctx, 10, 0)
	if len(tracks) != 2 {
		t.Fatalf("tracks = %d, want 2", len(tracks))
	}
}

func TestUnverifiedAssetsAreHidden(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	a, _ := s.CreateAsset(ctx, Asset{SHA256: "ee", Size: 1, Format: "mp3", Codec: "mp3"})
	s.Publish(ctx, a.ID, EntryInput{Title: "pending", Album: "X"})
	if tracks, _ := s.Tracks(ctx, 10, 0); len(tracks) != 0 {
		t.Fatal("track with an unverified asset is listed")
	}
	if _, err := s.StreamTarget(ctx, a.ID); err == nil {
		t.Fatal("unverified asset is streamable")
	}
}

func TestSearchNormalizesWidthAndCase(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	s.Publish(ctx, verifiedAsset(t, s, "ff"), EntryInput{Title: "ウンディーネ", Artist: "牧野由依",
		Album: "ＡＲＩＡ The ORIGINATION", AlbumArtist: "Various Artists"})
	s.Publish(ctx, verifiedAsset(t, s, "gg"), EntryInput{Title: "100% Love", Artist: "x"})

	cases := map[string]struct{ tracks, albums, artists int }{
		"ｳﾝﾃﾞｨｰﾈ": {1, 0, 0}, // half-width katakana query
		"aria":    {0, 1, 0}, // full-width letters in the album title
		"牧野":      {1, 0, 1}, // two characters: below the trigram length
		"100%":    {1, 0, 0}, // % must be literal
		"0%":      {1, 0, 0},
		"%":       {1, 0, 0},
		"zzz":     {0, 0, 0},
	}
	for q, want := range cases {
		r, err := s.Search(ctx, q, 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Tracks) != want.tracks || len(r.Albums) != want.albums || len(r.Artists) != want.artists {
			t.Errorf("search %q: tracks %d albums %d artists %d, want %+v", q, len(r.Tracks), len(r.Albums), len(r.Artists), want)
		}
	}
}

func TestAlbumDetailOrdersByDiscAndTrack(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	for i, tc := range []struct{ disc, track int }{{2, 1}, {1, 2}, {1, 1}} {
		s.Publish(ctx, verifiedAsset(t, s, string(rune('h'+i))), EntryInput{Title: "t", Album: "Box", AlbumArtist: "A", DiscNo: tc.disc, TrackNo: tc.track})
	}
	albums, _ := s.Albums(ctx, 10, 0, false)
	d, err := s.Album(ctx, albums[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	got := []int{}
	for _, e := range d.Entries {
		got = append(got, e.DiscNo*10+e.TrackNo)
	}
	if len(got) != 3 || got[0] != 11 || got[1] != 12 || got[2] != 21 {
		t.Fatalf("order = %v", got)
	}
	if d.Tracks != 3 || d.DurationMS != 180000 {
		t.Fatalf("summary = %+v", d.AlbumSummary)
	}
}
