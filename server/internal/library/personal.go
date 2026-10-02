package library

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/HHim8826/kanade/server/internal/db"
)

// Favorites, playlists, lyrics and history (P2-1, docs/p2-design.md).

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid request")
)

// ---- favorites ----

type FavoriteIDs struct {
	Tracks []int64 `json:"tracks"`
	Albums []int64 `json:"albums"`
}

func (s *Store) FavoriteIDs(ctx context.Context) (FavoriteIDs, error) {
	ids := FavoriteIDs{Tracks: []int64{}, Albums: []int64{}}
	var err error
	if ids.Tracks, err = s.ids(ctx, `SELECT track_id FROM favorite_tracks ORDER BY created_at DESC`); err != nil {
		return ids, err
	}
	ids.Albums, err = s.ids(ctx, `SELECT album_id FROM favorite_albums ORDER BY created_at DESC`)
	return ids, err
}

func (s *Store) ids(ctx context.Context, query string, args ...any) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetFavorite marks or unmarks a track ("track") or an album ("album").
func (s *Store) SetFavorite(ctx context.Context, kind string, id int64, on bool) error {
	table, col, ref := "favorite_tracks", "track_id", "tracks"
	switch kind {
	case "track":
	case "album":
		table, col, ref = "favorite_albums", "album_id", "albums"
	default:
		return ErrInvalid
	}
	if !on {
		_, err := s.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+col+` = ?`, id)
		return err
	}
	r, err := s.db.ExecContext(ctx, `INSERT INTO `+table+` (`+col+`, created_at) SELECT id, ? FROM `+ref+` WHERE id = ?
		ON CONFLICT DO NOTHING`, db.Now(), id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		var ok int
		if s.db.QueryRowContext(ctx, `SELECT 1 FROM `+ref+` WHERE id = ?`, id).Scan(&ok) != nil {
			return ErrNotFound
		}
	}
	return nil
}

type Favorites struct {
	Tracks []TrackItem    `json:"tracks"`
	Albums []AlbumSummary `json:"albums"`
}

// Favorites lists favorite tracks and albums, most recently added first.
func (s *Store) Favorites(ctx context.Context) (*Favorites, error) {
	f := &Favorites{}
	var err error
	if f.Tracks, err = scanTracks(s.db.QueryContext(ctx, trackSQL+` JOIN favorite_tracks f ON f.track_id = t.id
		ORDER BY f.created_at DESC`)); err != nil {
		return nil, err
	}
	f.Albums, err = scanAlbums(s.db.QueryContext(ctx, albumSummarySQL+` JOIN favorite_albums f ON f.album_id = al.id
		GROUP BY al.id `+listed+` ORDER BY f.created_at DESC`))
	return f, err
}

// ---- playlists ----

type Playlist struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Tracks      int    `json:"tracks"`
	CoverID     int64  `json:"cover_id,omitempty"` // the first item with a cover
	UpdatedAt   int64  `json:"updated_at"`
}

type PlaylistItem struct {
	ItemID int64 `json:"item_id"`
	TrackItem
}

type PlaylistDetail struct {
	Playlist
	DurationMS int64          `json:"duration_ms"`
	Items      []PlaylistItem `json:"items"`
}

// itemAlbum is the album an item shows and plays in: the one it was added from, else the
// track's first album.
const itemAlbum = `coalesce(pi.album_id, (SELECT e.album_id FROM album_entries e WHERE e.track_id = pi.track_id ORDER BY e.id LIMIT 1))`

const playlistSQL = `SELECT p.id, p.name, p.description, p.updated_at,
	(SELECT count(*) FROM playlist_items pi WHERE pi.playlist_id = p.id),
	coalesce((SELECT al.cover_id FROM playlist_items pi JOIN albums al ON al.id = ` + itemAlbum + `
		WHERE pi.playlist_id = p.id AND al.cover_id IS NOT NULL ORDER BY pi.pos, pi.id LIMIT 1), 0)
	FROM playlists p`

func scanPlaylists(rows *sql.Rows, err error) ([]Playlist, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Playlist{}
	for rows.Next() {
		var p Playlist
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.UpdatedAt, &p.Tracks, &p.CoverID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) Playlists(ctx context.Context) ([]Playlist, error) {
	return scanPlaylists(s.db.QueryContext(ctx, playlistSQL+` ORDER BY p.updated_at DESC`))
}

func (s *Store) Playlist(ctx context.Context, id int64) (*PlaylistDetail, error) {
	list, err := scanPlaylists(s.db.QueryContext(ctx, playlistSQL+` WHERE p.id = ?`, id))
	if err != nil || len(list) == 0 {
		return nil, err
	}
	d := &PlaylistDetail{Playlist: list[0], Items: []PlaylistItem{}}
	rows, err := s.db.QueryContext(ctx, `SELECT pi.id, t.id, t.title, t.artist,
		coalesce(al.title, ''), coalesce(al.id, 0), coalesce(al.cover_id, 0), t.kind, `+briefCols+`
		FROM playlist_items pi JOIN tracks t ON t.id = pi.track_id
		JOIN assets a ON a.id = coalesce(pi.asset_id, (SELECT ta.asset_id FROM track_assets ta JOIN assets x ON x.id = ta.asset_id
			WHERE ta.track_id = t.id AND x.state = 'verified' ORDER BY ta.asset_id LIMIT 1))
		LEFT JOIN albums al ON al.id = `+itemAlbum+`
		WHERE pi.playlist_id = ? ORDER BY pi.pos, pi.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var it PlaylistItem
		t := &it.TrackItem
		if err := rows.Scan(append([]any{&it.ItemID, &t.ID, &t.Title, &t.Artist, &t.Album, &t.AlbumID, &t.CoverID, &t.Kind}, t.Asset.dest()...)...); err != nil {
			return nil, err
		}
		d.DurationMS += t.Asset.DurationMS
		d.Items = append(d.Items, it)
	}
	return d, rows.Err()
}

func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 200 {
		return "", ErrInvalid
	}
	return name, nil
}

func (s *Store) CreatePlaylist(ctx context.Context, name, description string) (int64, error) {
	name, err := cleanName(name)
	if err != nil {
		return 0, err
	}
	now := db.Now()
	r, err := s.db.ExecContext(ctx, `INSERT INTO playlists (name, description, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		name, strings.TrimSpace(description), now, now)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

func (s *Store) UpdatePlaylist(ctx context.Context, id int64, name, description string) error {
	name, err := cleanName(name)
	if err != nil {
		return err
	}
	return s.affectOne(s.db.ExecContext(ctx, `UPDATE playlists SET name = ?, description = ?, updated_at = ? WHERE id = ?`,
		name, strings.TrimSpace(description), db.Now(), id))
}

func (s *Store) DeletePlaylist(ctx context.Context, id int64) error {
	return s.affectOne(s.db.ExecContext(ctx, `DELETE FROM playlists WHERE id = ?`, id))
}

func (s *Store) affectOne(r sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

type PlaylistAdd struct {
	TrackID int64 `json:"track_id"`
	AlbumID int64 `json:"album_id"` // optional: the album it was picked from
	AssetID int64 `json:"asset_id"` // optional: a specific file version of the track
}

// AddToPlaylist appends items in order. Unknown albums or assets that are not the track's
// are dropped back to the defaults rather than failing the whole request.
func (s *Store) AddToPlaylist(ctx context.Context, id int64, items []PlaylistAdd) (int, error) {
	if len(items) == 0 || len(items) > 5000 {
		return 0, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var pos int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce((SELECT max(pos) + 1 FROM playlist_items WHERE playlist_id = p.id), 0)
		FROM playlists p WHERE p.id = ?`, id).Scan(&pos); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	now := db.Now()
	added := 0
	for _, it := range items {
		r, err := tx.ExecContext(ctx, `INSERT INTO playlist_items (playlist_id, pos, track_id, album_id, asset_id, added_at)
			SELECT ?, ?, t.id,
				(SELECT id FROM albums WHERE id = ?),
				(SELECT asset_id FROM track_assets WHERE track_id = t.id AND asset_id = ?), ?
			FROM tracks t WHERE t.id = ?`, id, pos, it.AlbumID, it.AssetID, now, it.TrackID)
		if err != nil {
			return 0, err
		}
		if n, _ := r.RowsAffected(); n > 0 {
			pos++
			added++
		}
	}
	if added == 0 {
		return 0, ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE playlists SET updated_at = ? WHERE id = ?`, now, id); err != nil {
		return 0, err
	}
	return added, tx.Commit()
}

func (s *Store) RemovePlaylistItem(ctx context.Context, id, itemID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.affectOne(tx.ExecContext(ctx, `DELETE FROM playlist_items WHERE playlist_id = ? AND id = ?`, id, itemID)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE playlists SET updated_at = ? WHERE id = ?`, db.Now(), id); err != nil {
		return err
	}
	return tx.Commit()
}

// ReorderPlaylist takes every item ID of the playlist in the new order.
func (s *Store) ReorderPlaylist(ctx context.Context, id int64, itemIDs []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM playlist_items WHERE playlist_id = ?`, id)
	if err != nil {
		return err
	}
	have := map[int64]bool{}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		have[v] = true
	}
	rows.Close()
	if len(itemIDs) != len(have) {
		return ErrInvalid
	}
	for i, v := range itemIDs {
		if !have[v] {
			return ErrInvalid
		}
		delete(have, v) // also rejects an ID given twice
		if _, err := tx.ExecContext(ctx, `UPDATE playlist_items SET pos = ? WHERE id = ?`, i, v); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE playlists SET updated_at = ? WHERE id = ?`, db.Now(), id); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- lyrics ----

const (
	LyricsEmbedded = "embedded"
	LyricsLRC      = "lrc"
	LyricsManual   = "manual"
)

type Lyrics struct {
	Source    string `json:"source"`
	Synced    bool   `json:"synced"`
	Text      string `json:"text"`
	UpdatedAt int64  `json:"updated_at"`
}

var lrcTime = regexp.MustCompile(`\[\d{1,3}:\d{1,2}(?:[.:]\d{1,3})?\]`)

// IsSynced reports LRC-style time tags.
func IsSynced(text string) bool { return lrcTime.MatchString(text) }

const maxLyrics = 256 << 10

func (s *Store) Lyrics(ctx context.Context, trackID int64) (*Lyrics, error) {
	var l Lyrics
	err := s.db.QueryRowContext(ctx, `SELECT source, synced, text, updated_at FROM lyrics WHERE track_id = ?`, trackID).
		Scan(&l.Source, &l.Synced, &l.Text, &l.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &l, err
}

// SetLyrics stores lyrics for a track. Manual text always wins and an empty manual text deletes.
// Imported text never replaces manual text, and replaces imported text only when it adds time
// tags or the old text had none either.
func (s *Store) SetLyrics(ctx context.Context, trackID int64, source, text string) (bool, error) {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if len(text) > maxLyrics {
		return false, ErrInvalid
	}
	if source == LyricsManual && text == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM lyrics WHERE track_id = ?`, trackID)
		return true, err
	}
	if text == "" {
		return false, nil
	}
	synced := IsSynced(text)
	r, err := s.db.ExecContext(ctx, `INSERT INTO lyrics (track_id, source, synced, text, updated_at)
		SELECT id, ?, ?, ?, ? FROM tracks WHERE id = ?
		ON CONFLICT (track_id) DO UPDATE SET source = excluded.source, synced = excluded.synced, text = excluded.text,
			updated_at = excluded.updated_at
		WHERE ? = 'manual' OR (lyrics.source != 'manual' AND (excluded.synced = 1 OR lyrics.synced = 0))`,
		source, synced, text, db.Now(), trackID, source)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	if n == 0 && source == LyricsManual {
		return false, ErrNotFound
	}
	return n > 0, nil
}

// ---- history ----

type HistoryItem struct {
	PlayID     int64 `json:"play_id"`
	StartedAt  int64 `json:"started_at"`
	UpdatedAt  int64 `json:"updated_at"`
	ListenedMS int64 `json:"listened_ms"`
	Counted    bool  `json:"counted"`
	Finished   bool  `json:"finished"`
	TrackItem
}

// History lists playbacks newest first, leaving out skips (under 10 s heard, not finished).
// before pages by updated_at.
func (s *Store) History(ctx context.Context, limit int, before int64) ([]HistoryItem, error) {
	if before <= 0 {
		before = 1<<62 - 1
	}
	rows, err := s.db.QueryContext(ctx, `SELECT p.id, p.started_at, p.updated_at, p.listened_ms, p.counted, p.finished,
		t.id, t.title, t.artist, coalesce(al.title, ''), coalesce(al.id, 0), coalesce(al.cover_id, 0), t.kind, `+briefCols+`
		FROM plays p JOIN tracks t ON t.id = p.track_id JOIN assets a ON a.id = p.asset_id
		LEFT JOIN albums al ON al.id = coalesce(p.album_id, (SELECT e.album_id FROM album_entries e WHERE e.track_id = t.id ORDER BY e.id LIMIT 1))
		WHERE p.updated_at < ? AND (p.listened_ms >= 10000 OR p.finished = 1 OR p.counted = 1)
		ORDER BY p.updated_at DESC LIMIT ?`, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HistoryItem{}
	for rows.Next() {
		var h HistoryItem
		t := &h.TrackItem
		if err := rows.Scan(append([]any{&h.PlayID, &h.StartedAt, &h.UpdatedAt, &h.ListenedMS, &h.Counted, &h.Finished,
			&t.ID, &t.Title, &t.Artist, &t.Album, &t.AlbumID, &t.CoverID, &t.Kind}, t.Asset.dest()...)...); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

type TopTrack struct {
	Plays int `json:"plays"`
	TrackItem
}

// TopTracks ranks tracks by counted plays (decision D9) since the given time.
func (s *Store) TopTracks(ctx context.Context, since int64, limit int) ([]TopTrack, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT track_id, count(*) FROM plays WHERE counted = 1 AND updated_at > ?
		GROUP BY track_id ORDER BY count(*) DESC, max(updated_at) DESC LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	type hit struct {
		id int64
		n  int
	}
	var hits []hit
	for rows.Next() {
		var h hit
		if err := rows.Scan(&h.id, &h.n); err != nil {
			rows.Close()
			return nil, err
		}
		hits = append(hits, h)
	}
	rows.Close()
	out := []TopTrack{}
	for _, h := range hits {
		items, err := scanTracks(s.db.QueryContext(ctx, trackSQL+` WHERE t.id = ?`, h.id))
		if err != nil {
			return nil, err
		}
		if len(items) > 0 {
			out = append(out, TopTrack{Plays: h.n, TrackItem: items[0]})
		}
	}
	return out, nil
}
