package library

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

type AssetBrief struct {
	ID         int64  `json:"id"`
	Format     string `json:"format"`
	Codec      string `json:"codec"`
	SampleRate int    `json:"sample_rate"`
	BitDepth   int    `json:"bit_depth"`
	Channels   int    `json:"channels"`
	DurationMS int64  `json:"duration_ms"`
	Bitrate    int    `json:"bitrate"`
	Size       int64  `json:"size"`
}

const briefCols = `a.id, a.format, a.codec, a.sample_rate, a.bit_depth, a.channels, a.duration_ms, a.bitrate, a.size`

func (b *AssetBrief) dest() []any {
	return []any{&b.ID, &b.Format, &b.Codec, &b.SampleRate, &b.BitDepth, &b.Channels, &b.DurationMS, &b.Bitrate, &b.Size}
}

type AlbumSummary struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	AlbumArtist string `json:"album_artist"`
	Date        string `json:"date"`
	CoverID     int64  `json:"cover_id,omitempty"`
	Tracks      int    `json:"tracks"`
	DurationMS  int64  `json:"duration_ms"`
}

const albumSummarySQL = `SELECT al.id, al.title, al.album_artist, al.date, coalesce(al.cover_id, 0),
	count(e.id), coalesce(sum(a.duration_ms), 0)
	FROM albums al
	LEFT JOIN album_entries e ON e.album_id = al.id
	LEFT JOIN assets a ON a.id = e.asset_id AND a.state = 'verified'`

func scanAlbums(rows *sql.Rows, err error) ([]AlbumSummary, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AlbumSummary{}
	for rows.Next() {
		var a AlbumSummary
		if err := rows.Scan(&a.ID, &a.Title, &a.AlbumArtist, &a.Date, &a.CoverID, &a.Tracks, &a.DurationMS); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Albums lists albums by album artist and title, or newest first when recent is set.
func (s *Store) Albums(ctx context.Context, limit, offset int, recent bool) ([]AlbumSummary, error) {
	order := `al.album_artist, al.title`
	if recent {
		order = `al.id DESC`
	}
	return scanAlbums(s.db.QueryContext(ctx, albumSummarySQL+` GROUP BY al.id ORDER BY `+order+` LIMIT ? OFFSET ?`, limit, offset))
}

type Entry struct {
	EntryID int64      `json:"entry_id"`
	DiscNo  int        `json:"disc_no"`
	TrackNo int        `json:"track_no"`
	TrackID int64      `json:"track_id"`
	Title   string     `json:"title"`
	Artist  string     `json:"artist"`
	Asset   AssetBrief `json:"asset"`
}

type AlbumDetail struct {
	AlbumSummary
	Entries []Entry `json:"entries"`
}

func (s *Store) Album(ctx context.Context, id int64) (*AlbumDetail, error) {
	list, err := scanAlbums(s.db.QueryContext(ctx, albumSummarySQL+` WHERE al.id = ? GROUP BY al.id`, id))
	if err != nil || len(list) == 0 {
		return nil, err
	}
	d := &AlbumDetail{AlbumSummary: list[0], Entries: []Entry{}}
	rows, err := s.db.QueryContext(ctx, `SELECT e.id, e.disc_no, e.track_no, t.id, t.title, t.artist, `+briefCols+`
		FROM album_entries e JOIN tracks t ON t.id = e.track_id JOIN assets a ON a.id = e.asset_id
		WHERE e.album_id = ? AND a.state = 'verified' ORDER BY e.disc_no, e.track_no, t.title`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e Entry
		if err := rows.Scan(append([]any{&e.EntryID, &e.DiscNo, &e.TrackNo, &e.TrackID, &e.Title, &e.Artist}, e.Asset.dest()...)...); err != nil {
			return nil, err
		}
		d.Entries = append(d.Entries, e)
	}
	return d, rows.Err()
}

type TrackItem struct {
	ID      int64      `json:"id"`
	Title   string     `json:"title"`
	Artist  string     `json:"artist"`
	Album   string     `json:"album,omitempty"`
	AlbumID int64      `json:"album_id,omitempty"`
	CoverID int64      `json:"cover_id,omitempty"`
	Asset   AssetBrief `json:"asset"` // the track's first verified file
}

// trackSQL picks one verified asset per track and the first album it appears on.
const trackSQL = `SELECT t.id, t.title, t.artist,
	coalesce(fa.title, ''), coalesce(fa.id, 0), coalesce(fa.cover_id, 0),
	` + briefCols + `
	FROM tracks t
	JOIN assets a ON a.id = (SELECT ta.asset_id FROM track_assets ta JOIN assets x ON x.id = ta.asset_id
		WHERE ta.track_id = t.id AND x.state = 'verified' ORDER BY ta.asset_id LIMIT 1)
	LEFT JOIN albums fa ON fa.id = (SELECT e.album_id FROM album_entries e WHERE e.track_id = t.id ORDER BY e.id LIMIT 1)`

func scanTracks(rows *sql.Rows, err error) ([]TrackItem, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TrackItem{}
	for rows.Next() {
		var t TrackItem
		if err := rows.Scan(append([]any{&t.ID, &t.Title, &t.Artist, &t.Album, &t.AlbumID, &t.CoverID}, t.Asset.dest()...)...); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) Tracks(ctx context.Context, limit, offset int) ([]TrackItem, error) {
	return scanTracks(s.db.QueryContext(ctx, trackSQL+` ORDER BY t.title LIMIT ? OFFSET ?`, limit, offset))
}

type Artist struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Tracks int    `json:"tracks"`
}

func (s *Store) Artists(ctx context.Context, limit, offset int) ([]Artist, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ar.id, ar.name, count(ta.track_id) FROM artists ar
		LEFT JOIN track_artists ta ON ta.artist_id = ar.id GROUP BY ar.id ORDER BY ar.name LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Artist{}
	for rows.Next() {
		var a Artist
		if err := rows.Scan(&a.ID, &a.Name, &a.Tracks); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) ArtistTracks(ctx context.Context, artistID int64) ([]TrackItem, error) {
	return scanTracks(s.db.QueryContext(ctx, trackSQL+` JOIN track_artists ta2 ON ta2.track_id = t.id
		WHERE ta2.artist_id = ? ORDER BY t.title`, artistID))
}

type SearchResult struct {
	Tracks  []TrackItem    `json:"tracks"`
	Albums  []AlbumSummary `json:"albums"`
	Artists []Artist       `json:"artists"`
}

// Search matches a substring of titles and names. The trigram index serves queries of three
// or more characters; shorter ones (common in Japanese) scan the index table, which is small.
func (s *Store) Search(ctx context.Context, q string, limit int) (*SearchResult, error) {
	res := &SearchResult{Tracks: []TrackItem{}, Albums: []AlbumSummary{}, Artists: []Artist{}}
	q = Normalize(q)
	if q == "" {
		return res, nil
	}
	pattern := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
	ids := map[string][]any{}
	rows, err := s.db.QueryContext(ctx, `SELECT kind, ref_id FROM search_index WHERE text LIKE ? ESCAPE '\' LIMIT 500`, pattern)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var kind string
		var id int64
		if err := rows.Scan(&kind, &id); err != nil {
			rows.Close()
			return nil, err
		}
		if len(ids[kind]) < limit {
			ids[kind] = append(ids[kind], id)
		}
	}
	rows.Close()
	in := func(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }
	if v := ids["track"]; len(v) > 0 {
		if res.Tracks, err = scanTracks(s.db.QueryContext(ctx, trackSQL+` WHERE t.id IN (`+in(len(v))+`) ORDER BY t.title`, v...)); err != nil {
			return nil, err
		}
	}
	if v := ids["album"]; len(v) > 0 {
		if res.Albums, err = scanAlbums(s.db.QueryContext(ctx, albumSummarySQL+` WHERE al.id IN (`+in(len(v))+`) GROUP BY al.id ORDER BY al.title`, v...)); err != nil {
			return nil, err
		}
	}
	if v := ids["artist"]; len(v) > 0 {
		rows, err := s.db.QueryContext(ctx, `SELECT ar.id, ar.name, count(ta.track_id) FROM artists ar
			LEFT JOIN track_artists ta ON ta.artist_id = ar.id WHERE ar.id IN (`+in(len(v))+`) GROUP BY ar.id ORDER BY ar.name`, v...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var a Artist
			if err := rows.Scan(&a.ID, &a.Name, &a.Tracks); err != nil {
				return nil, err
			}
			res.Artists = append(res.Artists, a)
		}
	}
	return res, nil
}

// StreamTarget resolves an asset for playback.
func (s *Store) StreamTarget(ctx context.Context, assetID int64) (*Asset, error) {
	a, err := s.Asset(ctx, assetID)
	if err != nil || a == nil {
		return a, err
	}
	if a.State != AssetVerified || a.DriveFileID == "" {
		return nil, errors.New("asset is not available yet")
	}
	return a, nil
}
