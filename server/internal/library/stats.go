package library

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Personal listening statistics (review #93), from the listening spans: what was heard when, in the
// listener's time zone. Time is what was really heard (no pauses, no skipping ahead); a play counts
// once it reaches the D9 threshold, in the span it did, apart from time. A day is active with a
// minute heard. Plays recorded before spans were kept are spread back from their last report and
// marked estimated.

const activeMS = 60_000

// StatsKind narrows the statistics to music or to drama and talk ("" for all).
func kindCond(kind string) (string, []any) {
	if kind == "music" || kind == "spoken" {
		return ` AND l.kind = ?`, []any{kind}
	}
	return "", nil
}

// rootsOf follows each album of src (album IDs: a query, or a list of placeholders) to the album it
// was merged into, however many merges along (review #109, #152): roots(src, id). A loop or a
// merge into a missing album ends the walk.
func rootsOf(src string) string {
	return `WITH RECURSIVE up(src, cur, depth) AS (
		SELECT id, id, 0 FROM albums WHERE id IN (` + src + `)
		UNION ALL
		SELECT up.src, a.merged_into, up.depth + 1 FROM up JOIN albums a ON a.id = up.cur
			JOIN albums t ON t.id = a.merged_into WHERE up.depth < 32
	), roots(src, id) AS (SELECT src, cur FROM (SELECT src, cur, max(depth) FROM up GROUP BY src))
	`
}

// albumRoots follows the albums listened from in a period (its first two arguments, from and to):
// only those, found through the bucket index, so a short period does not read the whole history
// (review #129).
var albumRoots = rootsOf(`SELECT album_id FROM listening WHERE bucket >= ? AND bucket < ?`)

type spanRow struct {
	bucket, track, album, ms int64
	counted, estimated       bool
}

// spans reads a period's listening; with albums, the album each span was played from too (as it is
// now, after merges), which only the summary needs (review #129).
func (s *Store) spans(ctx context.Context, from, to int64, kind string, albums bool) ([]spanRow, error) {
	q, args := spansQuery(from, to, kind, albums)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []spanRow
	for rows.Next() {
		var r spanRow
		if err := rows.Scan(&r.bucket, &r.track, &r.album, &r.ms, &r.counted, &r.estimated); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func spansQuery(from, to int64, kind string, albums bool) (string, []any) {
	cond, args := kindCond(kind)
	args = append([]any{from, to}, args...)
	if !albums {
		return `SELECT l.bucket, l.track_id, 0, l.ms, l.counted, l.estimated FROM listening l WHERE l.bucket >= ? AND l.bucket < ?` + cond, args
	}
	return albumRoots + `SELECT l.bucket, l.track_id, coalesce(r.id, 0), l.ms, l.counted, l.estimated
		FROM listening l LEFT JOIN roots r ON r.src = l.album_id WHERE l.bucket >= ? AND l.bucket < ?` + cond, append([]any{from, to}, args...)
}

func localDate(ms int64, loc *time.Location) string {
	return time.UnixMilli(ms).In(loc).Format(time.DateOnly)
}

// DayTotal is one day's listening.
type DayTotal struct {
	Date      string `json:"date"` // YYYY-MM-DD in the time zone asked for
	MS        int64  `json:"ms"`
	Plays     int    `json:"plays"`  // plays that reached the threshold that day
	Tracks    int    `json:"tracks"` // different songs heard
	Estimated bool   `json:"estimated,omitempty"`
}

// ListeningDays adds up each day from (inclusive) to to (exclusive), Unix ms; days without
// listening are left out.
func (s *Store) ListeningDays(ctx context.Context, loc *time.Location, from, to int64, kind string) ([]DayTotal, error) {
	rows, err := s.spans(ctx, from, to, kind, false)
	if err != nil {
		return nil, err
	}
	days := map[string]*DayTotal{}
	tracks := map[string]map[int64]bool{}
	for _, r := range rows {
		d := localDate(r.bucket, loc)
		t := days[d]
		if t == nil {
			t = &DayTotal{Date: d}
			days[d], tracks[d] = t, map[int64]bool{}
		}
		t.MS += r.ms
		if r.counted {
			t.Plays++
		}
		if r.ms > 0 {
			tracks[d][r.track] = true
		}
		t.Estimated = t.Estimated || r.estimated
	}
	out := make([]DayTotal, 0, len(days))
	for d, t := range days {
		t.Tracks = len(tracks[d])
		out = append(out, *t)
	}
	slices.SortFunc(out, func(a, b DayTotal) int { return strings.Compare(a.Date, b.Date) })
	return out, nil
}

// Summary is a period's listening.
type Summary struct {
	MS         int64 `json:"ms"`
	Plays      int   `json:"plays"`
	Tracks     int   `json:"tracks"`      // different songs heard
	Albums     int   `json:"albums"`      // different albums they were played from
	ActiveDays int   `json:"active_days"` // days with a minute heard
	Estimated  bool  `json:"estimated,omitempty"`
}

func (s *Store) ListeningSummary(ctx context.Context, loc *time.Location, from, to int64, kind string) (*Summary, error) {
	rows, err := s.spans(ctx, from, to, kind, true)
	if err != nil {
		return nil, err
	}
	out := &Summary{}
	tracks, albums, days := map[int64]bool{}, map[int64]bool{}, map[string]int64{}
	for _, r := range rows {
		out.MS += r.ms
		if r.counted {
			out.Plays++
		}
		if r.ms > 0 {
			tracks[r.track] = true
			if r.album != 0 {
				albums[r.album] = true
			}
		}
		days[localDate(r.bucket, loc)] += r.ms
		out.Estimated = out.Estimated || r.estimated
	}
	out.Tracks, out.Albums = len(tracks), len(albums)
	for _, ms := range days {
		if ms >= activeMS {
			out.ActiveDays++
		}
	}
	return out, nil
}

// TopItem is a song, an artist or an album in a ranking.
type TopItem struct {
	ID      int64      `json:"id,omitempty"` // the song, album or artist (0: an artist named on songs only)
	Name    string     `json:"name"`         // its title, or the artist
	Artist  string     `json:"artist,omitempty"`
	CoverID int64      `json:"cover_id,omitempty"`
	MS      int64      `json:"ms"`
	Plays   int        `json:"plays"`
	Tracks  int        `json:"tracks,omitempty"` // different songs, for an artist or an album
	Track   *TrackItem `json:"track,omitempty"`  // a song still in the library, to play
	Removed bool       `json:"removed,omitempty"`
}

// ListeningTop ranks songs, artists or albums (group) of a period by plays or by time heard (by).
// A song is one song whatever file of it was played; an album is the one it was played from
// (followed to where it was merged, through every merge).
func (s *Store) ListeningTop(ctx context.Context, from, to int64, kind, group, by string, limit int) ([]TopItem, error) {
	cond, args := kindCond(kind)
	order := `ms DESC, plays DESC`
	if by == "plays" {
		order = `plays DESC, ms DESC`
	}
	period := append([]any{from, to}, args...)
	args = append(period, limit)
	var q string
	switch group {
	case "artists":
		// An artist is the one the song is linked to, so the ranking opens its page (review #108); a
		// song with an artist but no link to one ranks under the name, with no page (ID 0).
		args = append(append(append([]any{}, period...), period...), limit)
		q = `SELECT id, name, '', 0, ms, plays, n FROM (
			SELECT ar.id AS id, ar.name AS name, sum(l.ms) AS ms, sum(l.counted) AS plays, count(DISTINCT l.track_id) AS n
				FROM listening l JOIN track_artists ta ON ta.track_id = l.track_id JOIN artists ar ON ar.id = ta.artist_id
				WHERE l.bucket >= ? AND l.bucket < ?` + cond + ` GROUP BY ar.id
			UNION ALL
			SELECT 0, t.artist, sum(l.ms), sum(l.counted), count(DISTINCT l.track_id) FROM listening l JOIN tracks t ON t.id = l.track_id
				WHERE l.bucket >= ? AND l.bucket < ?` + cond + ` AND t.artist != ''
				AND NOT EXISTS (SELECT 1 FROM track_artists ta WHERE ta.track_id = t.id) GROUP BY t.artist
		) ORDER BY ` + order + `, name LIMIT ?`
	case "albums":
		args = append([]any{from, to}, args...) // the period's albums, for albumRoots
		q = albumRoots + `SELECT al.id, al.title, al.album_artist, coalesce(al.cover_id, 0), sum(l.ms) AS ms, sum(l.counted) AS plays,
			count(DISTINCT l.track_id) FROM listening l
			JOIN roots r ON r.src = l.album_id JOIN albums al ON al.id = r.id
			WHERE l.bucket >= ? AND l.bucket < ?` + cond + ` GROUP BY al.id ORDER BY ` + order + ` LIMIT ?`
	default:
		q = `SELECT l.track_id, coalesce(t.title, ''), coalesce(t.artist, ''), 0, sum(l.ms) AS ms, sum(l.counted) AS plays, 0
			FROM listening l LEFT JOIN tracks t ON t.id = l.track_id WHERE l.bucket >= ? AND l.bucket < ?` + cond + `
			GROUP BY l.track_id ORDER BY ` + order + ` LIMIT ?`
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out := []TopItem{}
	for rows.Next() {
		var it TopItem
		if err := rows.Scan(&it.ID, &it.Name, &it.Artist, &it.CoverID, &it.MS, &it.Plays, &it.Tracks); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if group == "artists" || group == "albums" {
		return out, nil
	}
	return out, s.fillTracks(ctx, out)
}

// fillTracks adds the playable song to ranked songs; a song gone from the library is marked removed.
func (s *Store) fillTracks(ctx context.Context, items []TopItem) error {
	for i := range items {
		list, err := scanTracks(s.db.QueryContext(ctx, trackSQL+` WHERE t.id = ?`, items[i].ID))
		if err != nil {
			return err
		}
		if len(list) == 0 {
			items[i].Removed = items[i].Name == ""
			continue
		}
		items[i].Track = &list[0]
		items[i].CoverID = list[0].CoverID
	}
	return nil
}

// DayDetail is what one day's listening was.
type DayDetail struct {
	DayTotal
	Songs  []TopItem `json:"songs"`
	Albums []TopItem `json:"albums"`
}

func (s *Store) ListeningDay(ctx context.Context, loc *time.Location, date, kind string) (*DayDetail, error) {
	day, err := time.ParseInLocation(time.DateOnly, date, loc)
	if err != nil {
		return nil, invalid("date is YYYY-MM-DD")
	}
	from, to := day.UnixMilli(), day.AddDate(0, 0, 1).UnixMilli()
	out := &DayDetail{DayTotal: DayTotal{Date: date}}
	days, err := s.ListeningDays(ctx, loc, from, to, kind)
	if err != nil {
		return nil, err
	}
	if len(days) > 0 {
		out.DayTotal = days[0]
	}
	if out.Songs, err = s.ListeningTop(ctx, from, to, kind, "tracks", "time", 200); err != nil {
		return nil, err
	}
	if out.Albums, err = s.ListeningTop(ctx, from, to, kind, "albums", "time", 50); err != nil {
		return nil, err
	}
	return out, nil
}

// Trends are a period's shape: each day (also those without listening), the hours of the day and the
// days of the week it was heard in, the songs first heard in it, and the streaks of active days.
type Trends struct {
	Days      []DayTotal `json:"days"`
	Hours     [24]int64  `json:"hours"`    // ms heard in each hour of the day
	Weekdays  [7]int64   `json:"weekdays"` // Monday first
	NewSongs  []TopItem  `json:"new_songs"`
	NewCount  int        `json:"new_count"`
	Streak    int        `json:"streak"`  // active days in a row up to today (or yesterday)
	Longest   int        `json:"longest"` // the most ever
	LongestTo string     `json:"longest_to,omitempty"`
}

func (s *Store) ListeningTrends(ctx context.Context, loc *time.Location, from, to int64, kind string, now time.Time) (*Trends, error) {
	rows, err := s.spans(ctx, from, to, kind, false)
	if err != nil {
		return nil, err
	}
	out := &Trends{Days: []DayTotal{}, NewSongs: []TopItem{}}
	for _, r := range rows {
		t := time.UnixMilli(r.bucket).In(loc)
		out.Hours[t.Hour()] += r.ms
		out.Weekdays[(int(t.Weekday())+6)%7] += r.ms
	}
	days, err := s.ListeningDays(ctx, loc, from, to, kind)
	if err != nil {
		return nil, err
	}
	have := map[string]DayTotal{}
	for _, d := range days {
		have[d.Date] = d
	}
	end := time.UnixMilli(to).In(loc)
	for d := time.UnixMilli(from).In(loc); d.Before(end); d = d.AddDate(0, 0, 1) {
		k := d.Format(time.DateOnly)
		t, ok := have[k]
		if !ok {
			t = DayTotal{Date: k}
		}
		out.Days = append(out.Days, t)
	}
	// Songs whose first listening ever falls in the period.
	cond, args := kindCond(kind)
	first := `SELECT l.track_id, min(l.bucket) AS m FROM listening l WHERE 1 = 1` + cond + ` GROUP BY l.track_id HAVING m >= ? AND m < ?`
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM (`+first+`)`, append(args, from, to)...).Scan(&out.NewCount); err != nil {
		return nil, err
	}
	ids, err := idsTx(ctx, s.db, `SELECT track_id FROM (`+first+`) ORDER BY m DESC LIMIT 20`, append(args, from, to)...)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		out.NewSongs = append(out.NewSongs, TopItem{ID: id})
	}
	if err := s.fillTracks(ctx, out.NewSongs); err != nil {
		return nil, err
	}
	for i := range out.NewSongs {
		if t := out.NewSongs[i].Track; t != nil {
			out.NewSongs[i].Name, out.NewSongs[i].Artist = t.Title, t.Artist
		}
	}
	out.Streak, out.Longest, out.LongestTo, err = s.streaks(ctx, loc, kind, now)
	return out, err
}

// streaks are the active days in a row up to today (counting from yesterday while today has not
// begun) and the longest run ever, with its last day.
func (s *Store) streaks(ctx context.Context, loc *time.Location, kind string, now time.Time) (current, longest int, longestTo string, err error) {
	all, err := s.ListeningDays(ctx, loc, 0, now.UnixMilli()+1, kind)
	if err != nil {
		return 0, 0, "", err
	}
	active := map[string]bool{}
	var dates []string
	for _, d := range all {
		if d.MS >= activeMS {
			active[d.Date] = true
			dates = append(dates, d.Date)
		}
	}
	run, prev := 0, time.Time{}
	for _, k := range dates {
		d, _ := time.ParseInLocation(time.DateOnly, k, loc)
		if !prev.IsZero() && prev.AddDate(0, 0, 1).Format(time.DateOnly) == k {
			run++
		} else {
			run = 1
		}
		if run > longest {
			longest, longestTo = run, k
		}
		prev = d
	}
	day := now.In(loc)
	if !active[day.Format(time.DateOnly)] {
		day = day.AddDate(0, 0, -1)
	}
	for active[day.Format(time.DateOnly)] {
		current++
		day = day.AddDate(0, 0, -1)
	}
	return current, longest, longestTo, nil
}

// ListenedRow is one span of listening, for export.
type ListenedRow struct {
	Start     string `json:"start"` // the span's start, in the time zone asked for (RFC 3339)
	TrackID   int64  `json:"track_id"`
	Title     string `json:"title"`
	Artist    string `json:"artist"`
	Album     string `json:"album"`
	Kind      string `json:"kind"`
	MS        int64  `json:"ms"`
	Counted   bool   `json:"counted"`
	Estimated bool   `json:"estimated"`
}

// ExportListening walks every span of listening, oldest first.
func (s *Store) ExportListening(ctx context.Context, loc *time.Location, each func(ListenedRow) error) error {
	rows, err := s.db.QueryContext(ctx, `SELECT l.bucket, l.track_id, coalesce(t.title, ''), coalesce(t.artist, ''), coalesce(a.title, ''),
		l.kind, l.ms, l.counted, l.estimated FROM listening l LEFT JOIN tracks t ON t.id = l.track_id LEFT JOIN albums a ON a.id = l.album_id
		ORDER BY l.bucket, l.id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r ListenedRow
		var bucket int64
		if err := rows.Scan(&bucket, &r.TrackID, &r.Title, &r.Artist, &r.Album, &r.Kind, &r.MS, &r.Counted, &r.Estimated); err != nil {
			return err
		}
		r.Start = time.UnixMilli(bucket).In(loc).Format(time.RFC3339)
		if err := each(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ClearListening forgets the listening statistics; plays themselves (resume points, the history)
// stay, and are not spread again.
func (s *Store) ClearListening(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM listening`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('listening_backfill', 'done')
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`); err != nil {
		return err
	}
	return tx.Commit()
}

// EstimatedUntil is when the latest estimated span is, so the page can say up to when the
// statistics are worked out from older plays (0: none are).
func (s *Store) EstimatedUntil(ctx context.Context) (int64, error) {
	var v sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT max(bucket) FROM listening WHERE estimated = 1`).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("estimated spans: %w", err)
	}
	return v.Int64, nil
}
