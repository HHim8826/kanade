package library

import (
	"context"
	"database/sql"
	"errors"
	"sort"
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

// listed keeps albums that were merged away or emptied out of lists; they still exist, so undo
// can fill them again and their page can point to where the songs went.
const listed = `HAVING count(e.id) > 0`

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
	return s.AlbumsBy(ctx, AlbumQuery{Limit: limit, Offset: offset, Recent: recent})
}

// AlbumQuery picks albums of the lists: all, a category's or those in none (review #92), whose title
// or album artist contains Search.
type AlbumQuery struct {
	Limit, Offset int
	Recent        bool  // the latest made first, else by album artist and title
	Category      int64 // > 0: in this category; -1: in no category
	Search        string
}

func (s *Store) AlbumsBy(ctx context.Context, q AlbumQuery) ([]AlbumSummary, error) {
	order := `al.album_artist, al.title, al.id`
	if q.Recent {
		order = `al.id DESC`
	}
	var where []string
	var args []any
	switch {
	case q.Category > 0:
		where = append(where, `al.id IN (SELECT album_id FROM album_categories WHERE category_id = ?)`)
		args = append(args, q.Category)
	case q.Category < 0:
		where = append(where, `al.id NOT IN (SELECT album_id FROM album_categories)`)
	}
	if t := strings.TrimSpace(q.Search); t != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(t) + "%"
		where = append(where, `(al.title LIKE ? ESCAPE '\' OR al.album_artist LIKE ? ESCAPE '\')`)
		args = append(args, like, like)
	}
	cond := ""
	if len(where) > 0 {
		cond = ` WHERE ` + strings.Join(where, " AND ")
	}
	args = append(args, q.Limit, q.Offset)
	return scanAlbums(s.db.QueryContext(ctx, albumSummarySQL+cond+` GROUP BY al.id `+listed+` ORDER BY `+order+` LIMIT ? OFFSET ?`, args...))
}

type Entry struct {
	EntryID int64      `json:"entry_id"`
	DiscNo  int        `json:"disc_no"`
	TrackNo int        `json:"track_no"`
	TrackID int64      `json:"track_id"`
	Title   string     `json:"title"`
	Artist  string     `json:"artist"`
	Version string     `json:"version,omitempty"`
	Kind    string     `json:"kind"`
	Asset   AssetBrief `json:"asset"`
}

type AlbumDetail struct {
	AlbumSummary
	Catalog    string    `json:"catalog"`
	Edition    string    `json:"edition"`
	MBRelease  string    `json:"mb_release,omitempty"`
	MergedInto int64     `json:"merged_into,omitempty"` // emptied by a merge into this album
	Original   bool      `json:"original"`              // made by an import, so its tags can be restored
	Aliases    []string  `json:"aliases"`
	Sidecars   []Sidecar `json:"sidecars"` // CUE sheets and rip logs kept with the album
	Entries    []Entry   `json:"entries"`
	// Sections are the names of its discs that have one (review #82).
	Sections map[int]string `json:"sections"`
	// Categories are the user's folders it is in (review #92).
	Categories []CategoryBrief `json:"categories"`
}

func (s *Store) Album(ctx context.Context, id int64) (*AlbumDetail, error) {
	list, err := scanAlbums(s.db.QueryContext(ctx, albumSummarySQL+` WHERE al.id = ? GROUP BY al.id`, id))
	if err != nil || len(list) == 0 {
		return nil, err
	}
	d := &AlbumDetail{AlbumSummary: list[0], Entries: []Entry{}}
	var merged sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT catalog, edition, mb_release, merged_into, origin IS NOT NULL FROM albums WHERE id = ?`, id).
		Scan(&d.Catalog, &d.Edition, &d.MBRelease, &merged, &d.Original); err != nil {
		return nil, err
	}
	d.MergedInto = merged.Int64
	if d.Aliases, err = aliasNames(ctx, s.db, "album", id); err != nil {
		return nil, err
	}
	if d.Sidecars, err = s.albumSidecars(ctx, id); err != nil {
		return nil, err
	}
	if d.Categories, err = s.AlbumCategories(ctx, id); err != nil {
		return nil, err
	}
	if d.Sections, err = sectionNames(ctx, s.db, id); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.id, e.disc_no, e.track_no, t.id, t.title, t.artist, t.version, t.kind, `+briefCols+`
		FROM album_entries e JOIN tracks t ON t.id = e.track_id JOIN assets a ON a.id = e.asset_id
		WHERE e.album_id = ? AND a.state = 'verified' ORDER BY e.disc_no, e.track_no, t.title`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e Entry
		if err := rows.Scan(append([]any{&e.EntryID, &e.DiscNo, &e.TrackNo, &e.TrackID, &e.Title, &e.Artist, &e.Version, &e.Kind}, e.Asset.dest()...)...); err != nil {
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
	Kind    string     `json:"kind"`  // music | spoken
	Asset   AssetBrief `json:"asset"` // the track's first verified file
}

// trackSQL picks one verified asset per track and the first album it appears on.
const trackSQL = `SELECT t.id, t.title, t.artist,
	coalesce(fa.title, ''), coalesce(fa.id, 0), coalesce(fa.cover_id, 0), t.kind,
	` + briefCols + `
	FROM tracks t
	JOIN assets a ON a.id = (SELECT ta.asset_id FROM track_assets ta JOIN assets x ON x.id = ta.asset_id
		WHERE ta.track_id = t.id AND x.state = 'verified' ORDER BY ta.asset_id LIMIT 1)
	LEFT JOIN albums fa ON fa.id = (SELECT e.album_id FROM album_entries e WHERE e.track_id = t.id ORDER BY e.id LIMIT 1)`

// trackDetailSQL is trackSQL that also takes a file missing from Drive when there is no other, so
// such a track can still be edited or deleted.
var trackDetailSQL = strings.Replace(trackSQL, `x.state = 'verified' ORDER BY ta.asset_id`,
	`x.state IN ('verified', 'missing') ORDER BY x.state = 'verified' DESC, ta.asset_id`, 1)

func scanTracks(rows *sql.Rows, err error) ([]TrackItem, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TrackItem{}
	for rows.Next() {
		var t TrackItem
		if err := rows.Scan(append([]any{&t.ID, &t.Title, &t.Artist, &t.Album, &t.AlbumID, &t.CoverID, &t.Kind}, t.Asset.dest()...)...); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Track filters: the songs the home page asks to sort out.
const (
	NoAlbum  = "no_album"  // on no album
	NoArtist = "no_artist" // no artist
)

var trackFilters = map[string]string{
	"":       "",
	NoAlbum:  ` WHERE NOT EXISTS (SELECT 1 FROM album_entries e WHERE e.track_id = t.id)`,
	NoArtist: ` WHERE t.artist = ''`,
}

// Tracks lists songs by title, all of them or those a filter keeps.
func (s *Store) Tracks(ctx context.Context, limit, offset int, filter string) ([]TrackItem, error) {
	where, ok := trackFilters[filter]
	if !ok {
		return nil, invalid("unknown filter %q", filter)
	}
	return scanTracks(s.db.QueryContext(ctx, trackSQL+where+` ORDER BY t.title, t.id LIMIT ? OFFSET ?`, limit, offset)) // a total order: pages neither repeat nor skip
}

type Artist struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Tracks int    `json:"tracks"`
}

func (s *Store) Artists(ctx context.Context, limit, offset int) ([]Artist, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ar.id, ar.name, count(ta.track_id) FROM artists ar
		JOIN track_artists ta ON ta.artist_id = ar.id GROUP BY ar.id ORDER BY ar.name, ar.id LIMIT ? OFFSET ?`, limit, offset)
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

type ArtistDetail struct {
	Artist
	Aliases []string    `json:"aliases"`
	Items   []TrackItem `json:"items"`
}

// ArtistDetail is an artist with its other names and songs; nil when there is no such artist.
func (s *Store) ArtistDetail(ctx context.Context, id int64) (*ArtistDetail, error) {
	d := &ArtistDetail{}
	err := s.db.QueryRowContext(ctx, `SELECT id, name FROM artists WHERE id = ?`, id).Scan(&d.ID, &d.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if d.Aliases, err = aliasNames(ctx, s.db, "artist", id); err != nil {
		return nil, err
	}
	d.Items, err = scanTracks(s.db.QueryContext(ctx, trackSQL+` JOIN track_artists ta2 ON ta2.track_id = t.id
		WHERE ta2.artist_id = ? ORDER BY t.title`, id))
	d.Tracks = len(d.Items)
	return d, err
}

// ArtistByName is the ID of the artist with exactly this name, or 0.
func (s *Store) ArtistByName(ctx context.Context, name string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM artists WHERE name = ?`, strings.TrimSpace(name)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

type TrackEntry struct {
	EntryID int64  `json:"entry_id"`
	AlbumID int64  `json:"album_id"`
	Album   string `json:"album"`
	DiscNo  int    `json:"disc_no"`
	TrackNo int    `json:"track_no"`
}

type TrackDetail struct {
	TrackItem
	Version     string       `json:"version"`
	MBRecording string       `json:"mb_recording,omitempty"`
	Aliases     []string     `json:"aliases"`
	Entries     []TrackEntry `json:"entries"`           // every album the track is on
	Missing     bool         `json:"missing,omitempty"` // its file is gone from Drive
}

// Track is one track with what its edit dialog shows; nil when there is no such track.
func (s *Store) Track(ctx context.Context, id int64) (*TrackDetail, error) {
	items, err := scanTracks(s.db.QueryContext(ctx, trackDetailSQL+` WHERE t.id = ?`, id))
	if err != nil || len(items) == 0 {
		return nil, err
	}
	d := &TrackDetail{TrackItem: items[0], Entries: []TrackEntry{}}
	if err := s.db.QueryRowContext(ctx, `SELECT t.version, t.mb_recording, a.state = ? FROM tracks t, assets a WHERE t.id = ? AND a.id = ?`,
		AssetMissing, id, d.Asset.ID).Scan(&d.Version, &d.MBRecording, &d.Missing); err != nil {
		return nil, err
	}
	if d.Aliases, err = aliasNames(ctx, s.db, "track", id); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.id, al.id, al.title, e.disc_no, e.track_no FROM album_entries e
		JOIN albums al ON al.id = e.album_id WHERE e.track_id = ? ORDER BY e.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e TrackEntry
		if err := rows.Scan(&e.EntryID, &e.AlbumID, &e.Album, &e.DiscNo, &e.TrackNo); err != nil {
			return nil, err
		}
		d.Entries = append(d.Entries, e)
	}
	return d, rows.Err()
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
	q, raw := Normalize(q), q
	if q == "" { // only symbols, like "%" or "△": match them as written
		if q = fold(raw, false); strings.TrimSpace(q) == "" {
			return res, nil
		}
	}
	// The trigram index serves LIKE and GLOB, but not LIKE with ESCAPE (review #163): a pattern
	// escapes only when the words have the characters it would have to.
	match, pattern := `text LIKE ?`, "%"+q+"%"
	switch {
	case !strings.ContainsAny(q, `%_\`):
	case !strings.ContainsAny(q, `*?[]`):
		match, pattern = `text GLOB ?`, "*"+q+"*" // the text is folded to lower case, as q is
	default:
		match, pattern = `text LIKE ? ESCAPE '\'`, "%"+strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)+"%"
	}
	// Each kind its own limit: common words do not leave albums and artists out behind songs.
	ids := map[string][]any{}
	for _, kind := range []string{"track", "album", "artist"} {
		rows, err := s.db.QueryContext(ctx, `SELECT ref_id FROM search_index WHERE `+match+` AND kind = ? LIMIT ?`, pattern, kind, limit)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			ids[kind] = append(ids[kind], id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	var err error
	in := func(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }
	if v := ids["track"]; len(v) > 0 {
		if res.Tracks, err = scanTracks(s.db.QueryContext(ctx, trackSQL+` WHERE t.id IN (`+in(len(v))+`) ORDER BY t.title`, v...)); err != nil {
			return nil, err
		}
	}
	if v := ids["album"]; len(v) > 0 {
		if res.Albums, err = scanAlbums(s.db.QueryContext(ctx, albumSummarySQL+` WHERE al.id IN (`+in(len(v))+`) GROUP BY al.id `+listed+` ORDER BY al.title`, v...)); err != nil {
			return nil, err
		}
	}
	if v := ids["artist"]; len(v) > 0 {
		rows, err := s.db.QueryContext(ctx, `SELECT ar.id, ar.name, count(ta.track_id) FROM artists ar
			JOIN track_artists ta ON ta.artist_id = ar.id WHERE ar.id IN (`+in(len(v))+`) GROUP BY ar.id ORDER BY ar.name`, v...)
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

// AlbumByTags is the album that an import with this album and album artist would join (the one
// first made from those tags, followed through merges), or nil.
func (s *Store) AlbumByTags(ctx context.Context, album, albumArtist string) (*AlbumSummary, error) {
	var id int64
	var merged sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT id, merged_into FROM albums WHERE origin = ? ORDER BY id LIMIT 1`,
		albumOrigin(album, albumArtist)).Scan(&id, &merged)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for hops := 0; merged.Valid && hops < 10; hops++ {
		id = merged.Int64
		if err := s.db.QueryRowContext(ctx, `SELECT merged_into FROM albums WHERE id = ?`, id).Scan(&merged); err != nil {
			return nil, err
		}
	}
	list, err := scanAlbums(s.db.QueryContext(ctx, albumSummarySQL+` WHERE al.id = ? GROUP BY al.id`, id))
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

// TrackBySameAudio names a library track whose file decodes to the same audio (decision D2 §6: the
// same recording with other tags), or "". All-zero MD5s mean "not computed" and match nothing.
func (s *Store) TrackBySameAudio(ctx context.Context, audioMD5 string) (string, error) {
	if audioMD5 == "" || strings.Trim(audioMD5, "0") == "" {
		return "", nil
	}
	var title string
	err := s.db.QueryRowContext(ctx, `SELECT t.title FROM assets a JOIN track_assets ta ON ta.asset_id = a.id
		JOIN tracks t ON t.id = ta.track_id WHERE a.audio_md5 = ? LIMIT 1`, audioMD5).Scan(&title)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return title, err
}

// RandomTracks picks up to n songs at random from the whole library (reviews #72, #73): each song
// as likely as any other, whatever album or how many versions it has; only songs with a playable
// file; of kind (music or spoken, "" both); leaving out not (the songs just played) unless that
// leaves nothing to play.
func (s *Store) RandomTracks(ctx context.Context, n int, kind string, not []int64) ([]TrackItem, error) {
	// pick picks songs at random, leaving out those of not, or only among those of only.
	pick := func(not, only []int64) ([]any, error) {
		args := []any{kind, kind}
		q := `SELECT t.id FROM tracks t WHERE (? = '' OR t.kind = ?) AND EXISTS (SELECT 1 FROM track_assets ta
			JOIN assets a ON a.id = ta.asset_id WHERE ta.track_id = t.id AND a.state = 'verified')`
		for _, set := range []struct {
			op  string
			ids []int64
		}{{"NOT IN", not}, {"IN", only}} {
			if len(set.ids) > 0 {
				q += ` AND t.id ` + set.op + ` (` + strings.Repeat("?, ", len(set.ids)-1) + `?)`
				for _, id := range set.ids {
					args = append(args, id)
				}
			}
		}
		rows, err := s.db.QueryContext(ctx, q+` ORDER BY random() LIMIT ?`, append(args, n+len(only))...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var ids []any
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		return ids, rows.Err()
	}
	ids, err := pick(not, nil)
	if err == nil && len(ids) == 0 && len(not) > 0 {
		// A small library: round again (review #105).
		var left []any
		if left, err = pick(nil, not); err == nil {
			ids = nil
			for _, id := range roundAgain(int64s(left), not) {
				ids = append(ids, id)
			}
			ids = ids[:min(len(ids), n)]
		}
	}
	if err != nil || len(ids) == 0 {
		return []TrackItem{}, err
	}
	list, err := scanTracks(s.db.QueryContext(ctx, trackSQL+` WHERE t.id IN (`+strings.Repeat("?, ", len(ids)-1)+`?)`, ids...))
	if err != nil {
		return nil, err
	}
	order := map[int64]int{}
	for i, id := range ids {
		order[id.(int64)] = i
	}
	sort.Slice(list, func(i, j int) bool { return order[list[i].ID] < order[list[j].ID] })
	return list, nil
}
