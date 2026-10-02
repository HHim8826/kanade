package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/HHim8826/kanade/server/internal/db"
)

// Organizing the library (P2-2, docs/p2-design.md). Every metadata change goes through an editor,
// which writes it to the edits table, so that an action can be undone field by field (plan §4).
// Audio files are never rewritten; the database is the source of truth.

const (
	SourceUser     = "user"
	SourceIdentify = "identify"
	SourceRestore  = "restore"
	SourceUndo     = "undo"
)

// Change sets one field of one object. A nil Value is NULL.
type Change struct {
	Target string  `json:"target"` // track | album | entry | artist
	ID     int64   `json:"id"`
	Field  string  `json:"field"`
	Value  *string `json:"value"`
}

func Str(s string) *string { return &s }
func num(n int64) *string  { return Str(strconv.FormatInt(n, 10)) }

var tables = map[string]string{"track": "tracks", "album": "albums", "entry": "album_entries", "artist": "artists"}

// fields maps each editable field to its column; "aliases" and "row" are handled separately.
var fields = map[string]map[string]string{
	"track": {"title": "title", "artist": "artist", "version": "version", "kind": "kind", "mb_recording": "mb_recording",
		"aliases": ""},
	"album": {"title": "title", "album_artist": "album_artist", "date": "date", "catalog": "catalog", "edition": "edition",
		"cover_id": "cover_id", "merged_into": "merged_into", "mb_release": "mb_release", "aliases": ""},
	"entry":  {"album_id": "album_id", "disc_no": "disc_no", "track_no": "track_no", "row": ""},
	"artist": {"aliases": ""},
}

// origin keys join their parts with U+001F (migration 0007).
const sep = "\x1f"

func albumOrigin(title, artist string) string { return title + sep + artist }

func entryOrigin(album string, disc, track int) string {
	return album + sep + strconv.Itoa(disc) + sep + strconv.Itoa(track)
}

// splitEntryOrigin undoes entryOrigin.
func splitEntryOrigin(o string) (album string, disc, track int, ok bool) {
	i := strings.LastIndex(o, sep)
	if i < 0 {
		return "", 0, 0, false
	}
	j := strings.LastIndex(o[:i], sep)
	if j < 0 {
		return "", 0, 0, false
	}
	d, err1 := strconv.Atoi(o[j+1 : i])
	t, err2 := strconv.Atoi(o[i+1:])
	return o[:j], d, t, err1 == nil && err2 == nil
}

// entryRow is an album entry as stored in a "row" edit, so a removed entry can be put back as it was.
type entryRow struct {
	ID        int64   `json:"id"`
	AlbumID   int64   `json:"album_id"`
	TrackID   int64   `json:"track_id"`
	AssetID   int64   `json:"asset_id"`
	DiscNo    int     `json:"disc_no"`
	TrackNo   int     `json:"track_no"`
	CreatedAt int64   `json:"created_at"`
	Origin    *string `json:"origin"`
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// editor applies changes inside one transaction and records them under one edit group.
type editor struct {
	ctx     context.Context
	tx      *sql.Tx
	group   int64
	n       int                // changes recorded
	reindex map[string][]int64 // kind -> objects whose search text changed
	seen    map[string]bool
}

// edit runs fn as one action. Nothing is stored when fn changes nothing; the group ID is then 0.
func (s *Store) edit(ctx context.Context, source, summary string, fn func(e *editor) error) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `INSERT INTO edit_groups (source, summary, created_at) VALUES (?, ?, ?)`, source, summary, db.Now())
	if err != nil {
		return 0, err
	}
	e := &editor{ctx: ctx, tx: tx, reindex: map[string][]int64{}, seen: map[string]bool{}}
	e.group, _ = r.LastInsertId()
	if err := fn(e); err != nil {
		return 0, err
	}
	if e.n == 0 {
		return 0, nil
	}
	for kind, ids := range e.reindex {
		for _, id := range ids {
			if err := reindex(ctx, tx, kind, id); err != nil {
				return 0, err
			}
		}
	}
	return e.group, tx.Commit()
}

func (e *editor) markIndex(kind string, id int64) {
	key := kind + strconv.FormatInt(id, 10)
	if !e.seen[key] {
		e.seen[key] = true
		e.reindex[kind] = append(e.reindex[kind], id)
	}
}

// current reads a field; ok is false when the object no longer exists.
func (e *editor) current(target string, id int64, field string) (v *string, ok bool, err error) {
	table := tables[target]
	switch field {
	case "row":
		var r entryRow
		err := e.tx.QueryRowContext(e.ctx, `SELECT id, album_id, track_id, asset_id, disc_no, track_no, created_at, origin
			FROM album_entries WHERE id = ?`, id).Scan(&r.ID, &r.AlbumID, &r.TrackID, &r.AssetID, &r.DiscNo, &r.TrackNo, &r.CreatedAt, &r.Origin)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, true, nil
		}
		if err != nil {
			return nil, false, err
		}
		b, _ := json.Marshal(r)
		return Str(string(b)), true, nil
	case "aliases":
		var one int
		if err := e.tx.QueryRowContext(e.ctx, `SELECT 1 FROM `+table+` WHERE id = ?`, id).Scan(&one); errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		} else if err != nil {
			return nil, false, err
		}
		names, err := aliasNames(e.ctx, e.tx, target, id)
		return Str(strings.Join(names, "\n")), true, err
	}
	var s sql.NullString
	err = e.tx.QueryRowContext(e.ctx, `SELECT CAST(`+fields[target][field]+` AS TEXT) FROM `+table+` WHERE id = ?`, id).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil || !s.Valid {
		return nil, err == nil, err
	}
	return &s.String, true, nil
}

func same(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// check validates a value from a user or a lookup and puts it in stored form.
func (e *editor) check(c Change) (*string, error) {
	if _, ok := fields[c.Target][c.Field]; !ok || c.Field == "row" {
		return nil, invalid("cannot change %s.%s", c.Target, c.Field)
	}
	v := ""
	if c.Value != nil {
		v = strings.TrimSpace(*c.Value)
	}
	exists := func(table string, id int64) bool {
		var one int
		return e.tx.QueryRowContext(e.ctx, `SELECT 1 FROM `+table+` WHERE id = ?`, id).Scan(&one) == nil
	}
	intIn := func(lo, hi int64) (int64, error) {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < lo || n > hi {
			return 0, invalid("%s must be a number from %d to %d", c.Field, lo, hi)
		}
		return n, nil
	}
	switch c.Field {
	case "aliases":
		return Str(strings.Join(cleanAliases(strings.Split(v, "\n")), "\n")), nil
	case "title":
		if v == "" {
			return nil, invalid("title cannot be empty")
		}
	case "kind":
		if v != "music" && v != "spoken" {
			return nil, invalid("kind is music or spoken")
		}
	case "disc_no":
		n, err := intIn(1, 99)
		return num(n), err
	case "track_no":
		n, err := intIn(0, 999)
		return num(n), err
	case "album_id":
		n, err := intIn(1, 1<<62)
		if err == nil && !exists("albums", n) {
			err = fmt.Errorf("%w: album %d", ErrNotFound, n)
		}
		return num(n), err
	case "cover_id", "merged_into":
		if c.Value == nil || v == "" || v == "0" {
			return nil, nil
		}
		n, err := intIn(1, 1<<62)
		table := map[string]string{"cover_id": "covers", "merged_into": "albums"}[c.Field]
		if err == nil && (!exists(table, n) || (c.Field == "merged_into" && n == c.ID)) {
			err = invalid("%s %d", c.Field, n)
		}
		return num(n), err
	}
	if utf8.RuneCountInString(v) > 500 {
		return nil, invalid("%s is too long", c.Field)
	}
	return &v, nil
}

func cleanAliases(in []string) []string {
	out := []string{}
	for _, a := range in {
		a = strings.TrimSpace(a)
		if a != "" && utf8.RuneCountInString(a) <= 200 && !slices.Contains(out, a) && len(out) < 50 {
			out = append(out, a)
		}
	}
	slices.Sort(out)
	return out
}

// set applies a checked change when it differs from what is stored, and records it.
func (e *editor) set(c Change) error {
	v, err := e.check(c)
	if err != nil {
		return err
	}
	cur, ok, err := e.current(c.Target, c.ID, c.Field)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s %d", ErrNotFound, c.Target, c.ID)
	}
	if same(cur, v) {
		return nil
	}
	if err := e.write(c.Target, c.ID, c.Field, v); err != nil {
		return err
	}
	return e.record(c.Target, c.ID, c.Field, cur, v)
}

func (e *editor) record(target string, id int64, field string, old, new *string) error {
	_, err := e.tx.ExecContext(e.ctx, `INSERT INTO edits (group_id, target, target_id, field, old_value, new_value)
		VALUES (?, ?, ?, ?, ?, ?)`, e.group, target, id, field, old, new)
	e.n++
	return err
}

// write stores a value as is (undo writes old values back through here too).
func (e *editor) write(target string, id int64, field string, v *string) error {
	ctx, tx := e.ctx, e.tx
	switch field {
	case "aliases":
		if _, err := tx.ExecContext(ctx, `DELETE FROM aliases WHERE target = ? AND target_id = ?`, target, id); err != nil {
			return err
		}
		if v != nil && *v != "" {
			for _, name := range strings.Split(*v, "\n") {
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO aliases (target, target_id, name) VALUES (?, ?, ?)`, target, id, name); err != nil {
					return err
				}
			}
		}
		e.markIndex(target, id)
		return nil
	case "row":
		if v == nil {
			if _, err := tx.ExecContext(ctx, `UPDATE import_items SET entry_id = NULL WHERE entry_id = ?`, id); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `DELETE FROM album_entries WHERE id = ?`, id)
			return err
		}
		var r entryRow
		if err := json.Unmarshal([]byte(*v), &r); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO album_entries (id, album_id, track_id, asset_id, disc_no, track_no, created_at, origin)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, r.AlbumID, r.TrackID, r.AssetID, r.DiscNo, r.TrackNo, r.CreatedAt, r.Origin)
		return err
	}
	var arg any
	if v != nil {
		arg = *v
		if field == "cover_id" || field == "merged_into" || field == "album_id" || field == "disc_no" || field == "track_no" {
			arg, _ = strconv.ParseInt(*v, 10, 64)
		}
	}
	stamp := ", updated_at = " + strconv.FormatInt(db.Now(), 10)
	if target == "entry" {
		stamp = ""
	}
	if _, err := tx.ExecContext(ctx, `UPDATE `+tables[target]+` SET `+fields[target][field]+` = ?`+stamp+` WHERE id = ?`, arg, id); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return invalid("the album already has this file at that disc and track number")
		}
		return err
	}
	switch {
	case target == "track" && field == "artist":
		if err := linkArtist(ctx, tx, id, deref(v)); err != nil {
			return err
		}
		e.markIndex("track", id)
	case target == "track" && field == "title", target == "album" && (field == "title" || field == "album_artist"):
		e.markIndex(target, id)
	}
	return nil
}

func deref(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// linkArtist points a track at the artist named by its artist string. Artists left without tracks
// stay (with their aliases) and are simply not listed.
func linkArtist(ctx context.Context, tx *sql.Tx, trackID int64, name string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM track_artists WHERE track_id = ?`, trackID); err != nil {
		return err
	}
	if name == "" {
		return nil
	}
	artistID, err := upsertArtist(ctx, tx, name)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO track_artists (track_id, artist_id) VALUES (?, ?)`, trackID, artistID)
	return err
}

func aliasNames(ctx context.Context, q querier, target string, id int64) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM aliases WHERE target = ? AND target_id = ? ORDER BY name`, target, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// reindex rebuilds one object's search text from its names and aliases.
func reindex(ctx context.Context, tx *sql.Tx, kind string, id int64) error {
	var parts []string
	var err error
	switch kind {
	case "track":
		var title, artist string
		err = tx.QueryRowContext(ctx, `SELECT title, artist FROM tracks WHERE id = ?`, id).Scan(&title, &artist)
		parts = []string{title, artist}
	case "album":
		var title, artist string
		err = tx.QueryRowContext(ctx, `SELECT title, album_artist FROM albums WHERE id = ?`, id).Scan(&title, &artist)
		parts = []string{title, artist}
	case "artist":
		var name string
		err = tx.QueryRowContext(ctx, `SELECT name FROM artists WHERE id = ?`, id).Scan(&name)
		parts = []string{name}
	default:
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `DELETE FROM search_index WHERE kind = ? AND ref_id = ?`, kind, id)
		return err
	}
	if err != nil {
		return err
	}
	aliases, err := aliasNames(ctx, tx, kind, id)
	if err != nil {
		return err
	}
	return index(ctx, tx, kind, id, append(parts, aliases...)...)
}

// name is an object's title or name, for summaries.
func (s *Store) name(ctx context.Context, target string, id int64) (string, error) {
	col := map[string]string{"track": "title", "album": "title", "artist": "name"}[target]
	var n string
	err := s.db.QueryRowContext(ctx, `SELECT `+col+` FROM `+tables[target]+` WHERE id = ?`, id).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return n, err
}

// ---- actions ----

// TrackEdit changes a track; nil fields stay as they are.
type TrackEdit struct {
	Title   *string   `json:"title"`
	Artist  *string   `json:"artist"`
	Version *string   `json:"version"`
	Kind    *string   `json:"kind"`
	Aliases *[]string `json:"aliases"`
}

func (e *editor) track(id int64, in TrackEdit) error {
	for _, f := range []struct {
		field string
		v     *string
	}{{"title", in.Title}, {"artist", in.Artist}, {"version", in.Version}, {"kind", in.Kind}} {
		if f.v != nil {
			if err := e.set(Change{"track", id, f.field, f.v}); err != nil {
				return err
			}
		}
	}
	if in.Aliases != nil {
		return e.set(Change{"track", id, "aliases", Str(strings.Join(*in.Aliases, "\n"))})
	}
	return nil
}

func (s *Store) EditTrack(ctx context.Context, id int64, in TrackEdit) (int64, error) {
	title, err := s.name(ctx, "track", id)
	if err != nil {
		return 0, err
	}
	return s.edit(ctx, SourceUser, fmt.Sprintf("編輯歌曲「%s」", title), func(e *editor) error { return e.track(id, in) })
}

// EntryEdit changes one entry of an album and the track on it.
type EntryEdit struct {
	EntryID int64 `json:"entry_id"`
	DiscNo  *int  `json:"disc_no"`
	TrackNo *int  `json:"track_no"`
	TrackEdit
}

// AlbumEdit changes an album, its entries and their tracks as one action.
type AlbumEdit struct {
	Title       *string     `json:"title"`
	AlbumArtist *string     `json:"album_artist"`
	Date        *string     `json:"date"`
	Catalog     *string     `json:"catalog"`
	Edition     *string     `json:"edition"`
	Kind        *string     `json:"kind"` // every track on the album
	Aliases     *[]string   `json:"aliases"`
	Entries     []EntryEdit `json:"entries"`
}

func (s *Store) EditAlbum(ctx context.Context, id int64, in AlbumEdit) (int64, error) {
	title, err := s.name(ctx, "album", id)
	if err != nil {
		return 0, err
	}
	return s.edit(ctx, SourceUser, fmt.Sprintf("編輯專輯「%s」", title), func(e *editor) error {
		for _, f := range []struct {
			field string
			v     *string
		}{{"title", in.Title}, {"album_artist", in.AlbumArtist}, {"date", in.Date}, {"catalog", in.Catalog}, {"edition", in.Edition}} {
			if f.v != nil {
				if err := e.set(Change{"album", id, f.field, f.v}); err != nil {
					return err
				}
			}
		}
		if in.Aliases != nil {
			if err := e.set(Change{"album", id, "aliases", Str(strings.Join(*in.Aliases, "\n"))}); err != nil {
				return err
			}
		}
		entries, err := e.entries(id)
		if err != nil {
			return err
		}
		if in.Kind != nil {
			for _, en := range entries {
				if err := e.set(Change{"track", en.TrackID, "kind", in.Kind}); err != nil {
					return err
				}
			}
		}
		byID := map[int64]entryRow{}
		for _, en := range entries {
			byID[en.ID] = en
		}
		for _, ed := range in.Entries {
			en, ok := byID[ed.EntryID]
			if !ok {
				return invalid("entry %d is not on this album", ed.EntryID)
			}
			if ed.DiscNo != nil {
				if err := e.set(Change{"entry", en.ID, "disc_no", num(int64(*ed.DiscNo))}); err != nil {
					return err
				}
			}
			if ed.TrackNo != nil {
				if err := e.set(Change{"entry", en.ID, "track_no", num(int64(*ed.TrackNo))}); err != nil {
					return err
				}
			}
			if err := e.track(en.TrackID, ed.TrackEdit); err != nil {
				return err
			}
		}
		return nil
	})
}

func (e *editor) entries(albumID int64) ([]entryRow, error) {
	rows, err := e.tx.QueryContext(e.ctx, `SELECT id, album_id, track_id, asset_id, disc_no, track_no, created_at, origin
		FROM album_entries WHERE album_id = ? ORDER BY disc_no, track_no, id`, albumID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []entryRow
	for rows.Next() {
		var r entryRow
		if err := rows.Scan(&r.ID, &r.AlbumID, &r.TrackID, &r.AssetID, &r.DiscNo, &r.TrackNo, &r.CreatedAt, &r.Origin); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetAlbumCover points an album at a stored cover.
func (s *Store) SetAlbumCover(ctx context.Context, id, coverID int64) (int64, error) {
	title, err := s.name(ctx, "album", id)
	if err != nil {
		return 0, err
	}
	return s.edit(ctx, SourceUser, fmt.Sprintf("更換「%s」的封面", title), func(e *editor) error {
		return e.set(Change{"album", id, "cover_id", num(coverID)})
	})
}

// SetAliases replaces the other names of an artist, album or track.
func (s *Store) SetAliases(ctx context.Context, target string, id int64, names []string) (int64, error) {
	if _, ok := fields[target]["aliases"]; !ok {
		return 0, ErrInvalid
	}
	n, err := s.name(ctx, target, id)
	if err != nil {
		return 0, err
	}
	return s.edit(ctx, SourceUser, fmt.Sprintf("編輯「%s」的別名", n), func(e *editor) error {
		return e.set(Change{target, id, "aliases", Str(strings.Join(names, "\n"))})
	})
}

// RenameArtist changes the artist string on every track and album that uses exactly this name.
// The new name becomes (or joins) its own artist, which takes over the aliases and keeps the old
// name as one more, so searching the old name still finds it. The old artist is no longer listed
// once nothing uses it.
func (s *Store) RenameArtist(ctx context.Context, id int64, name string) (int64, error) {
	old, err := s.name(ctx, "artist", id)
	if err != nil {
		return 0, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, invalid("name cannot be empty")
	}
	return s.edit(ctx, SourceUser, fmt.Sprintf("歌手改名：%s → %s", old, name), func(e *editor) error {
		for _, q := range []struct{ target, sql string }{
			{"track", `SELECT id FROM tracks WHERE artist = ?`},
			{"album", `SELECT id FROM albums WHERE album_artist = ?`},
		} {
			ids, err := idsTx(e.ctx, e.tx, q.sql, old)
			if err != nil {
				return err
			}
			field := map[string]string{"track": "artist", "album": "album_artist"}[q.target]
			for _, oid := range ids {
				if err := e.set(Change{q.target, oid, field, &name}); err != nil {
					return err
				}
			}
		}
		var to int64
		if err := e.tx.QueryRowContext(ctx, `SELECT id FROM artists WHERE name = ?`, name).Scan(&to); err != nil || to == id {
			return nil // no track carries the name (only album artists), or only the case changed
		}
		oldAliases, err := aliasNames(ctx, e.tx, "artist", id)
		if err != nil {
			return err
		}
		newAliases, err := aliasNames(ctx, e.tx, "artist", to)
		if err != nil {
			return err
		}
		merged := cleanAliases(append(append(newAliases, oldAliases...), old))
		return e.set(Change{"artist", to, "aliases", Str(strings.Join(merged, "\n"))})
	})
}

func idsTx(ctx context.Context, q querier, query string, args ...any) ([]int64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// MergeAlbum moves every entry of one album into another. A file the target already has is not
// added twice: that entry is removed instead. The emptied album remembers where it went, so later
// imports with its tags land in the target too.
func (s *Store) MergeAlbum(ctx context.Context, from, into int64) (int64, error) {
	if from == into {
		return 0, invalid("cannot merge an album into itself")
	}
	fromTitle, err := s.name(ctx, "album", from)
	if err != nil {
		return 0, err
	}
	intoTitle, err := s.name(ctx, "album", into)
	if err != nil {
		return 0, err
	}
	g, err := s.edit(ctx, SourceUser, fmt.Sprintf("將「%s」合併到「%s」", fromTitle, intoTitle), func(e *editor) error {
		var merged sql.NullInt64
		if err := e.tx.QueryRowContext(ctx, `SELECT merged_into FROM albums WHERE id = ?`, into).Scan(&merged); err != nil {
			return err
		}
		if merged.Valid {
			return invalid("the target album was itself merged into another one")
		}
		entries, err := e.entries(from)
		if err != nil {
			return err
		}
		for _, en := range entries {
			var dup int
			err := e.tx.QueryRowContext(ctx, `SELECT 1 FROM album_entries WHERE album_id = ? AND asset_id = ?`, into, en.AssetID).Scan(&dup)
			switch {
			case err == nil:
				if err := e.removeEntry(en.ID); err != nil {
					return err
				}
			case errors.Is(err, sql.ErrNoRows):
				if err := e.set(Change{"entry", en.ID, "album_id", num(into)}); err != nil {
					return err
				}
			default:
				return err
			}
		}
		return e.set(Change{"album", from, "merged_into", num(into)})
	})
	if err == nil && g != 0 { // a favorite album stays a favorite under its new home
		s.db.ExecContext(ctx, `INSERT OR IGNORE INTO favorite_albums (album_id, created_at)
			SELECT ?, created_at FROM favorite_albums WHERE album_id = ?`, into, from)
	}
	return g, err
}

// removeEntry deletes an entry and records the whole row, so undo can put it back.
func (e *editor) removeEntry(id int64) error {
	cur, _, err := e.current("entry", id, "row")
	if err != nil || cur == nil {
		return err
	}
	if err := e.write("entry", id, "row", nil); err != nil {
		return err
	}
	return e.record("entry", id, "row", cur, nil)
}

// SplitAlbum moves some entries of an album to a new album with the same details and a new title.
func (s *Store) SplitAlbum(ctx context.Context, from int64, entryIDs []int64, title string) (albumID, group int64, err error) {
	fromTitle, err := s.name(ctx, "album", from)
	if err != nil {
		return 0, 0, err
	}
	title = strings.TrimSpace(title)
	if title == "" || len(entryIDs) == 0 {
		return 0, 0, invalid("a split needs a title and at least one entry")
	}
	group, err = s.edit(ctx, SourceUser, fmt.Sprintf("從「%s」拆分出「%s」（%d 首）", fromTitle, title, len(entryIDs)), func(e *editor) error {
		entries, err := e.entries(from)
		if err != nil {
			return err
		}
		on := map[int64]bool{}
		for _, en := range entries {
			on[en.ID] = true
		}
		for _, id := range entryIDs {
			if !on[id] {
				return invalid("entry %d is not on this album", id)
			}
		}
		// A hand-made album has no origin, so imports never join it on their own.
		now := db.Now()
		r, err := e.tx.ExecContext(ctx, `INSERT INTO albums (title, album_artist, date, cover_id, catalog, edition, created_at, updated_at)
			SELECT ?, album_artist, date, cover_id, catalog, edition, ?, ? FROM albums WHERE id = ?`, title, now, now, from)
		if err != nil {
			return err
		}
		albumID, _ = r.LastInsertId()
		e.markIndex("album", albumID)
		for _, id := range entryIDs {
			if err := e.set(Change{"entry", id, "album_id", num(albumID)}); err != nil {
				return err
			}
		}
		return nil
	})
	return albumID, group, err
}

// RemoveEntries takes entries off an album (all of them when entryIDs is empty). The tracks stay
// in the library as standalone songs, and nothing is deleted from Drive.
func (s *Store) RemoveEntries(ctx context.Context, albumID int64, entryIDs []int64) (int64, error) {
	title, err := s.name(ctx, "album", albumID)
	if err != nil {
		return 0, err
	}
	summary := fmt.Sprintf("移除專輯「%s」", title)
	if len(entryIDs) > 0 {
		summary = fmt.Sprintf("從「%s」移除 %d 首", title, len(entryIDs))
	}
	return s.edit(ctx, SourceUser, summary, func(e *editor) error {
		entries, err := e.entries(albumID)
		if err != nil {
			return err
		}
		want := map[int64]bool{}
		for _, id := range entryIDs {
			want[id] = true
		}
		removed := 0
		for _, en := range entries {
			if len(entryIDs) == 0 || want[en.ID] {
				if err := e.removeEntry(en.ID); err != nil {
					return err
				}
				removed++
			}
		}
		if len(entryIDs) > 0 && removed != len(want) {
			return invalid("some entries are not on this album")
		}
		return nil
	})
}

// OriginalFunc returns what an import made of a track from its saved tags, or nil when there is
// no import record. The importer provides it.
type OriginalFunc func(ctx context.Context, trackID int64) (*EntryInput, error)

// RestoreAlbum puts an album back the way it was imported: its title and album artist, each
// entry's disc and track number and album (entries merged in from other albums go home, entries
// split off come back), and the tracks' own tags. Identification and hand edits are undone; aliases
// and the cover stay. The restore is itself an edit and can be undone.
func (s *Store) RestoreAlbum(ctx context.Context, id int64, original OriginalFunc) (int64, error) {
	var title string
	var origin sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT title, origin FROM albums WHERE id = ?`, id).Scan(&title, &origin)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	// Entries here, and entries imported here that were moved elsewhere.
	query := `SELECT id, track_id, coalesce(origin, '') FROM album_entries WHERE album_id = ?`
	args := []any{id}
	if origin.Valid {
		prefix := origin.String + sep
		query += ` OR substr(origin, 1, length(?)) = ?`
		args = append(args, prefix, prefix)
	}
	type ent struct {
		id, track int64
		origin    string
	}
	var ents []ent
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var en ent
		if err := rows.Scan(&en.id, &en.track, &en.origin); err != nil {
			rows.Close()
			return 0, err
		}
		ents = append(ents, en)
	}
	rows.Close()
	originals := map[int64]*EntryInput{}
	for _, en := range ents {
		if _, done := originals[en.track]; !done {
			if originals[en.track], err = original(ctx, en.track); err != nil {
				return 0, err
			}
		}
	}

	return s.edit(ctx, SourceRestore, fmt.Sprintf("恢復「%s」的原標籤", title), func(e *editor) error {
		if origin.Valid {
			t, a, _ := strings.Cut(origin.String, sep)
			date := ""
			for _, en := range ents {
				if o := originals[en.track]; o != nil && o.Date != "" && strings.HasPrefix(en.origin, origin.String+sep) {
					date = o.Date
					break
				}
			}
			for _, c := range []Change{{"album", id, "title", &t}, {"album", id, "album_artist", &a}, {"album", id, "date", &date},
				{"album", id, "catalog", Str("")}, {"album", id, "edition", Str("")}, {"album", id, "mb_release", Str("")},
				{"album", id, "merged_into", nil}} {
				if err := e.set(c); err != nil {
					return err
				}
			}
		}
		for _, en := range ents {
			if albumKey, disc, track, ok := splitEntryOrigin(en.origin); ok {
				home := id
				if !origin.Valid || albumKey != origin.String {
					var other int64
					err := e.tx.QueryRowContext(ctx, `SELECT id FROM albums WHERE origin = ? ORDER BY id LIMIT 1`, albumKey).Scan(&other)
					if err != nil && !errors.Is(err, sql.ErrNoRows) {
						return err
					}
					if other != 0 {
						home = other
						if err := e.set(Change{"album", other, "merged_into", nil}); err != nil {
							return err
						}
					}
				}
				for _, c := range []Change{{"entry", en.id, "album_id", num(home)}, {"entry", en.id, "disc_no", num(int64(max(disc, 1)))},
					{"entry", en.id, "track_no", num(int64(track))}} {
					if err := e.set(c); err != nil {
						return err
					}
				}
			}
			if err := e.restoreTrack(en.track, originals[en.track]); err != nil {
				return err
			}
		}
		return nil
	})
}

func (e *editor) restoreTrack(id int64, o *EntryInput) error {
	if o == nil {
		return nil
	}
	kind := o.Kind
	if kind == "" {
		kind = "music"
	}
	if err := e.track(id, TrackEdit{Title: &o.Title, Artist: &o.Artist, Version: Str(""), Kind: &kind}); err != nil {
		return err
	}
	return e.set(Change{"track", id, "mb_recording", Str("")}) // only identification sets it
}

// RestoreTrack puts a track's own tags back as imported.
func (s *Store) RestoreTrack(ctx context.Context, id int64, original OriginalFunc) (int64, error) {
	title, err := s.name(ctx, "track", id)
	if err != nil {
		return 0, err
	}
	o, err := original(ctx, id)
	if err != nil {
		return 0, err
	}
	if o == nil {
		return 0, invalid("this track has no import record")
	}
	return s.edit(ctx, SourceRestore, fmt.Sprintf("恢復歌曲「%s」的原標籤", title), func(e *editor) error { return e.restoreTrack(id, o) })
}

// ---- undo ----

// Conflict is a field an undo left alone.
type Conflict struct {
	Target string  `json:"target"`
	ID     int64   `json:"id"`
	Field  string  `json:"field"`
	Name   string  `json:"name"`
	Reason string  `json:"reason"` // changed: edited again since; gone: the object no longer exists; failed
	Want   *string `json:"want"`   // the value undo would have replaced
	Now    *string `json:"now"`    // what is there now
}

var ErrAlreadyUndone = errors.New("this change was already undone")

// Undo reverts the fields a group changed, newest first. A field that was changed again afterwards
// is left as it is and reported (plan §4: undo never overwrites later corrections). The undo is a
// group of its own, so it can be undone in turn.
func (s *Store) Undo(ctx context.Context, groupID int64) (int64, []Conflict, error) {
	var summary string
	var undone sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT summary, undone_by FROM edit_groups WHERE id = ?`, groupID).Scan(&summary, &undone)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil, ErrNotFound
	}
	if err != nil {
		return 0, nil, err
	}
	if undone.Valid {
		return 0, nil, ErrAlreadyUndone
	}
	edits, err := s.groupEdits(ctx, groupID)
	if err != nil {
		return 0, nil, err
	}
	if len(edits) == 0 {
		return 0, nil, invalid("nothing in this change can be undone")
	}
	var conflicts []Conflict
	g, err := s.edit(ctx, SourceUndo, "撤回："+summary, func(e *editor) error {
		conflicts = nil
		for i := len(edits) - 1; i >= 0; i-- {
			ed := edits[i]
			cur, ok, err := e.current(ed.Target, ed.ID, ed.Field)
			if err != nil {
				return err
			}
			c := Conflict{Target: ed.Target, ID: ed.ID, Field: ed.Field, Want: ed.New, Now: cur}
			later, err := laterEdit(ctx, e.tx, groupID, e.group, ed)
			if err != nil {
				return err
			}
			switch {
			case !ok:
				c.Reason = "gone"
			case later || !same(cur, ed.New): // changed since, even if back to the same value (review #20)
				c.Reason = "changed"
			default:
				if _, err := e.tx.ExecContext(ctx, `SAVEPOINT undo_one`); err != nil {
					return err
				}
				if err := e.write(ed.Target, ed.ID, ed.Field, ed.Old); err != nil {
					e.tx.ExecContext(ctx, `ROLLBACK TO undo_one`)
					c.Reason = "failed"
				} else {
					e.tx.ExecContext(ctx, `RELEASE undo_one`)
					if err := e.record(ed.Target, ed.ID, ed.Field, cur, ed.Old); err != nil {
						return err
					}
					continue
				}
			}
			conflicts = append(conflicts, c)
		}
		if e.n > 0 {
			if _, err := e.tx.ExecContext(ctx, `UPDATE edit_groups SET undo_of = ? WHERE id = ?`, groupID, e.group); err != nil {
				return err
			}
			_, err := e.tx.ExecContext(ctx, `UPDATE edit_groups SET undone_by = ? WHERE id = ?`, e.group, groupID)
			return err
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	for i := range conflicts {
		conflicts[i].Name = s.targetName(ctx, conflicts[i].Target, conflicts[i].ID, conflicts[i].Want)
	}
	return g, conflicts, nil
}

// laterEdit reports whether a change after group touched the same field and still stands: one not
// undone, and not itself the undo of a change after group (such a pair cancels out).
func laterEdit(ctx context.Context, q querier, group, undoing int64, ed Edit) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM edits x JOIN edit_groups g ON g.id = x.group_id
		WHERE x.target = ? AND x.target_id = ? AND x.field = ? AND x.group_id > ? AND x.group_id != ?
		AND g.undone_by IS NULL AND NOT (g.undo_of IS NOT NULL AND g.undo_of > ?) LIMIT 1`,
		ed.Target, ed.ID, ed.Field, group, undoing, group).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ---- the edit log ----

type EditGroup struct {
	ID        int64  `json:"id"`
	Source    string `json:"source"`
	Summary   string `json:"summary"`
	CreatedAt int64  `json:"created_at"`
	UndoOf    int64  `json:"undo_of,omitempty"`
	UndoneBy  int64  `json:"undone_by,omitempty"`
	Changes   int    `json:"changes"`
	Edits     []Edit `json:"edits,omitempty"`
}

type Edit struct {
	Target   string  `json:"target"`
	ID       int64   `json:"id"`
	Field    string  `json:"field"`
	Name     string  `json:"name"` // the object's title or name now; empty when it is gone
	Old      *string `json:"old"`
	New      *string `json:"new"`
	OldLabel string  `json:"old_label,omitempty"` // album titles for album references
	NewLabel string  `json:"new_label,omitempty"`
}

const groupSQL = `SELECT g.id, g.source, g.summary, g.created_at, coalesce(g.undo_of, 0), coalesce(g.undone_by, 0),
	(SELECT count(*) FROM edits x WHERE x.group_id = g.id) FROM edit_groups g`

func scanGroups(rows *sql.Rows, err error) ([]EditGroup, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EditGroup{}
	for rows.Next() {
		var g EditGroup
		if err := rows.Scan(&g.ID, &g.Source, &g.Summary, &g.CreatedAt, &g.UndoOf, &g.UndoneBy, &g.Changes); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// EditGroups lists actions newest first; before pages by group ID.
func (s *Store) EditGroups(ctx context.Context, limit int, before int64) ([]EditGroup, error) {
	if before <= 0 {
		before = 1 << 62
	}
	return scanGroups(s.db.QueryContext(ctx, groupSQL+` WHERE g.id < ? ORDER BY g.id DESC LIMIT ?`, before, limit))
}

func (s *Store) EditGroup(ctx context.Context, id int64) (*EditGroup, error) {
	list, err := scanGroups(s.db.QueryContext(ctx, groupSQL+` WHERE g.id = ?`, id))
	if err != nil || len(list) == 0 {
		return nil, err
	}
	g := &list[0]
	if g.Edits, err = s.groupEdits(ctx, id); err != nil {
		return nil, err
	}
	albumTitle := func(v *string) string {
		if v == nil {
			return ""
		}
		var t string
		s.db.QueryRowContext(ctx, `SELECT title FROM albums WHERE id = ?`, *v).Scan(&t)
		return t
	}
	for i := range g.Edits {
		ed := &g.Edits[i]
		ed.Name = s.targetName(ctx, ed.Target, ed.ID, coalesceStr(ed.Old, ed.New))
		if ed.Field == "album_id" || ed.Field == "merged_into" {
			ed.OldLabel, ed.NewLabel = albumTitle(ed.Old), albumTitle(ed.New)
		}
	}
	return g, nil
}

func coalesceStr(a, b *string) *string {
	if a != nil {
		return a
	}
	return b
}

func (s *Store) groupEdits(ctx context.Context, id int64) ([]Edit, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT target, target_id, field, old_value, new_value FROM edits WHERE group_id = ? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Edit
	for rows.Next() {
		var e Edit
		if err := rows.Scan(&e.Target, &e.ID, &e.Field, &e.Old, &e.New); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// targetName names an object for the edit log: an entry by its track's title, which a removed
// entry still carries in its stored row.
func (s *Store) targetName(ctx context.Context, target string, id int64, row *string) string {
	if target == "entry" {
		var trackID int64
		if s.db.QueryRowContext(ctx, `SELECT track_id FROM album_entries WHERE id = ?`, id).Scan(&trackID) != nil && row != nil {
			var r entryRow
			if json.Unmarshal([]byte(*row), &r) == nil {
				trackID = r.TrackID
			}
		}
		target, id = "track", trackID
	}
	n, _ := s.name(ctx, target, id)
	return n
}

// ---- permanent deletion ----

// AlbumTrackIDs lists the album's tracks, including those whose file is missing from Drive, which
// album pages leave out.
func (s *Store) AlbumTrackIDs(ctx context.Context, albumID int64) ([]int64, error) {
	return idsTx(ctx, s.db, `SELECT track_id FROM album_entries WHERE album_id = ? GROUP BY track_id ORDER BY min(id)`, albumID)
}

// DeleteTrack removes a track for good, with its entries, playlist items, favorite, lyrics and
// plays. Files that no other track uses leave the library; their Drive file IDs are returned so the
// caller can move them to the Drive trash. This cannot be undone and is logged as an empty action.
func (s *Store) DeleteTrack(ctx context.Context, id int64) ([]string, error) {
	title, err := s.name(ctx, "track", id)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	assets, err := idsTx(ctx, tx, `SELECT asset_id FROM track_assets WHERE track_id = ?`, id)
	if err != nil {
		return nil, err
	}
	for _, q := range []string{
		`UPDATE import_items SET entry_id = NULL WHERE entry_id IN (SELECT id FROM album_entries WHERE track_id = ?1)`,
		`UPDATE import_items SET track_id = NULL WHERE track_id = ?1`,
		`DELETE FROM album_entries WHERE track_id = ?1`,
		`DELETE FROM aliases WHERE target = 'track' AND target_id = ?1`,
		`DELETE FROM search_index WHERE kind = 'track' AND ref_id = ?1`,
		`DELETE FROM tracks WHERE id = ?1`, // cascades to links, plays, favorites, playlist items, lyrics
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return nil, err
		}
	}
	var drive []string
	for _, a := range assets {
		var used int
		if tx.QueryRowContext(ctx, `SELECT 1 FROM track_assets WHERE asset_id = ? LIMIT 1`, a).Scan(&used) == nil {
			continue
		}
		var fileID sql.NullString
		tx.QueryRowContext(ctx, `SELECT drive_file_id FROM assets WHERE id = ?`, a).Scan(&fileID)
		for _, q := range []string{
			`UPDATE import_items SET asset_id = NULL WHERE asset_id = ?`,
			`DELETE FROM plays WHERE asset_id = ?`,
			`DELETE FROM album_entries WHERE asset_id = ?`,
			`DELETE FROM assets WHERE id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, q, a); err != nil {
				return nil, err
			}
		}
		if fileID.Valid && fileID.String != "" {
			drive = append(drive, fileID.String)
			// Owed to the Drive trash until it is there, in the same transaction (review #26).
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO drive_trash (file_id, created_at) VALUES (?, ?)`,
				fileID.String, db.Now()); err != nil {
				return nil, err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO edit_groups (source, summary, created_at) VALUES (?, ?, ?)`,
		SourceUser, fmt.Sprintf("永久刪除歌曲「%s」", title), db.Now()); err != nil {
		return nil, err
	}
	return drive, tx.Commit()
}

// ApplyChanges applies a set of looked-up changes as one action, for example a MusicBrainz
// identification. Fields that already hold the value are skipped.
func (s *Store) ApplyChanges(ctx context.Context, source, summary string, changes []Change) (int64, error) {
	return s.edit(ctx, source, summary, func(e *editor) error {
		for _, c := range changes {
			if err := e.set(c); err != nil {
				return err
			}
		}
		return nil
	})
}
