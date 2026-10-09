package library

import (
	"context"
	"fmt"
	"slices"
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
	s.db.Exec(`UPDATE plays SET started_at = started_at - 600000 WHERE session = 'a'`) // heard over ten minutes (review #102)
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

// A playback's first report that sat in the network keeps the time it was made: the device's clock
// offset comes from its quickest recent report, not from that one (review #8). A later report
// arriving before an earlier one moves the start back; each device's clock offset is its own.
func TestDelayedFirstReportKeepsItsTime(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	newSong := assetWithDuration(t, s, "f1", 200_000)
	s.Publish(ctx, newSong, EntryInput{Title: "New Song"})
	oldSong := assetWithDuration(t, s, "f2", 200_000)
	s.Publish(ctx, oldSong, EntryInput{Title: "Old Song"})
	latest := func() string {
		var session string
		s.db.QueryRow(`SELECT session FROM plays ORDER BY updated_at DESC, id DESC LIMIT 1`).Scan(&session)
		return session
	}
	now := time.Now().UnixMilli()
	s.RecordPlay(ctx, PlayReport{Session: "new", AssetID: newSong, PositionMS: 1000, ListenedMS: 1000, Seq: 1, At: now, Client: "phone"})
	// Started a minute earlier on the same clock; its first report arrives only now.
	s.RecordPlay(ctx, PlayReport{Session: "old", AssetID: oldSong, PositionMS: 1000, ListenedMS: 1000, Seq: 1, At: now - 60_000, Client: "phone"})
	if latest() != "new" {
		t.Fatal("a delayed first report became the latest playback")
	}
	var started int64
	s.db.QueryRow(`SELECT started_at FROM plays WHERE session = 'old'`).Scan(&started)
	if started > now-59_000 {
		t.Fatalf("delayed first report dated %d, made at %d", started, now-60_000)
	}
	// The second report of a playback arrives before its first: the start is the first's.
	s.RecordPlay(ctx, PlayReport{Session: "x", AssetID: newSong, PositionMS: 15_000, ListenedMS: 15_000, Seq: 2, At: now - 30_000, Client: "phone"})
	s.RecordPlay(ctx, PlayReport{Session: "x", AssetID: newSong, PositionMS: 0, ListenedMS: 0, Seq: 1, At: now - 45_000, Client: "phone"})
	var start, updated, pos int64
	s.db.QueryRow(`SELECT started_at, updated_at, position_ms FROM plays WHERE session = 'x'`).Scan(&start, &updated, &pos)
	if start > now-44_000 || updated < now-31_000 || pos != 15_000 {
		t.Fatalf("out of order: start %d updated %d pos %d", now-start, now-updated, pos)
	}
	// A device five minutes fast: its reports are dated by its own offset, not another device's,
	// and never in the future.
	time.Sleep(5 * time.Millisecond)
	fast := time.Now().UnixMilli() + 300_000
	s.RecordPlay(ctx, PlayReport{Session: "fast", AssetID: oldSong, PositionMS: 1000, ListenedMS: 1000, Seq: 1, At: fast, Client: "laptop"})
	var fastAt int64
	s.db.QueryRow(`SELECT updated_at FROM plays WHERE session = 'fast'`).Scan(&fastAt)
	if fastAt > time.Now().UnixMilli() || latest() != "fast" {
		t.Fatalf("fast clock: dated %d ahead, latest %s", fastAt-time.Now().UnixMilli(), latest())
	}
	time.Sleep(5 * time.Millisecond)
	s.RecordPlay(ctx, PlayReport{Session: "new", AssetID: newSong, PositionMS: 9000, ListenedMS: 9000, Seq: 2, At: time.Now().UnixMilli(), Client: "phone"})
	if latest() != "new" {
		t.Fatalf("the phone's newer report lost to the fast laptop: %s", latest())
	}
}

// Played from an album merged since (review #152): the home rows and the smart playlists find the
// album it went into, however many merges along, and undo puts them back.
func TestPlayedAlbumsFollowMerges(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	album := func(name string, kind string) (int64, int64) {
		t.Helper()
		asset := assetWithDuration(t, s, name, 1_800_000)
		res, err := s.Publish(ctx, asset, EntryInput{Title: name, Album: name + " album", AlbumArtist: "X", Kind: kind})
		if err != nil {
			t.Fatal(err)
		}
		var id int64
		s.db.QueryRow(`SELECT album_id FROM album_entries WHERE id = ?`, res.EntryID).Scan(&id)
		return asset, id
	}
	drama, A := album("drama", "spoken")
	_, B := album("b", "")
	_, C := album("c", "")
	s.RecordPlay(ctx, PlayReport{Session: "a", AssetID: drama, AlbumID: A, PositionMS: 600_000, ListenedMS: 600_000})
	rules := Rules{Match: "all", Sort: "album", Conditions: []Condition{{Field: "album", Op: "is", IDs: []int64{A}}}}
	check := func(when string, want int64) {
		t.Helper()
		recent, _ := s.RecentlyPlayedAlbums(ctx, 10)
		if len(recent) != 1 || recent[0].ID != want {
			t.Fatalf("%s: recent albums %+v", when, recent)
		}
		if c, _ := s.Continue(ctx); c == nil || c.AlbumID != want {
			t.Fatalf("%s: continue %+v", when, c)
		}
		if spoken, _ := s.UnfinishedSpoken(ctx, 10); len(spoken) != 1 || spoken[0].AlbumID != want {
			t.Fatalf("%s: unfinished %+v", when, spoken)
		}
		if tracks, _, err := s.SmartTracks(ctx, rules, nil, 100); err != nil || !slices.ContainsFunc(tracks, func(it TrackItem) bool { return it.Title == "drama" }) {
			t.Fatalf("%s: album rule %+v %v", when, tracks, err)
		}
	}
	check("before", A)
	g1, err := s.MergeAlbum(ctx, A, B)
	if err != nil {
		t.Fatal(err)
	}
	check("merged into B", B)
	g2, err := s.MergeAlbum(ctx, B, C)
	if err != nil {
		t.Fatal(err)
	}
	check("then into C", C)
	for _, g := range []int64{g2, g1} {
		if _, conflicts, err := s.Undo(ctx, g); err != nil || len(conflicts) != 0 {
			t.Fatalf("undo: %v %+v", err, conflicts)
		}
	}
	check("undone", A)
}

// Recently played albums are read from the latest playback back, a few at a time (review #186):
// an album played long before the others is still found, once, after one played many times since,
// and an album merged since is the one it went into.
func TestRecentlyPlayedAlbumsReadsBack(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	album := func(name string) (int64, int64, int64) {
		t.Helper()
		asset := assetWithDuration(t, s, name, 60_000)
		res, err := s.Publish(ctx, asset, EntryInput{Title: name, Album: name + " album", AlbumArtist: "X"})
		if err != nil {
			t.Fatal(err)
		}
		var id, track int64
		s.db.QueryRow(`SELECT album_id, track_id FROM album_entries WHERE id = ?`, res.EntryID).Scan(&id, &track)
		return asset, track, id
	}
	oa, ot, old := album("old")
	ma, mt, merged := album("merged")
	na, nt, newest := album("newest")
	play := func(i int, asset, track, album int64) {
		if _, err := s.db.Exec(`INSERT INTO plays (session, asset_id, track_id, album_id, started_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			fmt.Sprint("p", i), asset, track, album, i, i); err != nil {
			t.Fatal(err)
		}
	}
	play(1, oa, ot, old)
	play(2, ma, mt, merged)
	for i := range 300 {
		play(10+i, na, nt, newest)
	}
	if _, err := s.MergeAlbum(ctx, merged, newest); err != nil {
		t.Fatal(err)
	}
	recent, err := s.RecentlyPlayedAlbums(ctx, 2)
	if err != nil || len(recent) != 2 || recent[0].ID != newest || recent[1].ID != old {
		t.Fatalf("recent %+v %v", recent, err)
	}
	if recent, _ := s.RecentlyPlayedAlbums(ctx, 10); len(recent) != 2 {
		t.Fatalf("all %+v", recent)
	}
}

// Removed albums keep their rows for undo, but take no place among the recently played: earlier
// ones show instead, and undo brings them back in their place (review #194).
func TestRecentlyPlayedAlbumsLeaveRemovedOut(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	var albums []int64
	for i := range 13 {
		name := fmt.Sprint("album ", i)
		asset := assetWithDuration(t, s, name, 60_000)
		res, err := s.Publish(ctx, asset, EntryInput{Title: name, Album: name, AlbumArtist: "X"})
		if err != nil {
			t.Fatal(err)
		}
		var album, track int64
		s.db.QueryRow(`SELECT album_id, track_id FROM album_entries WHERE id = ?`, res.EntryID).Scan(&album, &track)
		s.db.Exec(`INSERT INTO plays (session, asset_id, track_id, album_id, started_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			name, asset, track, album, i+1, i+1)
		albums = append(albums, album)
	}
	recent := func(limit int) []int64 {
		t.Helper()
		list, err := s.RecentlyPlayedAlbums(ctx, limit)
		if err != nil {
			t.Fatal(err)
		}
		ids := []int64{}
		for _, a := range list {
			ids = append(ids, a.ID)
		}
		return ids
	}
	newer := slices.Clone(albums[1:])
	slices.Reverse(newer) // the latest first
	g, err := s.RemoveAlbums(ctx, newer)
	if err != nil {
		t.Fatal(err)
	}
	if got := recent(12); !slices.Equal(got, albums[:1]) {
		t.Fatalf("the 12 latest removed: %v, want %v", got, albums[:1])
	}
	if got := recent(1); !slices.Equal(got, albums[:1]) {
		t.Fatalf("one asked for: %v", got)
	}
	if _, _, err := s.Undo(ctx, g); err != nil {
		t.Fatal(err)
	}
	if got := recent(12); !slices.Equal(got, newer) {
		t.Fatalf("undone: %v, want %v", got, newer)
	}
	if _, err := s.RemoveAlbums(ctx, albums); err != nil {
		t.Fatal(err)
	}
	if got := recent(12); len(got) != 0 {
		t.Fatalf("all removed: %v", got)
	}
}

// The history is read on from where each few stopped (review #191): playbacks of one millisecond
// across the few read at a time are each read once; a history of one album, of fewer albums than
// asked for, or of many merged into one, is read through and lists each album once.
func TestRecentlyPlayedAlbumsReadOn(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	type song struct{ asset, track, album int64 }
	album := func(name string) song {
		t.Helper()
		asset := assetWithDuration(t, s, name, 60_000)
		res, err := s.Publish(ctx, asset, EntryInput{Title: name, Album: name, AlbumArtist: "X"})
		if err != nil {
			t.Fatal(err)
		}
		x := song{asset: asset}
		s.db.QueryRow(`SELECT album_id, track_id FROM album_entries WHERE id = ?`, res.EntryID).Scan(&x.album, &x.track)
		return x
	}
	n := 0
	play := func(x song, at int64) {
		t.Helper()
		n++
		if _, err := s.db.Exec(`INSERT INTO plays (session, asset_id, track_id, album_id, started_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			fmt.Sprint("p", n), x.asset, x.track, x.album, at, at); err != nil {
			t.Fatal(err)
		}
	}
	recent := func(limit int) []int64 {
		t.Helper()
		list, err := s.RecentlyPlayedAlbums(ctx, limit)
		if err != nil {
			t.Fatal(err)
		}
		ids := []int64{}
		for _, a := range list {
			ids = append(ids, a.ID)
		}
		return ids
	}
	oldest, tie, often := album("oldest"), album("tie"), album("often")
	play(oldest, 100)
	play(tie, 500) // of the same millisecond as the 1,000 after it, and read after them
	for range 1000 {
		play(often, 500)
	}
	if got, want := recent(3), []int64{often.album, tie.album, oldest.album}; !slices.Equal(got, want) {
		t.Fatalf("ties across the few read at a time: %v, want %v", got, want)
	}
	if got := recent(10); len(got) != 3 {
		t.Fatalf("fewer albums than asked for: %v", got)
	}
	// Many albums merged into one: listed once, before what was played earlier.
	into := album("into")
	for i := range 30 {
		x := album(fmt.Sprint("part ", i))
		for j := range 40 {
			play(x, int64(1000+i*40+j))
		}
		if _, err := s.MergeAlbum(ctx, x.album, into.album); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := recent(3), []int64{into.album, often.album, tie.album}; !slices.Equal(got, want) {
		t.Fatalf("merged into one: %v, want %v", got, want)
	}
}

// Merges one after another make a chain of any length (review #201): recently played and the
// listening statistics both follow it to its end, the album still there; undoing the last merge
// brings back the album before it; only albums merged in a loop are none.
func TestLongMergeChains(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	var albums []int64
	var first, firstTrack int64
	for i := range 13 {
		name := fmt.Sprint("chain ", i)
		asset := assetWithDuration(t, s, name, 60_000)
		res, err := s.Publish(ctx, asset, EntryInput{Title: name, Album: name, AlbumArtist: "X"})
		if err != nil {
			t.Fatal(err)
		}
		var album, track int64
		s.db.QueryRow(`SELECT album_id, track_id FROM album_entries WHERE id = ?`, res.EntryID).Scan(&album, &track)
		if i == 0 {
			first, firstTrack = asset, track
		}
		albums = append(albums, album)
	}
	at := time.Now().Add(-time.Hour).UnixMilli()
	res, err := s.db.Exec(`INSERT INTO plays (session, asset_id, track_id, album_id, started_at, updated_at, listened_ms, counted)
		VALUES ('chain', ?, ?, ?, ?, ?, 60000, 1)`, first, firstTrack, albums[0], at-60_000, at)
	if err != nil {
		t.Fatal(err)
	}
	play, _ := res.LastInsertId()
	if err := addListening(ctx, s.db, play, at, 60_000, true, false); err != nil {
		t.Fatal(err)
	}
	var last int64
	for i := range 11 { // 0 -> 1 -> ... -> 11
		if last, err = s.MergeAlbum(ctx, albums[i], albums[i+1]); err != nil {
			t.Fatalf("merge %d: %v", i, err)
		}
	}
	end := albums[11]
	shown := func() ([]int64, []int64) {
		t.Helper()
		recent, err := s.RecentlyPlayedAlbums(ctx, 12)
		if err != nil {
			t.Fatal(err)
		}
		top, err := s.ListeningTop(ctx, 0, time.Now().Add(time.Hour).UnixMilli(), "", "albums", "", 10)
		if err != nil {
			t.Fatal(err)
		}
		var r, st []int64
		for _, a := range recent {
			r = append(r, a.ID)
		}
		for _, a := range top {
			st = append(st, a.ID)
		}
		return r, st
	}
	if now, _, err := s.AlbumNow(ctx, albums[0]); err != nil || now != end {
		t.Fatalf("11 merges along: %d %v, want %d", now, err, end)
	}
	if r, st := shown(); !slices.Equal(r, []int64{end}) || !slices.Equal(st, []int64{end}) {
		t.Fatalf("recently played %v, statistics %v, want [%d]", r, st, end)
	}
	if _, _, err := s.Undo(ctx, last); err != nil {
		t.Fatal(err)
	}
	if r, st := shown(); !slices.Equal(r, []int64{albums[10]}) || !slices.Equal(st, []int64{albums[10]}) {
		t.Fatalf("the last merge undone: %v, %v, want [%d]", r, st, albums[10])
	}
	// A loop, which merging never makes: none, and no endless walk.
	s.db.Exec(`UPDATE albums SET merged_into = ? WHERE id = ?`, albums[0], albums[10])
	if now, _, err := s.AlbumNow(ctx, albums[3]); err == nil || now != 0 {
		t.Fatalf("a loop: %d %v", now, err)
	}
	if r, st := shown(); len(r) != 0 || len(st) != 0 {
		t.Fatalf("albums in a loop listed: %v, %v", r, st)
	}
}
