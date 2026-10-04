package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/HHim8826/kanade/server/internal/db"
)

// Batch changes of the library (review #83). Each one checks every album, song or entry it is
// given before changing anything, and is one edit that undo takes back as a whole.

// AlbumBrief names an album in a plan.
type AlbumBrief struct {
	ID          int64  `json:"id"` // 0: a new album
	Title       string `json:"title"`
	AlbumArtist string `json:"album_artist"`
}

// Move is where one entry goes.
type Move struct {
	EntryID   int64      `json:"entry_id"`
	TrackID   int64      `json:"track_id"`
	Title     string     `json:"title"`
	Artist    string     `json:"artist"`
	From      AlbumBrief `json:"from"`
	Disc      int        `json:"disc"`
	Track     int        `json:"track"`
	Duplicate bool       `json:"duplicate"` // the album has its file already: this entry goes instead
	assetID   int64
}

// Add is a song of the library that joins an album with its file, as a new entry (a song a source
// brought that the library had from another import, or one taken off every album since).
type Add struct {
	TrackID int64  `json:"track_id"`
	Title   string `json:"title"`
	Artist  string `json:"artist"`
	Disc    int    `json:"disc"`
	Track   int    `json:"track"`
	Loose   bool   `json:"loose,omitempty"` // on no album now
}

// Plan is what an arrangement does, shown before it is done (merging albums, organizing a source).
type Plan struct {
	Target   AlbumBrief     `json:"target"`
	Moves    []Move         `json:"moves"`
	Adds     []Add          `json:"adds,omitempty"`
	Sections map[int]string `json:"sections"`
	Emptied  []AlbumBrief   `json:"emptied"`  // left with no songs: they point at the target afterwards
	Sidecars int            `json:"sidecars"` // CUE sheets and logs of the emptied albums, kept with the target
}

// MergeRequest merges albums into one.
type MergeRequest struct {
	Albums      []int64 `json:"albums"` // in this order
	Into        int64   `json:"into"`   // an album of the library, one of these or not; 0: a new album
	Title       string  `json:"title"`  // of a new album
	AlbumArtist string  `json:"album_artist"`
	// Sections: each album (each disc of one) becomes a section of its own, named after it, in the
	// order given; else the songs keep their disc and track numbers.
	Sections bool `json:"sections"`
}

func brief(ctx context.Context, q querier, id int64) (AlbumBrief, error) {
	b := AlbumBrief{ID: id}
	var merged sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT title, album_artist, merged_into FROM albums WHERE id = ?`, id).Scan(&b.Title, &b.AlbumArtist, &merged)
	if errors.Is(err, sql.ErrNoRows) {
		return b, fmt.Errorf("%w: album %d", ErrNotFound, id)
	}
	if err == nil && merged.Valid {
		err = invalid("album %d was merged into another one", id)
	}
	return b, err
}

// entryMoves lists an album's entries as moves to where they are now.
func entryMoves(ctx context.Context, q querier, from AlbumBrief) ([]Move, error) {
	rows, err := q.QueryContext(ctx, `SELECT e.id, e.track_id, e.asset_id, e.disc_no, e.track_no, t.title, t.artist
		FROM album_entries e JOIN tracks t ON t.id = e.track_id WHERE e.album_id = ? ORDER BY e.disc_no, e.track_no, e.id`, from.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Move
	for rows.Next() {
		m := Move{From: from}
		if err := rows.Scan(&m.EntryID, &m.TrackID, &m.assetID, &m.Disc, &m.Track, &m.Title, &m.Artist); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) planMerge(ctx context.Context, q querier, req MergeRequest) (*Plan, error) {
	var ids []int64
	for _, id := range req.Albums {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	p := &Plan{Sections: map[int]string{}, Moves: []Move{}, Emptied: []AlbumBrief{}}
	if req.Into != 0 {
		t, err := brief(ctx, q, req.Into)
		if err != nil {
			return nil, err
		}
		p.Target = t
	} else {
		p.Target = AlbumBrief{Title: strings.TrimSpace(req.Title), AlbumArtist: strings.TrimSpace(req.AlbumArtist)}
		if p.Target.Title == "" {
			return nil, invalid("a new album needs a title")
		}
	}
	members := ids
	if len(members) == 0 || (len(members) == 1 && members[0] == req.Into) {
		return nil, invalid("choose albums to merge")
	}
	// Where sections start: after the target's own discs, unless it is one of the albums merged.
	next := 1
	if req.Into != 0 && !slices.Contains(members, req.Into) {
		if err := q.QueryRowContext(ctx, `SELECT coalesce(max(disc_no), 0) + 1 FROM album_entries WHERE album_id = ?`, req.Into).Scan(&next); err != nil {
			return nil, err
		}
		if names, err := sectionNames(ctx, q, req.Into); err == nil {
			for d, n := range names {
				p.Sections[d] = n
			}
		}
	}
	for _, id := range members {
		b, err := brief(ctx, q, id)
		if err != nil {
			return nil, err
		}
		moves, err := entryMoves(ctx, q, b)
		if err != nil {
			return nil, err
		}
		if req.Sections {
			discs := map[int]int{} // its disc -> the section
			own, _ := sectionNames(ctx, q, id)
			var order []int
			for _, m := range moves {
				if _, ok := discs[m.Disc]; !ok {
					order = append(order, m.Disc)
					discs[m.Disc] = 0
				}
			}
			for _, d := range order {
				if next > 99 {
					return nil, invalid("more than 99 sections")
				}
				discs[d] = next
				name := b.Title
				switch {
				case own[d] != "":
					name += " " + own[d]
				case len(order) > 1:
					name += fmt.Sprintf(" Disc %d", d)
				}
				p.Sections[next] = name
				next++
			}
			for i := range moves {
				moves[i].Disc = discs[moves[i].Disc]
			}
		}
		p.Moves = append(p.Moves, moves...)
		if id != req.Into {
			p.Emptied = append(p.Emptied, b)
		}
	}
	return p, s.markDuplicates(ctx, q, p)
}

// markDuplicates marks the moves whose file the target has already (or that an earlier move
// brings): those entries go instead of making a second one. The target's own entries come first.
func (s *Store) markDuplicates(ctx context.Context, q querier, p *Plan) error {
	have := map[int64]bool{}
	moving := map[int64]bool{}
	for i := range p.Moves {
		m := &p.Moves[i]
		moving[m.EntryID] = true
		if m.assetID == 0 { // a plan made elsewhere
			if err := q.QueryRowContext(ctx, `SELECT asset_id FROM album_entries WHERE id = ?`, m.EntryID).Scan(&m.assetID); err != nil {
				return fmt.Errorf("%w: entry %d", ErrNotFound, m.EntryID)
			}
		}
	}
	if p.Target.ID != 0 {
		rows, err := q.QueryContext(ctx, `SELECT id, asset_id FROM album_entries WHERE album_id = ?`, p.Target.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, a int64
			if rows.Scan(&id, &a) == nil && !moving[id] {
				have[a] = true
			}
		}
		rows.Close()
	}
	for _, own := range []bool{true, false} {
		for i := range p.Moves {
			if m := &p.Moves[i]; (m.From.ID == p.Target.ID && p.Target.ID != 0) == own {
				m.Duplicate = have[m.assetID]
				have[m.assetID] = true
			}
		}
	}
	for _, e := range p.Emptied {
		var n int
		q.QueryRowContext(ctx, `SELECT count(*) FROM sidecars WHERE album_id = ?`, e.ID).Scan(&n)
		p.Sidecars += n
	}
	return nil
}

// CheckPlan fills in what a plan made elsewhere does as the library is now: which entries are
// duplicates, and the CUE sheets and logs that follow the albums it empties.
func (s *Store) CheckPlan(ctx context.Context, p *Plan) error { return s.markDuplicates(ctx, s.db, p) }

// PlanMerge says what MergeAlbums would do.
func (s *Store) PlanMerge(ctx context.Context, req MergeRequest) (*Plan, error) {
	return s.planMerge(ctx, s.db, req)
}

// MergeAlbums merges albums into one of them, another album or a new one, as one edit.
func (s *Store) MergeAlbums(ctx context.Context, req MergeRequest) (albumID, group int64, err error) {
	var plan *Plan
	group, err = s.edit(ctx, SourceUser, "", func(e *editor) error {
		if plan, err = s.planMerge(ctx, e.tx, req); err != nil {
			return err
		}
		albumID, err = e.arrange(plan, mergeSummary(plan))
		return err
	})
	return albumID, group, err
}

func mergeSummary(p *Plan) string {
	return fmt.Sprintf("將 %d 張專輯合併到「%s」（%d 首）", len(p.Emptied), p.Target.Title, len(p.Moves))
}

// Arrange carries out a plan made elsewhere (organizing a download's songs, review #82): the moves
// are checked against the library as it is now. summary names the edit.
func (s *Store) Arrange(ctx context.Context, p *Plan, summary string, rules ...Rule) (albumID, group int64, err error) {
	group, err = s.edit(ctx, SourceUser, summary, func(e *editor) error {
		if err := s.markDuplicates(ctx, e.tx, p); err != nil {
			return err
		}
		if albumID, err = e.arrange(p, ""); err != nil {
			return err
		}
		for _, r := range rules {
			if err := e.rule(r, albumID); err != nil {
				return err
			}
		}
		return nil
	})
	return albumID, group, err
}

// Rule is how a download imports from now on, changed in the same edit as an arrangement of its
// songs (review #87, #88): a collection made from it is saved with the grouping its later rounds
// follow and the scope that finds the collection again, all or nothing, and undo takes them back
// with the songs.
type Rule struct {
	Download int64  // downloads.id
	Grouping string // its grouping from now on (downloads.grouping)
	Scope    string // an album_scopes scope that goes to the arranged album; "" for none
}

func (e *editor) rule(r Rule, album int64) error {
	if r.Download != 0 {
		cur, ok, err := e.current("download", r.Download, "grouping")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: download %d", ErrNotFound, r.Download)
		}
		if v := Str(r.Grouping); !same(cur, v) {
			if err := e.write("download", r.Download, "grouping", v); err != nil {
				return err
			}
			if err := e.record("download", r.Download, "grouping", cur, v); err != nil {
				return err
			}
		}
	}
	if r.Scope == "" {
		return nil
	}
	b, _ := json.Marshal(scopeSource{Scope: r.Scope, AlbumID: album})
	v := Str(string(b))
	var id int64
	err := e.tx.QueryRowContext(e.ctx, `SELECT id FROM album_scopes WHERE scope = ?`, r.Scope).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) { // a new row, its number never used before (AUTOINCREMENT)
		res, err := e.tx.ExecContext(e.ctx, `INSERT INTO album_scopes (scope, album_id, artist, derived, title, created_at)
			SELECT ?, id, album_artist, 0, title, ? FROM albums WHERE id = ?`, r.Scope, db.Now(), album)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		return e.record("scope", id, "source", nil, v)
	}
	if err != nil {
		return err
	}
	cur, _, err := e.current("scope", id, "source")
	if err != nil {
		return err
	}
	if same(cur, v) {
		return nil
	}
	if err := e.write("scope", id, "source", v); err != nil {
		return err
	}
	return e.record("scope", id, "source", cur, v)
}

// arrange moves the plan's entries into its target (made when new), names its sections, and points
// the albums it empties at it. summary, when given, names the edit.
func (e *editor) arrange(p *Plan, summary string) (int64, error) {
	ctx, tx := e.ctx, e.tx
	if summary != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE edit_groups SET summary = ? WHERE id = ?`, summary, e.group); err != nil {
			return 0, err
		}
	}
	target := p.Target.ID
	if target == 0 {
		// A hand-made album has no origin; the albums emptied into it point at it, so imports with
		// their tags come here.
		var cover any
		for _, m := range p.Moves {
			var c sql.NullInt64
			if tx.QueryRowContext(ctx, `SELECT cover_id FROM albums WHERE id = ?`, m.From.ID).Scan(&c) == nil && c.Valid {
				cover = c.Int64
				break
			}
		}
		now := db.Now()
		r, err := tx.ExecContext(ctx, `INSERT INTO albums (title, album_artist, cover_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
			p.Target.Title, p.Target.AlbumArtist, cover, now, now)
		if err != nil {
			return 0, err
		}
		target, _ = r.LastInsertId()
		e.markIndex("album", target)
	}
	for _, m := range p.Moves {
		var album, asset int64
		err := tx.QueryRowContext(ctx, `SELECT album_id, asset_id FROM album_entries WHERE id = ?`, m.EntryID).Scan(&album, &asset)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("%w: entry %d", ErrNotFound, m.EntryID)
		}
		if err != nil {
			return 0, err
		}
		if m.Duplicate {
			if err := e.removeEntry(m.EntryID); err != nil {
				return 0, err
			}
			continue
		}
		for _, c := range []Change{{"entry", m.EntryID, "album_id", num(target)}, {"entry", m.EntryID, "disc_no", num(int64(m.Disc))},
			{"entry", m.EntryID, "track_no", num(int64(m.Track))}} {
			if err := e.set(c); err != nil {
				return 0, err
			}
		}
	}
	if len(p.Adds) > 0 {
		have := map[int64]bool{}
		rows, err := tx.QueryContext(ctx, `SELECT asset_id FROM album_entries WHERE album_id = ?`, target)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var a int64
			if rows.Scan(&a) == nil {
				have[a] = true
			}
		}
		rows.Close()
		for _, a := range p.Adds {
			var asset int64
			err := tx.QueryRowContext(ctx, `SELECT ta.asset_id FROM track_assets ta JOIN assets x ON x.id = ta.asset_id WHERE ta.track_id = ?
				ORDER BY x.state = 'verified' DESC, ta.asset_id LIMIT 1`, a.TrackID).Scan(&asset)
			if errors.Is(err, sql.ErrNoRows) {
				return 0, fmt.Errorf("%w: song %d", ErrNotFound, a.TrackID)
			}
			if err != nil {
				return 0, err
			}
			if have[asset] {
				continue
			}
			have[asset] = true
			if err := e.addEntry(target, a.TrackID, asset, a.Disc, a.Track); err != nil {
				return 0, err
			}
		}
	}
	if len(p.Sections) > 0 {
		if err := e.nameSections(target, p.Sections); err != nil {
			return 0, err
		}
	}
	for _, a := range p.Emptied {
		var left int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM album_entries WHERE album_id = ?`, a.ID).Scan(&left); err != nil {
			return 0, err
		}
		if left == 0 && a.ID != target {
			if err := e.set(Change{"album", a.ID, "merged_into", num(target)}); err != nil {
				return 0, err
			}
			// A favorite album stays a favorite under its new home.
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO favorite_albums (album_id, created_at)
				SELECT ?, created_at FROM favorite_albums WHERE album_id = ?`, target, a.ID); err != nil {
				return 0, err
			}
		}
	}
	return target, nil
}

// AlbumFields are the details several albums get at once; nil leaves a field as it is. The songs'
// own artists stay.
type AlbumFields struct {
	AlbumArtist *string `json:"album_artist"`
	Date        *string `json:"date"`
	Edition     *string `json:"edition"`
}

// EditAlbums gives albums the same details, as one edit.
func (s *Store) EditAlbums(ctx context.Context, ids []int64, f AlbumFields) (int64, error) {
	if len(ids) == 0 {
		return 0, invalid("choose albums")
	}
	return s.edit(ctx, SourceUser, fmt.Sprintf("修改 %d 張專輯的資訊", len(ids)), func(e *editor) error {
		for _, id := range ids {
			if _, err := brief(ctx, e.tx, id); err != nil {
				return err
			}
		}
		for _, id := range ids {
			for _, c := range []struct {
				field string
				v     *string
			}{{"album_artist", f.AlbumArtist}, {"date", f.Date}, {"edition", f.Edition}} {
				if c.v != nil {
					if err := e.set(Change{"album", id, c.field, c.v}); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}

// RemoveAlbums takes every entry off the albums, as one edit; the songs stay as standalone songs.
func (s *Store) RemoveAlbums(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, invalid("choose albums")
	}
	return s.edit(ctx, SourceUser, fmt.Sprintf("移除 %d 張專輯", len(ids)), func(e *editor) error {
		for _, id := range ids {
			if _, err := brief(ctx, e.tx, id); err != nil {
				return err
			}
		}
		for _, id := range ids {
			entries, err := e.entries(id)
			if err != nil {
				return err
			}
			for _, en := range entries {
				if err := e.removeEntry(en.ID); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// TrackFields are the details several songs get at once; nil leaves a field as it is.
type TrackFields struct {
	Artist *string `json:"artist"`
	Kind   *string `json:"kind"`
}

// EditTracks gives songs the same details, as one edit.
func (s *Store) EditTracks(ctx context.Context, ids []int64, f TrackFields) (int64, error) {
	if len(ids) == 0 {
		return 0, invalid("choose songs")
	}
	return s.edit(ctx, SourceUser, fmt.Sprintf("修改 %d 首歌的資訊", len(ids)), func(e *editor) error {
		for _, id := range ids {
			var one int
			if err := e.tx.QueryRowContext(ctx, `SELECT 1 FROM tracks WHERE id = ?`, id).Scan(&one); err != nil {
				return fmt.Errorf("%w: song %d", ErrNotFound, id)
			}
		}
		for _, id := range ids {
			if err := e.track(id, TrackEdit{Artist: f.Artist, Kind: f.Kind}); err != nil {
				return err
			}
		}
		return nil
	})
}

// PlaceRequest puts songs into an album: songs of the library (each joins with its file, as a new
// entry) or entries moved from where they are.
type PlaceRequest struct {
	Tracks      []int64 `json:"tracks"`
	Entries     []int64 `json:"entries"`
	Album       int64   `json:"album"` // 0: a new album
	Title       string  `json:"title"`
	AlbumArtist string  `json:"album_artist"`
	Disc        int     `json:"disc"`    // the disc (section) they go to; 0: a new one after the last
	Section     string  `json:"section"` // its name; "" leaves the name as it is
}

// PlaceSongs carries out a PlaceRequest as one edit: the songs go after the disc's last track, in
// the order given; a song whose file the album has already is not added twice.
func (s *Store) PlaceSongs(ctx context.Context, req PlaceRequest) (albumID, group int64, err error) {
	if len(req.Tracks)+len(req.Entries) == 0 {
		return 0, 0, invalid("choose songs")
	}
	group, err = s.edit(ctx, SourceUser, "", func(e *editor) error {
		tx := e.tx
		target := AlbumBrief{ID: req.Album, Title: strings.TrimSpace(req.Title), AlbumArtist: strings.TrimSpace(req.AlbumArtist)}
		if req.Album != 0 {
			if target, err = brief(ctx, tx, req.Album); err != nil {
				return err
			}
		} else if target.Title == "" {
			return invalid("a new album needs a title")
		}
		p := &Plan{Target: target, Sections: map[int]string{}}
		disc := req.Disc
		if disc == 0 || disc > 99 {
			if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(disc_no), 0) + 1 FROM album_entries WHERE album_id = ?`, req.Album).Scan(&disc); err != nil {
				return err
			}
			if req.Album == 0 {
				disc = 1
			}
		}
		if disc > 99 {
			return invalid("more than 99 sections")
		}
		if req.Section = strings.TrimSpace(req.Section); req.Section != "" {
			p.Sections[disc] = req.Section
		}
		var track int
		tx.QueryRowContext(ctx, `SELECT coalesce(max(track_no), 0) FROM album_entries WHERE album_id = ? AND disc_no = ?`, req.Album, disc).Scan(&track)
		for _, id := range req.Entries {
			m := Move{EntryID: id}
			var from int64
			err := tx.QueryRowContext(ctx, `SELECT e.album_id, e.track_id, e.asset_id, t.title, t.artist FROM album_entries e
				JOIN tracks t ON t.id = e.track_id WHERE e.id = ?`, id).Scan(&from, &m.TrackID, &m.assetID, &m.Title, &m.Artist)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: entry %d", ErrNotFound, id)
			}
			if err != nil {
				return err
			}
			m.From = AlbumBrief{ID: from}
			track++
			m.Disc, m.Track = disc, track
			p.Moves = append(p.Moves, m)
		}
		if err := s.markDuplicates(ctx, tx, p); err != nil {
			return err
		}
		albumID, err = e.arrange(p, fmt.Sprintf("將 %d 首歌放進「%s」", len(req.Tracks)+len(req.Entries), target.Title))
		if err != nil {
			return err
		}
		// Songs of the library join with their file, as new entries.
		have := map[int64]bool{}
		rows, err := tx.QueryContext(ctx, `SELECT asset_id FROM album_entries WHERE album_id = ?`, albumID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var a int64
			if rows.Scan(&a) == nil {
				have[a] = true
			}
		}
		rows.Close()
		for _, id := range req.Tracks {
			var asset int64
			err := tx.QueryRowContext(ctx, `SELECT ta.asset_id FROM track_assets ta JOIN assets a ON a.id = ta.asset_id WHERE ta.track_id = ?
				ORDER BY a.state = 'verified' DESC, ta.asset_id LIMIT 1`, id).Scan(&asset)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: song %d", ErrNotFound, id)
			}
			if err != nil {
				return err
			}
			if have[asset] {
				continue
			}
			have[asset] = true
			track++
			if err := e.addEntry(albumID, id, asset, disc, track); err != nil {
				return err
			}
		}
		return nil
	})
	return albumID, group, err
}

// addEntry makes an entry and records it, so undo takes it away.
func (e *editor) addEntry(albumID, trackID, assetID int64, disc, track int) error {
	r, err := e.tx.ExecContext(e.ctx, `INSERT INTO album_entries (album_id, track_id, asset_id, disc_no, track_no, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, albumID, trackID, assetID, disc, track, db.Now())
	if err != nil {
		return err
	}
	id, _ := r.LastInsertId()
	row, _, err := e.current("entry", id, "row")
	if err != nil {
		return err
	}
	return e.record("entry", id, "row", nil, row)
}

// SetFavorites marks songs and albums favorites, or not, at once; every ID must exist.
func (s *Store) SetFavorites(ctx context.Context, tracks, albums []int64, on bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := db.Now()
	for _, set := range []struct {
		ids                []int64
		table, col, ref, w string
	}{{tracks, "favorite_tracks", "track_id", "tracks", "song"}, {albums, "favorite_albums", "album_id", "albums", "album"}} {
		for _, id := range set.ids {
			var one int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM `+set.ref+` WHERE id = ?`, id).Scan(&one); err != nil {
				return fmt.Errorf("%w: %s %d", ErrNotFound, set.w, id)
			}
			q := `DELETE FROM ` + set.table + ` WHERE ` + set.col + ` = ?`
			args := []any{id}
			if on {
				q, args = `INSERT INTO `+set.table+` (`+set.col+`, created_at) VALUES (?, ?) ON CONFLICT DO NOTHING`, []any{id, now}
			}
			if _, err := tx.ExecContext(ctx, q, args...); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
