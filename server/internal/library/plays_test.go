package library

import (
	"context"
	"fmt"
	"testing"
	"time"
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

	// "Continue" is simply the latest playback: the song, 10 s in, in its album.
	c, err := s.Continue(ctx)
	if err != nil || c == nil || c.Title != "Song" || c.PositionMS != 10_000 || c.AlbumID != musicAlbum || c.Finished {
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
	if c, _ := s.Continue(ctx); c == nil || c.Title != "Episode 1" || !c.Finished {
		t.Fatalf("after finishing the drama, continue = %+v (want it, marked finished)", c)
	}
	if spoken, _ := s.UnfinishedSpoken(ctx, 10); len(spoken) != 0 {
		t.Fatalf("finished drama still listed as unfinished: %+v", spoken)
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

// A late or repeated report never takes the position back, a newer one may (seeking back), and a
// late report of an old playback does not make it the latest (review #8).
func TestPlayReportsKeepTheirOrder(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	drama := assetWithDuration(t, s, "o1", 30*60_000)
	s.Publish(ctx, drama, EntryInput{Title: "Drama", Kind: "spoken"})
	now := time.Now().UnixMilli()
	s.RecordPlay(ctx, PlayReport{Session: "a", AssetID: drama, PositionMS: 60_000, ListenedMS: 60_000, Seq: 1, At: now})
	s.RecordPlay(ctx, PlayReport{Session: "a", AssetID: drama, PositionMS: 600_000, ListenedMS: 600_000, Seq: 3, At: now + 1})
	s.RecordPlay(ctx, PlayReport{Session: "a", AssetID: drama, PositionMS: 300_000, ListenedMS: 300_000, Seq: 2, At: now}) // late
	if pos, _ := s.ResumePosition(ctx, drama); pos != 600_000 {
		t.Fatalf("a late report took the position back to %d", pos)
	}
	s.RecordPlay(ctx, PlayReport{Session: "a", AssetID: drama, PositionMS: 100_000, ListenedMS: 600_000, Seq: 4, At: now + 2}) // a seek back
	if pos, _ := s.ResumePosition(ctx, drama); pos != 100_000 {
		t.Fatalf("a newer seek back ignored: %d", pos)
	}
	var heard int64
	s.db.QueryRow(`SELECT listened_ms FROM plays WHERE session = 'a'`).Scan(&heard)
	if heard != 600_000 {
		t.Fatalf("heard %d", heard)
	}

	// Another device starts something after; then a report of the first playback, made before
	// that, arrives late. The newer playback stays the latest.
	time.Sleep(5 * time.Millisecond)
	song := assetWithDuration(t, s, "o2", 200_000)
	s.Publish(ctx, song, EntryInput{Title: "Song"})
	s.RecordPlay(ctx, PlayReport{Session: "b", AssetID: song, PositionMS: 1000, ListenedMS: 1000, Seq: 1, At: time.Now().UnixMilli()})
	time.Sleep(5 * time.Millisecond)
	s.RecordPlay(ctx, PlayReport{Session: "a", AssetID: drama, PositionMS: 110_000, ListenedMS: 610_000, Seq: 5, At: now + 3})
	var latest string
	s.db.QueryRow(`SELECT session FROM plays ORDER BY updated_at DESC, id DESC LIMIT 1`).Scan(&latest)
	if latest != "b" {
		t.Fatalf("the late report made %q the latest", latest)
	}
}

// Rows sharing a millisecond at a page edge are neither lost nor repeated (review #25).
func TestHistoryCursorKeepsEqualTimestampRows(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	song := assetWithDuration(t, s, "h1", 200_000)
	s.Publish(ctx, song, EntryInput{Title: "Song"})
	for i := 0; i < 51; i++ {
		s.RecordPlay(ctx, PlayReport{Session: fmt.Sprint("s", i), AssetID: song, PositionMS: 100_000, ListenedMS: 100_000})
	}
	s.db.Exec(`UPDATE plays SET updated_at = 1000 + id`)
	s.db.Exec(`UPDATE plays SET updated_at = 1000 WHERE id IN (1, 2)`) // the two oldest share a millisecond
	seen := map[int64]bool{}
	before, beforeID := int64(0), int64(0)
	for page := 0; page < 10; page++ {
		h, err := s.History(ctx, 50, before, beforeID)
		if err != nil {
			t.Fatal(err)
		}
		if len(h) == 0 {
			break
		}
		for _, x := range h {
			if seen[x.PlayID] {
				t.Fatalf("play %d twice", x.PlayID)
			}
			seen[x.PlayID] = true
		}
		before, beforeID = h[len(h)-1].UpdatedAt, h[len(h)-1].PlayID
	}
	if len(seen) != 51 {
		t.Fatalf("saw %d of 51", len(seen))
	}
}
