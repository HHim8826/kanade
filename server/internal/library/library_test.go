package library

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	tracks, _ := s.Tracks(ctx, 10, 0, "")
	if len(tracks) != 2 {
		t.Fatalf("tracks = %d, want 2", len(tracks))
	}
}

func TestUnverifiedAssetsAreHidden(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	a, _ := s.CreateAsset(ctx, Asset{SHA256: "ee", Size: 1, Format: "mp3", Codec: "mp3"})
	s.Publish(ctx, a.ID, EntryInput{Title: "pending", Album: "X"})
	if tracks, _ := s.Tracks(ctx, 10, 0, ""); len(tracks) != 0 {
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
		"ｳﾝﾃﾞｨｰﾈ":  {1, 0, 0}, // half-width katakana query
		"aria":     {0, 1, 0}, // full-width letters in the album title
		"牧野":       {1, 0, 1}, // two characters: below the trigram length
		"100%":     {1, 0, 0}, // % must be literal
		"0%":       {1, 0, 0},
		"%":        {1, 0, 0},
		"zzz":      {0, 0, 0},
		"うんでぃ":     {1, 0, 0}, // hiragana finds katakana
		"牧野 由依":    {1, 0, 1}, // spaces do not matter
		"the orig": {0, 1, 0},
		"diーne":    {0, 0, 0},
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

// Searching uses the trigram index unless the words hold what LIKE and GLOB both treat as
// wildcards; each object's row is found by its rowid; and each kind has its own limit (review
// #163).
func TestSearchUsesTheIndex(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	for i := range 30 {
		s.Publish(ctx, verifiedAsset(t, s, fmt.Sprint("s", i)), EntryInput{Title: fmt.Sprint("Common song ", i), Artist: "x"})
	}
	s.Publish(ctx, verifiedAsset(t, s, "al"), EntryInput{Title: "t", Artist: "Common singer", Album: "Common album", AlbumArtist: "Z"})
	s.Publish(ctx, verifiedAsset(t, s, "st"), EntryInput{Title: "a*b_c", Artist: "y"})
	plan := func(match, pattern string) string {
		t.Helper()
		var id, parent, unused int
		var detail string
		if err := s.db.QueryRow(`EXPLAIN QUERY PLAN SELECT ref_id FROM search_index WHERE `+match+` AND kind = 'track'`, pattern).
			Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		return detail
	}
	if p := plan(`text LIKE ?`, "%common%"); !strings.Contains(p, ":L") {
		t.Errorf("LIKE: %s", p)
	}
	if p := plan(`text GLOB ?`, "*100%*"); !strings.Contains(p, ":G") {
		t.Errorf("GLOB: %s", p)
	}
	r, err := s.Search(ctx, "common", 20)
	if err != nil || len(r.Tracks) != 20 || len(r.Albums) != 1 || len(r.Artists) != 1 {
		t.Fatalf("common: %d %d %d %v", len(r.Tracks), len(r.Albums), len(r.Artists), err)
	}
	for _, q := range []string{"a*b_c", "b_c", "a*b"} {
		if r, _ := s.Search(ctx, q, 20); len(r.Tracks) != 1 {
			t.Errorf("%q: %d", q, len(r.Tracks))
		}
	}
	// A renamed object is found by its new name only: its old row was replaced.
	var id int64
	s.db.QueryRow(`SELECT id FROM tracks WHERE title = 'Common song 3'`).Scan(&id)
	if _, err := s.ApplyChanges(ctx, SourceUser, "rename", []Change{{"track", id, "title", Str("Renamed tune")}}); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Search(ctx, "renamed", 20); len(r.Tracks) != 1 {
		t.Fatalf("new name: %d", len(r.Tracks))
	}
	if r, _ := s.Search(ctx, "common song 3", 20); len(r.Tracks) != 0 {
		t.Fatalf("old name still found: %+v", r.Tracks)
	}
	var rows int
	s.db.QueryRow(`SELECT count(*) FROM search_index WHERE kind = 'track' AND ref_id = ?`, id).Scan(&rows)
	if rows != 1 {
		t.Fatalf("%d rows for the track", rows)
	}
}

// Rebuilding a large index takes seconds, not minutes.
func TestSearchIndexRebuild(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	tx, _ := s.db.Begin()
	for i := range 20000 {
		tx.Exec(`INSERT INTO tracks (id, title, artist, created_at, updated_at) VALUES (?, ?, 'x', 0, 0)`, i+1, fmt.Sprint("song ", i))
	}
	tx.Commit()
	s.db.Exec(`DELETE FROM settings WHERE key = 'search_index'`)
	t0 := time.Now()
	if err := s.EnsureSearchIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d > 20*time.Second {
		t.Fatalf("rebuilt in %v", d)
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM search_index WHERE text LIKE '%song19999%' AND rowid = ?`, indexRow("track", 20000)).Scan(&n)
	if n != 1 {
		t.Fatal("not found after the rebuild")
	}
}
