package library

import (
	"context"
	"testing"
)

func assetWithDuration(t *testing.T, s *Store, sha string, durationMS int64) int64 {
	t.Helper()
	ctx := context.Background()
	a, err := s.CreateAsset(ctx, Asset{SHA256: sha, Size: 1, Format: "flac", Codec: "flac", DurationMS: durationMS})
	if err != nil {
		t.Fatal(err)
	}
	s.MarkVerified(ctx, a.ID, "d-"+sha)
	return a.ID
}

func TestPlayThreshold(t *testing.T) {
	cases := []struct{ duration, need int64 }{
		{20_000, -1},           // under 30 s: never counts
		{3 * 60_000, 90_000},   // half of a 3-minute song
		{30 * 60_000, 240_000}, // capped at 4 minutes for a 30-minute drama
	}
	for _, c := range cases {
		if got := playThreshold(c.duration); got != c.need {
			t.Errorf("threshold(%d) = %d, want %d", c.duration, got, c.need)
		}
	}
}

func TestPlaySessionIsDedupedAndMonotonic(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	asset := assetWithDuration(t, s, "p1", 200_000)
	r, _ := s.Publish(ctx, asset, EntryInput{Title: "Song", Album: "A", AlbumArtist: "X"})
	_ = r
	report := func(pos, heard int64, finished bool) {
		t.Helper()
		if err := s.RecordPlay(ctx, PlayReport{Session: "sess-1", AssetID: asset, PositionMS: pos, ListenedMS: heard, Finished: finished}); err != nil {
			t.Fatal(err)
		}
	}
	report(10_000, 10_000, false)
	report(120_000, 110_000, false) // past half: counts
	report(50_000, 40_000, false)   // a late, older report must not undo anything
	var n, counted, heard int64
	s.db.QueryRow(`SELECT count(*), max(counted), max(listened_ms) FROM plays`).Scan(&n, &counted, &heard)
	if n != 1 || counted != 1 || heard != 110_000 {
		t.Fatalf("rows=%d counted=%d heard=%d", n, counted, heard)
	}
	if err := s.RecordPlay(ctx, PlayReport{Session: "", AssetID: asset}); err == nil {
		t.Fatal("empty session accepted")
	}
	if err := s.RecordPlay(ctx, PlayReport{Session: "x", AssetID: 999}); err == nil {
		t.Fatal("unknown asset accepted")
	}
}

func TestContinueAndSpoken(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	song := assetWithDuration(t, s, "song", 240_000)
	drama := assetWithDuration(t, s, "drama", 1_800_000)
	songRes, _ := s.Publish(ctx, song, EntryInput{Title: "Song", Album: "Music", AlbumArtist: "X"})
	s.Publish(ctx, drama, EntryInput{Title: "Episode 1", Album: "Drama CD", AlbumArtist: "Y", Kind: "spoken"})
	var musicAlbum int64
	s.db.QueryRow(`SELECT album_id FROM album_entries WHERE id = ?`, songRes.EntryID).Scan(&musicAlbum)

	s.RecordPlay(ctx, PlayReport{Session: "a", AssetID: drama, PositionMS: 600_000, ListenedMS: 600_000})
	s.RecordPlay(ctx, PlayReport{Session: "b", AssetID: song, AlbumID: musicAlbum, PositionMS: 10_000, ListenedMS: 10_000})

	// The song is newer but only 10 s in, so "continue" is the drama at 10 minutes.
	c, err := s.Continue(ctx)
	if err != nil || c == nil || c.Title != "Episode 1" || c.PositionMS != 600_000 || c.Kind != "spoken" {
		t.Fatalf("continue = %+v %v", c, err)
	}
	if pos, _ := s.ResumePosition(ctx, drama); pos != 600_000 {
		t.Fatalf("resume position = %d", pos)
	}
	spoken, _ := s.UnfinishedSpoken(ctx, 10)
	if len(spoken) != 1 {
		t.Fatalf("unfinished spoken = %d", len(spoken))
	}
	s.RecordPlay(ctx, PlayReport{Session: "c", AssetID: drama, PositionMS: 1_800_000, ListenedMS: 1_200_000, Finished: true})
	if c, _ := s.Continue(ctx); c != nil {
		t.Fatalf("finished drama still offered: %+v", c)
	}
	recent, _ := s.RecentlyPlayedAlbums(ctx, 10)
	if len(recent) != 1 || recent[0].ID != musicAlbum { // the drama plays had no album context
		t.Fatalf("recent albums = %+v", recent)
	}
	for i := 0; i < 20; i++ {
		id, _ := s.RandomAlbum(ctx)
		if id != musicAlbum {
			t.Fatalf("random album %d is not the music album", id)
		}
	}
}
