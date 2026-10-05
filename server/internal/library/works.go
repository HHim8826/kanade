package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/HHim8826/kanade/server/internal/db"
)

// Works (review #94): an anime, a game, a book… that albums and songs belong to, as Bangumi
// describes it. Albums are linked to works by hand, a song can say what it is to a work (its
// opening, an insert song…), and a work's page gathers what the library has of it. A work keeps
// what the source said and when, so it shows without the source; linking changes nothing of the
// album or its songs, and goes through the edit log, so undo takes it back.

const SourceBangumi = "bangumi"

// Uses are what a song can be to a work, in the order a work's page lists them.
var Uses = []string{"op", "ed", "insert", "theme", "character", "bgm", "other"}

// useOrder sorts songs by their use, those saying none last.
const useOrder = `CASE tw.use WHEN 'op' THEN 0 WHEN 'ed' THEN 1 WHEN 'insert' THEN 2 WHEN 'theme' THEN 3
	WHEN 'character' THEN 4 WHEN 'bgm' THEN 5 WHEN 'other' THEN 6 ELSE 7 END`

const (
	maxWorksPerAlbum = 50
	maxWorksPerTrack = 20
	maxWorkNote      = 200
)

// WorkData is what a source says of a work, to keep. Zero numbers are none.
type WorkData struct {
	Source, SourceID                      string
	Type                                  int
	Name, NameCN, Platform, Date, Summary string
	Score                                 float64
	Rank, Votes                           int
	Image                                 string
}

type Work struct {
	ID        int64   `json:"id"`
	Source    string  `json:"source"`
	SourceID  string  `json:"source_id"`
	Type      int     `json:"type"`
	Name      string  `json:"name"`
	NameCN    string  `json:"name_cn,omitempty"`
	Platform  string  `json:"platform,omitempty"`
	Date      string  `json:"date,omitempty"`
	Summary   string  `json:"summary,omitempty"`
	Score     float64 `json:"score,omitempty"`
	Rank      int     `json:"rank,omitempty"`
	Votes     int     `json:"votes,omitempty"`
	Image     bool    `json:"image"` // the source has a picture of it
	FetchedAt int64   `json:"fetched_at"`
	Albums    int     `json:"albums"` // albums in the lists linked to it
	Tracks    int     `json:"tracks"` // songs that say what they are to it
}

// WorkBrief is a work as an album shows it.
type WorkBrief struct {
	ID       int64  `json:"id"`
	SourceID string `json:"source_id"`
	Type     int    `json:"type"`
	Name     string `json:"name"`
	NameCN   string `json:"name_cn,omitempty"`
	Platform string `json:"platform,omitempty"`
	Date     string `json:"date,omitempty"`
	Image    bool   `json:"image"`
}

// TrackWork is what a song is to a work.
type TrackWork struct {
	Work int64  `json:"work_id"`
	Use  string `json:"use"`
	Note string `json:"note,omitempty"`
}

// WorkTrack is a song on a work's page.
type WorkTrack struct {
	TrackItem
	Use  string `json:"use"`
	Note string `json:"note,omitempty"`
}

type WorkDetail struct {
	Work   Work           `json:"work"`
	Albums []AlbumSummary `json:"albums"`
	Songs  []WorkTrack    `json:"songs"`
}

const workCols = `w.id, w.source, w.source_id, w.type, w.name, w.name_cn, w.platform, w.date, w.summary,
	coalesce(w.score, 0), coalesce(w.rank, 0), coalesce(w.votes, 0), w.image != '', w.fetched_at,
	(SELECT count(*) FROM album_works aw JOIN albums al ON al.id = aw.album_id WHERE aw.work_id = w.id AND ` + albumListed + `) AS albums_n,
	(SELECT count(*) FROM track_works tw WHERE tw.work_id = w.id AND EXISTS (SELECT 1 FROM track_assets ta
		JOIN assets x ON x.id = ta.asset_id WHERE ta.track_id = tw.track_id AND x.state = 'verified')) AS tracks_n`

func scanWorks(rows *sql.Rows, err error) ([]Work, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Work{}
	for rows.Next() {
		var w Work
		if err := rows.Scan(&w.ID, &w.Source, &w.SourceID, &w.Type, &w.Name, &w.NameCN, &w.Platform, &w.Date, &w.Summary,
			&w.Score, &w.Rank, &w.Votes, &w.Image, &w.FetchedAt, &w.Albums, &w.Tracks); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func nullIfZero[T int | float64](v T) any {
	if v == 0 {
		return nil
	}
	return v
}

// PutWork keeps what a source says of a work, as new or over what it said before, and returns the
// work's number. It is no edit: the source's description, not the user's.
func (s *Store) PutWork(ctx context.Context, d WorkData) (int64, error) {
	if d.Source == "" || d.SourceID == "" || strings.TrimSpace(d.Name) == "" && strings.TrimSpace(d.NameCN) == "" {
		return 0, invalid("a work needs its source and a name")
	}
	name := strings.TrimSpace(d.Name)
	if name == "" {
		name = strings.TrimSpace(d.NameCN)
	}
	now := db.Now()
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO works (source, source_id, type, name, name_cn, platform, date, summary, score, rank, votes,
		image, fetched_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (source, source_id) DO UPDATE SET type = excluded.type, name = excluded.name, name_cn = excluded.name_cn,
			platform = excluded.platform, date = excluded.date, summary = excluded.summary, score = excluded.score,
			rank = excluded.rank, votes = excluded.votes, image = excluded.image, fetched_at = excluded.fetched_at
		RETURNING id`, d.Source, d.SourceID, d.Type, name, strings.TrimSpace(d.NameCN), strings.TrimSpace(d.Platform),
		strings.TrimSpace(d.Date), strings.TrimSpace(d.Summary), nullIfZero(d.Score), nullIfZero(d.Rank), nullIfZero(d.Votes),
		d.Image, now, now).Scan(&id)
	return id, err
}

// WorkFor is the work kept for a source's subject, or nil.
func (s *Store) WorkFor(ctx context.Context, source, sourceID string) (*Work, error) {
	list, err := scanWorks(s.db.QueryContext(ctx, `SELECT `+workCols+` FROM works w WHERE w.source = ? AND w.source_id = ?`, source, sourceID))
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

// WorkImage is where the source has the work's picture ("" for none); ErrNotFound when there is no
// such work.
func (s *Store) WorkImage(ctx context.Context, id int64) (string, error) {
	var u string
	err := s.db.QueryRowContext(ctx, `SELECT image FROM works WHERE id = ?`, id).Scan(&u)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return u, err
}

// WorkSource is the source and subject a work was read from.
func (s *Store) WorkSource(ctx context.Context, id int64) (source, sourceID string, fetched int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT source, source_id, fetched_at FROM works WHERE id = ?`, id).Scan(&source, &sourceID, &fetched)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return
}

// Works lists the works something in the library is linked to, of a type (0: all), by name or, with
// byDate, the latest first.
func (s *Store) Works(ctx context.Context, typ int, byDate bool) ([]Work, error) {
	order := `name COLLATE NOCASE, id`
	if byDate {
		order = `date = '', date DESC, name COLLATE NOCASE, id`
	}
	cond, args := ``, []any{}
	if typ > 0 {
		cond, args = ` WHERE w.type = ?`, append(args, typ)
	}
	return scanWorks(s.db.QueryContext(ctx, `SELECT * FROM (SELECT `+workCols+` FROM works w`+cond+`)
		WHERE albums_n > 0 OR tracks_n > 0 ORDER BY `+order, args...))
}

// Work is a work with the albums and songs linked to it, or nil.
func (s *Store) Work(ctx context.Context, id int64) (*WorkDetail, error) {
	list, err := scanWorks(s.db.QueryContext(ctx, `SELECT `+workCols+` FROM works w WHERE w.id = ?`, id))
	if err != nil || len(list) == 0 {
		return nil, err
	}
	d := &WorkDetail{Work: list[0], Songs: []WorkTrack{}}
	if d.Albums, err = scanAlbums(s.db.QueryContext(ctx, albumSummarySQL+` WHERE al.id IN (SELECT album_id FROM album_works WHERE work_id = ?)
		GROUP BY al.id `+listed+` ORDER BY al.date = '', al.date, al.title, al.id`, id)); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT tw.use, tw.note, `+strings.TrimPrefix(trackSQL, "SELECT ")+`
		JOIN track_works tw ON tw.track_id = t.id WHERE tw.work_id = ? ORDER BY `+useOrder+`, fa.date, fa.id, t.title, t.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t WorkTrack
		if err := rows.Scan(append([]any{&t.Use, &t.Note, &t.ID, &t.Title, &t.Artist, &t.Album, &t.AlbumID, &t.CoverID, &t.Kind}, t.Asset.dest()...)...); err != nil {
			return nil, err
		}
		d.Songs = append(d.Songs, t)
	}
	return d, rows.Err()
}

// AlbumWorks are the works an album is linked to.
func (s *Store) AlbumWorks(ctx context.Context, album int64) ([]WorkBrief, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.id, w.source_id, w.type, w.name, w.name_cn, w.platform, w.date, w.image != ''
		FROM album_works aw JOIN works w ON w.id = aw.work_id WHERE aw.album_id = ? ORDER BY aw.added_at, w.id`, album)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkBrief{}
	for rows.Next() {
		var w WorkBrief
		if err := rows.Scan(&w.ID, &w.SourceID, &w.Type, &w.Name, &w.NameCN, &w.Platform, &w.Date, &w.Image); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// albumTrackWorks are what the songs of an album are to works, by song.
func (s *Store) albumTrackWorks(ctx context.Context, album int64) (map[int64][]TrackWork, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tw.track_id, tw.work_id, tw.use, tw.note FROM track_works tw
		WHERE tw.track_id IN (SELECT track_id FROM album_entries WHERE album_id = ?) ORDER BY tw.track_id, tw.work_id`, album)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]TrackWork{}
	for rows.Next() {
		var track int64
		var w TrackWork
		if err := rows.Scan(&track, &w.Work, &w.Use, &w.Note); err != nil {
			return nil, err
		}
		out[track] = append(out[track], w)
	}
	return out, rows.Err()
}

// searchWorks are the linked works whose name has q.
func (s *Store) searchWorks(ctx context.Context, q string, limit int) ([]Work, error) {
	like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(strings.TrimSpace(q)) + "%"
	return scanWorks(s.db.QueryContext(ctx, `SELECT * FROM (SELECT `+workCols+` FROM works w
		WHERE w.name LIKE ? ESCAPE '\' OR w.name_cn LIKE ? ESCAPE '\') WHERE albums_n > 0 OR tracks_n > 0
		ORDER BY name COLLATE NOCASE LIMIT ?`, like, like, limit))
}

// ---- links, through the edit log ----

func (s *Store) workName(ctx context.Context, id int64) (string, error) {
	var n string
	err := s.db.QueryRowContext(ctx, `SELECT name FROM works WHERE id = ?`, id).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: work %d", ErrNotFound, id)
	}
	return n, err
}

func (e *editor) albumWorks(album int64) ([]int64, error) {
	return idsTx(e.ctx, e.tx, `SELECT work_id FROM album_works WHERE album_id = ? ORDER BY work_id`, album)
}

func (e *editor) trackWorks(track int64) ([]TrackWork, error) {
	rows, err := e.tx.QueryContext(e.ctx, `SELECT work_id, use, note FROM track_works WHERE track_id = ? ORDER BY work_id`, track)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TrackWork{}
	for rows.Next() {
		var w TrackWork
		if err := rows.Scan(&w.Work, &w.Use, &w.Note); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// encodeTrackWorks is a song's works as a "works" edit stores them: by work, as JSON.
func encodeTrackWorks(list []TrackWork) string {
	list = slices.Clone(list)
	sort.Slice(list, func(i, j int) bool { return list[i].Work < list[j].Work })
	if list == nil {
		list = []TrackWork{}
	}
	b, _ := json.Marshal(list)
	return string(b)
}

func decodeTrackWorks(v *string) []TrackWork {
	var list []TrackWork
	if v != nil {
		json.Unmarshal([]byte(*v), &list)
	}
	return list
}

// currentWorks reads an album's or a song's "works" (ok false when it is gone).
func (e *editor) currentWorks(target string, id int64) (*string, bool, error) {
	var one int
	if err := e.tx.QueryRowContext(e.ctx, `SELECT 1 FROM `+tables[target]+` WHERE id = ?`, id).Scan(&one); errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	}
	if target == "album" {
		ids, err := e.albumWorks(id)
		return Str(encodeIDs(ids)), true, err
	}
	list, err := e.trackWorks(id)
	return Str(encodeTrackWorks(list)), true, err
}

// writeWorks stores an album's or a song's works (only works that exist).
func (e *editor) writeWorks(target string, id int64, v *string) error {
	if target == "album" {
		if _, err := e.tx.ExecContext(e.ctx, `DELETE FROM album_works WHERE album_id = ?`, id); err != nil {
			return err
		}
		if v == nil {
			return nil
		}
		now := db.Now()
		for _, w := range decodeIDs(*v) {
			if _, err := e.tx.ExecContext(e.ctx, `INSERT OR IGNORE INTO album_works (album_id, work_id, added_at)
				SELECT ?, id, ? FROM works WHERE id = ?`, id, now, w); err != nil {
				return err
			}
		}
		return nil
	}
	if _, err := e.tx.ExecContext(e.ctx, `DELETE FROM track_works WHERE track_id = ?`, id); err != nil {
		return err
	}
	for _, w := range decodeTrackWorks(v) {
		if _, err := e.tx.ExecContext(e.ctx, `INSERT OR IGNORE INTO track_works (track_id, work_id, use, note)
			SELECT ?, id, ?, ? FROM works WHERE id = ?`, id, w.Use, w.Note, w.Work); err != nil {
			return err
		}
	}
	return nil
}

// setWorks makes an album's or a song's works these, recorded when that changes anything.
func (e *editor) setWorks(target string, id int64, v *string) error {
	cur, ok, err := e.currentWorks(target, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s %d", ErrNotFound, target, id)
	}
	if same(cur, v) {
		return nil
	}
	if err := e.writeWorks(target, id, v); err != nil {
		return err
	}
	return e.record(target, id, "works", cur, v)
}

// albumSongsUse changes what the album's songs are to a work: to another work (to > 0), keeping
// what they say of it already, or to none.
func (e *editor) albumSongsUse(album, from, to int64) error {
	tracks, err := idsTx(e.ctx, e.tx, `SELECT DISTINCT track_id FROM album_entries WHERE album_id = ? ORDER BY track_id`, album)
	if err != nil {
		return err
	}
	for _, t := range tracks {
		list, err := e.trackWorks(t)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(list, func(w TrackWork) bool { return w.Work == from })
		if i < 0 {
			continue
		}
		moved := list[i]
		list = slices.Delete(list, i, i+1)
		if to > 0 && !slices.ContainsFunc(list, func(w TrackWork) bool { return w.Work == to }) {
			moved.Work = to
			list = append(list, moved)
		}
		if err := e.setWorks("track", t, Str(encodeTrackWorks(list))); err != nil {
			return err
		}
	}
	return nil
}

// LinkWork links an album to a work, as one edit. With replace, the work takes the place of that
// one (a link corrected), and the album's songs say of it what they said of the other.
func (s *Store) LinkWork(ctx context.Context, album, work, replace int64) (int64, error) {
	title, err := s.name(ctx, "album", album)
	if err != nil {
		return 0, err
	}
	name, err := s.workName(ctx, work)
	if err != nil {
		return 0, err
	}
	summary := fmt.Sprintf("將「%s」關聯到作品「%s」", title, name)
	if replace > 0 {
		old, err := s.workName(ctx, replace)
		if err != nil {
			return 0, err
		}
		summary = fmt.Sprintf("「%s」的作品從「%s」改為「%s」", title, old, name)
	}
	return s.edit(ctx, SourceUser, summary, func(e *editor) error {
		ids, err := e.albumWorks(album)
		if err != nil {
			return err
		}
		if replace > 0 && replace != work {
			i := slices.Index(ids, replace)
			if i < 0 {
				return invalid("the album is not linked to work %d", replace)
			}
			ids = slices.Delete(ids, i, i+1)
			if err := e.albumSongsUse(album, replace, work); err != nil {
				return err
			}
		}
		if !slices.Contains(ids, work) {
			ids = append(ids, work)
		}
		if len(ids) > maxWorksPerAlbum {
			return invalid("an album is linked to at most %d works", maxWorksPerAlbum)
		}
		return e.setWorks("album", album, Str(encodeIDs(ids)))
	})
}

// UnlinkWork takes an album's link to a work away, with what its songs say of the work, as one edit.
func (s *Store) UnlinkWork(ctx context.Context, album, work int64) (int64, error) {
	title, err := s.name(ctx, "album", album)
	if err != nil {
		return 0, err
	}
	name, err := s.workName(ctx, work)
	if err != nil {
		return 0, err
	}
	return s.edit(ctx, SourceUser, fmt.Sprintf("解除「%s」與作品「%s」的關聯", title, name), func(e *editor) error {
		ids, err := e.albumWorks(album)
		if err != nil {
			return err
		}
		i := slices.Index(ids, work)
		if i < 0 {
			return nil
		}
		if err := e.albumSongsUse(album, work, 0); err != nil {
			return err
		}
		return e.setWorks("album", album, Str(encodeIDs(slices.Delete(ids, i, i+1))))
	})
}

// SetTrackWorks says what a song is to works, as one edit; works left out it is nothing to.
func (s *Store) SetTrackWorks(ctx context.Context, track int64, list []TrackWork) (int64, error) {
	title, err := s.name(ctx, "track", track)
	if err != nil {
		return 0, err
	}
	if len(list) > maxWorksPerTrack {
		return 0, invalid("a song belongs to at most %d works", maxWorksPerTrack)
	}
	clean := []TrackWork{}
	for _, w := range list {
		if slices.ContainsFunc(clean, func(x TrackWork) bool { return x.Work == w.Work }) {
			return 0, invalid("work %d is given twice", w.Work)
		}
		if w.Use != "" && !slices.Contains(Uses, w.Use) {
			return 0, invalid("use is one of %s, or empty", strings.Join(Uses, ", "))
		}
		w.Note = strings.Join(strings.Fields(w.Note), " ")
		if utf8.RuneCountInString(w.Note) > maxWorkNote {
			return 0, invalid("a note is at most %d characters", maxWorkNote)
		}
		if _, err := s.workName(ctx, w.Work); err != nil {
			return 0, err
		}
		clean = append(clean, w)
	}
	return s.edit(ctx, SourceUser, fmt.Sprintf("修改「%s」的作品用途", title), func(e *editor) error {
		return e.setWorks("track", track, Str(encodeTrackWorks(clean)))
	})
}

// moveWorks gives an album merged away its works' links, as categories (review #151).
func (e *editor) moveWorks(from, to int64) error {
	moving, err := e.albumWorks(from)
	if err != nil || len(moving) == 0 {
		return err
	}
	have, err := e.albumWorks(to)
	if err != nil {
		return err
	}
	for _, id := range moving {
		if !slices.Contains(have, id) {
			have = append(have, id)
		}
	}
	if err := e.setWorks("album", to, Str(encodeIDs(have))); err != nil {
		return err
	}
	return e.setWorks("album", from, Str(""))
}

// useNames say a use as the edit log shows it.
var useNames = map[string]string{"op": "片頭曲", "ed": "片尾曲", "insert": "插曲", "theme": "主題曲", "character": "角色歌",
	"bgm": "配樂", "other": "其他"}

// worksLabel says an album's or a song's works as the edit log shows them.
func (s *Store) worksLabel(ctx context.Context, target string, v *string) string {
	if v == nil || *v == "" || *v == "[]" {
		return "（無）"
	}
	name := func(id int64) string {
		n, err := s.workName(ctx, id)
		if err != nil {
			return "（作品 " + strconv.FormatInt(id, 10) + "）"
		}
		return n
	}
	var parts []string
	if target == "album" {
		for _, id := range decodeIDs(*v) {
			parts = append(parts, name(id))
		}
	} else {
		for _, w := range decodeTrackWorks(v) {
			p := name(w.Work)
			if u := useNames[w.Use]; u != "" {
				p += "（" + u + "）"
			}
			if w.Note != "" {
				p += " " + w.Note
			}
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "、")
}

// ---- an album's own entry (review #94: Bangumi's music subjects) ----

// AlbumSubject is the work (a music subject) an album is in Bangumi as, or nil.
func (s *Store) AlbumSubject(ctx context.Context, album int64) (*Work, error) {
	list, err := scanWorks(s.db.QueryContext(ctx, `SELECT `+workCols+` FROM album_subjects a JOIN works w ON w.id = a.work_id WHERE a.album_id = ?`, album))
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

// AlbumsAsSubjects are the albums in the lists that are these Bangumi subjects (by their number),
// by subject.
func (s *Store) AlbumsAsSubjects(ctx context.Context, sourceIDs []string) (map[string][]AlbumSummary, error) {
	out := map[string][]AlbumSummary{}
	if len(sourceIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(sourceIDs))
	for i, v := range sourceIDs {
		args[i] = v
	}
	rows, err := s.db.QueryContext(ctx, `SELECT w.source_id, a.album_id FROM album_subjects a JOIN works w ON w.id = a.work_id
		WHERE w.source = 'bangumi' AND w.source_id IN (?`+strings.Repeat(", ?", len(args)-1)+`)`, args...)
	if err != nil {
		return nil, err
	}
	bySubject := map[int64]string{}
	var ids []any
	for rows.Next() {
		var sid string
		var album int64
		if err := rows.Scan(&sid, &album); err != nil {
			rows.Close()
			return nil, err
		}
		bySubject[album] = sid
		ids = append(ids, album)
	}
	rows.Close()
	if len(ids) == 0 {
		return out, nil
	}
	albums, err := scanAlbums(s.db.QueryContext(ctx, albumSummarySQL+` WHERE al.id IN (?`+strings.Repeat(", ?", len(ids)-1)+`) GROUP BY al.id `+listed, ids...))
	if err != nil {
		return nil, err
	}
	for _, a := range albums {
		out[bySubject[a.ID]] = append(out[bySubject[a.ID]], a)
	}
	return out, nil
}

func (e *editor) currentSubject(album int64) (*string, bool, error) {
	var one int
	if err := e.tx.QueryRowContext(e.ctx, `SELECT 1 FROM albums WHERE id = ?`, album).Scan(&one); errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	}
	var w int64
	err := e.tx.QueryRowContext(e.ctx, `SELECT work_id FROM album_subjects WHERE album_id = ?`, album).Scan(&w)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, true, nil
	}
	return num(w), err == nil, err
}

func (e *editor) writeSubject(album int64, v *string) error {
	if _, err := e.tx.ExecContext(e.ctx, `DELETE FROM album_subjects WHERE album_id = ?`, album); err != nil || v == nil {
		return err
	}
	w, _ := strconv.ParseInt(*v, 10, 64)
	_, err := e.tx.ExecContext(e.ctx, `INSERT INTO album_subjects (album_id, work_id, added_at) SELECT ?, id, ? FROM works WHERE id = ?`, album, db.Now(), w)
	return err
}

// SetAlbumSubject makes an album this work (a music subject) in Bangumi, or none (work 0), as one
// edit.
func (s *Store) SetAlbumSubject(ctx context.Context, album, work int64) (int64, error) {
	title, err := s.name(ctx, "album", album)
	if err != nil {
		return 0, err
	}
	summary := fmt.Sprintf("解除「%s」的 Bangumi 條目", title)
	var v *string
	if work > 0 {
		name, err := s.workName(ctx, work)
		if err != nil {
			return 0, err
		}
		summary, v = fmt.Sprintf("將「%s」綁定 Bangumi 條目「%s」", title, name), num(work)
	}
	return s.edit(ctx, SourceUser, summary, func(e *editor) error {
		cur, ok, err := e.currentSubject(album)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: album %d", ErrNotFound, album)
		}
		if same(cur, v) {
			return nil
		}
		if err := e.writeSubject(album, v); err != nil {
			return err
		}
		return e.record("album", album, "subject", cur, v)
	})
}

// moveSubject gives an album merged away its Bangumi entry, when the other has none.
func (e *editor) moveSubject(from, to int64) error {
	moving, _, err := e.currentSubject(from)
	if err != nil || moving == nil {
		return err
	}
	have, _, err := e.currentSubject(to)
	if err != nil {
		return err
	}
	if have == nil {
		if err := e.writeSubject(to, moving); err != nil {
			return err
		}
		if err := e.record("album", to, "subject", nil, moving); err != nil {
			return err
		}
	}
	if err := e.writeSubject(from, nil); err != nil {
		return err
	}
	return e.record("album", from, "subject", moving, nil)
}
