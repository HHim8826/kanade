package library

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Heard time is cut into the 15-minute spans it was heard in, across midnight too.
func TestSpread(t *testing.T) {
	q := int64(spanMS)
	for _, c := range []struct {
		end, ms int64
		want    string
	}{
		{10 * q, q, fmt.Sprint([]span{{9 * q, q}})},
		{10*q + 60_000, 2 * 60_000, fmt.Sprint([]span{{9 * q, 60_000}, {10 * q, 60_000}})},
		{10 * q, 0, "[]"},
		{10*q + 1, 2*q + 2, fmt.Sprint([]span{{7 * q, 1}, {8 * q, q}, {9 * q, q}, {10 * q, 1}})},
	} {
		got := spread(c.end, c.ms)
		if got == nil {
			got = []span{}
		}
		if fmt.Sprint(got) != c.want {
			t.Fatalf("spread(%d, %d) = %v, want %s", c.end, c.ms, got, c.want)
		}
	}
}

// What reports add to a play is recorded once: a report sent again or arriving late adds nothing,
// and the play is marked counted in the span it reached the threshold (review #93).
func TestReportsAddListeningOnce(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	f := fixture{t, s}
	r := f.song("a", "A", "Album", 1, 1) // a 60 s file: counted at 30 s
	var asset int64
	s.db.QueryRow(`SELECT asset_id FROM album_entries WHERE id = ?`, r.EntryID).Scan(&asset)
	report := func(seq, heard int64) {
		t.Helper()
		if err := s.RecordPlay(ctx, PlayReport{Session: "one", AssetID: asset, ListenedMS: heard, PositionMS: heard, Seq: seq}); err != nil {
			t.Fatal(err)
		}
	}
	report(1, 10_000)
	report(2, 40_000)
	report(2, 40_000) // sent again
	report(1, 10_000) // late
	report(3, 55_000)
	var ms, counted, rows int64
	s.db.QueryRow(`SELECT sum(ms), sum(counted), count(*) FROM listening`).Scan(&ms, &counted, &rows)
	if ms != 55_000 || counted != 1 {
		t.Fatalf("listening %d ms, %d counted (%d rows)", ms, counted, rows)
	}
}

// statsFixture records listening at given times for songs, as reports at those times would.
type statsFixture struct {
	t *testing.T
	s *Store
	n int
}

func (f *statsFixture) play(track, album int64, at time.Time, ms int64, counted bool) {
	f.t.Helper()
	f.n++
	var asset int64
	f.s.db.QueryRow(`SELECT asset_id FROM track_assets WHERE track_id = ?`, track).Scan(&asset)
	var albumArg any
	if album != 0 {
		albumArg = album
	}
	r, err := f.s.db.Exec(`INSERT INTO plays (session, asset_id, track_id, album_id, started_at, updated_at, listened_ms, counted)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, fmt.Sprint("s", f.n), asset, track, albumArg, at.UnixMilli()-ms, at.UnixMilli(), ms, counted)
	if err != nil {
		f.t.Fatal(err)
	}
	id, _ := r.LastInsertId()
	if err := addListening(context.Background(), f.s.db, id, at.UnixMilli(), ms, counted, false); err != nil {
		f.t.Fatal(err)
	}
}

// Days, periods, rankings and trends add up what was heard when, in the time zone asked for; a
// session across midnight counts on both days; a deleted song keeps its days' totals (review #93).
func TestListeningStatistics(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	lib := fixture{t, s}
	taipei, _ := time.LoadLocation("Asia/Taipei")
	a := lib.song("a", "Song A", "Album 1", 1, 1)
	b := lib.song("b", "Song B", "Album 1", 1, 2)
	c := lib.song("c", "Talk C", "Drama", 1, 1)
	s.db.Exec(`UPDATE tracks SET kind = 'spoken' WHERE id = ?`, c.TrackID)
	album1, drama := lib.albumOf(a.EntryID), lib.albumOf(c.EntryID)
	f := &statsFixture{t: t, s: s}
	at := func(day, hour, min int) time.Time { return time.Date(2026, 10, day, hour, min, 0, 0, taipei) }
	f.play(a.TrackID, album1, at(1, 0, 10), 20*60_000, true) // 23:50 on the 30th to 00:10 on the 1st
	f.play(a.TrackID, album1, at(1, 9, 0), 3*60_000, true)
	f.play(b.TrackID, album1, at(1, 9, 30), 4*60_000, false)
	f.play(c.TrackID, drama, at(2, 22, 0), 60*60_000, true)
	f.play(b.TrackID, album1, at(3, 8, 0), 30_000, false) // under a minute: not an active day
	f.play(b.TrackID, album1, at(5, 8, 0), 2*60_000, true)

	month := func(loc *time.Location) (int64, int64) {
		return time.Date(2026, 9, 1, 0, 0, 0, 0, loc).UnixMilli(), time.Date(2026, 11, 1, 0, 0, 0, 0, loc).UnixMilli()
	}
	from, to := month(taipei)
	days, err := s.ListeningDays(ctx, taipei, from, to, "")
	if err != nil {
		t.Fatal(err)
	}
	got := ""
	for _, d := range days {
		got += fmt.Sprintf("%s:%dm/%dp/%dt ", d.Date, d.MS/60_000, d.Plays, d.Tracks)
	}
	if got != "2026-09-30:10m/0p/1t 2026-10-01:17m/2p/2t 2026-10-02:60m/1p/1t 2026-10-03:0m/0p/1t 2026-10-05:2m/1p/1t " {
		t.Fatalf("days in Taipei: %s", got)
	}
	// In UTC (eight hours behind) the same listening falls on other days: the session before midnight
	// in Taipei is all on the 30th, the morning plays of the 3rd and the 5th on the day before.
	uf, ut := month(time.UTC)
	utc, _ := s.ListeningDays(ctx, time.UTC, uf, ut, "")
	got = ""
	for _, d := range utc {
		got += fmt.Sprintf("%s:%ds ", d.Date, d.MS/1000)
	}
	if got != "2026-09-30:1200s 2026-10-01:420s 2026-10-02:3630s 2026-10-04:120s " {
		t.Fatalf("days in UTC: %s", got)
	}
	if music, _ := s.ListeningDays(ctx, taipei, from, to, "music"); len(music) != 4 {
		t.Fatalf("music days: %+v", music)
	}

	sum, _ := s.ListeningSummary(ctx, taipei, from, to, "")
	if sum.MS != 89*60_000+30_000 || sum.Plays != 4 || sum.Tracks != 3 || sum.Albums != 2 || sum.ActiveDays != 4 {
		t.Fatalf("summary %+v", sum)
	}

	top, _ := s.ListeningTop(ctx, from, to, "music", "tracks", "time", 10)
	if len(top) != 2 || top[0].Name != "Song A" || top[0].MS != 23*60_000 || top[0].Plays != 2 || top[0].Track == nil {
		t.Fatalf("top songs by time %+v", top)
	}
	if byPlays, _ := s.ListeningTop(ctx, from, to, "", "tracks", "plays", 10); byPlays[0].Name != "Song A" || byPlays[1].Plays != 1 {
		t.Fatalf("top songs by plays %+v", byPlays)
	}
	if albums, _ := s.ListeningTop(ctx, from, to, "", "albums", "time", 10); len(albums) != 2 || albums[0].ID != drama || albums[1].Tracks != 2 {
		t.Fatalf("top albums %+v", albums)
	}
	if artists, _ := s.ListeningTop(ctx, from, to, "music", "artists", "plays", 10); len(artists) != 2 || artists[0].Name != "xSong A" {
		t.Fatalf("top artists %+v", artists)
	}

	tr, err := s.ListeningTrends(ctx, taipei, at(1, 0, 0).UnixMilli(), at(8, 0, 0).UnixMilli(), "", at(6, 12, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Days) != 7 || tr.Hours[9] != 4*60_000 || tr.Hours[8] != 3*60_000 || tr.Hours[0] != 10*60_000 || tr.Weekdays[3] != 17*60_000 {
		t.Fatalf("trends: %d days, hour 9 %d, hour 0 %d, Thursday %d", len(tr.Days), tr.Hours[9], tr.Hours[0], tr.Weekdays[3])
	}
	// Active days: Sep 30, Oct 1, Oct 2, Oct 5 (the 3rd had 30 s); on the 6th the streak is the 5th.
	if tr.Streak != 1 || tr.Longest != 3 || tr.LongestTo != "2026-10-02" || tr.NewCount != 2 || tr.NewSongs[0].Name != "Talk C" || tr.NewSongs[1].Name != "Song B" {
		t.Fatalf("streaks %d/%d to %s, new %d %+v", tr.Streak, tr.Longest, tr.LongestTo, tr.NewCount, tr.NewSongs)
	}

	// A deleted song keeps its days' totals, shown as removed in rankings.
	if _, err := s.DeleteTrack(ctx, c.TrackID); err != nil {
		t.Fatal(err)
	}
	if after, _ := s.ListeningSummary(ctx, taipei, from, to, ""); after.MS != sum.MS || after.Plays != sum.Plays {
		t.Fatalf("after deleting a song: %+v", after)
	}
	if top, _ := s.ListeningTop(ctx, from, to, "", "tracks", "time", 10); !top[0].Removed || top[0].Track != nil {
		t.Fatalf("a deleted song in the ranking: %+v", top[0])
	}
	day, _ := s.ListeningDay(ctx, taipei, "2026-10-01", "")
	if day.MS != 17*60_000 || len(day.Songs) != 2 || len(day.Albums) != 1 {
		t.Fatalf("day detail %+v", day)
	}
}

// Plays from before listening spans were kept are spread back from their last report, once,
// marked estimated.
func TestBackfillListening(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	f := fixture{t, s}
	r := f.song("a", "A", "Album", 1, 1)
	var asset int64
	s.db.QueryRow(`SELECT asset_id FROM album_entries WHERE id = ?`, r.EntryID).Scan(&asset)
	end := time.Date(2026, 1, 2, 0, 5, 0, 0, time.UTC).UnixMilli()
	s.db.Exec(`INSERT INTO plays (session, asset_id, track_id, started_at, updated_at, listened_ms, counted) VALUES ('old', ?, ?, ?, ?, ?, 1)`,
		asset, r.TrackID, end-10*60_000, end, 10*60_000)
	for range 2 {
		if err := s.BackfillListening(ctx); err != nil {
			t.Fatal(err)
		}
	}
	days, _ := s.ListeningDays(ctx, time.UTC, 0, end+1, "")
	if len(days) != 2 || days[0].MS != 5*60_000 || days[1].MS != 5*60_000 || days[1].Plays != 1 || !days[0].Estimated {
		t.Fatalf("backfilled %+v", days)
	}
	if until, _ := s.EstimatedUntil(ctx); until == 0 {
		t.Fatal("no estimated spans")
	}
	if err := s.ClearListening(ctx); err != nil {
		t.Fatal(err)
	}
	s.BackfillListening(ctx)
	if days, _ := s.ListeningDays(ctx, time.UTC, 0, end+1, ""); len(days) != 0 {
		t.Fatalf("spread again after clearing: %+v", days)
	}
}

// Heard time is bounded by real time (review #102): a report of more than a day is refused before
// any work, a first report hears no more than the file, and later ones no more than the time since
// the playback started; what the play keeps and what the spans add up stay the same.
func TestHeardTimeIsBounded(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	f := fixture{t, s}
	r := f.song("a", "A", "Album", 1, 1) // a 60 s file
	var asset int64
	s.db.QueryRow(`SELECT asset_id FROM album_entries WHERE id = ?`, r.EntryID).Scan(&asset)
	report := func(session string, seq, heard int64) error {
		return s.RecordPlay(ctx, PlayReport{Session: session, AssetID: asset, ListenedMS: heard, PositionMS: 1, Seq: seq})
	}
	totals := func(session string) (heard, spans, rows int64) {
		t.Helper()
		s.db.QueryRow(`SELECT listened_ms FROM plays WHERE session = ?`, session).Scan(&heard)
		s.db.QueryRow(`SELECT coalesce(sum(l.ms), 0), count(*) FROM listening l JOIN plays p ON p.id = l.play_id
			WHERE p.session = ?`, session).Scan(&spans, &rows)
		return
	}
	if err := report("year", 1, 365*24*60*60_000); err != ErrBadPlay {
		t.Fatalf("a year heard: %v", err)
	}
	var n int64
	s.db.QueryRow(`SELECT count(*) FROM plays`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d plays after a refused report", n)
	}
	// Within a day, but more than the file and the time since: kept to what could have been heard.
	if err := report("first", 1, 20*60*60_000); err != nil {
		t.Fatal(err)
	}
	if heard, spans, rows := totals("first"); heard != 60_000+heardSlackMS || spans != heard || rows > 1 {
		t.Fatalf("first report: heard %d, spans %d in %d rows", heard, spans, rows)
	}
	if err := report("first", 2, 20*60*60_000); err != nil {
		t.Fatal(err)
	}
	if heard, spans, _ := totals("first"); heard > 60_000+2*heardSlackMS || spans != heard {
		t.Fatalf("second report: heard %d, spans %d", heard, spans)
	}
	// Hearing it again in the same playback counts, as far as the time since it started allows.
	s.db.Exec(`UPDATE plays SET started_at = started_at - 600000 WHERE session = 'first'`)
	if err := report("first", 3, 7*60_000); err != nil {
		t.Fatal(err)
	}
	if heard, spans, _ := totals("first"); heard != 7*60_000 || spans != heard {
		t.Fatalf("heard again: heard %d, spans %d", heard, spans)
	}
}
