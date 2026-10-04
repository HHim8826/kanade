package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/HHim8826/kanade/server/internal/db"
)

// Smart playlists (review #96): rules that pick songs from the library whenever they are asked —
// the user's categories, albums, artists, kind, favorites, how often and when songs were played —
// in an order, up to a number of songs or minutes. Played as they are, or on and on: every pick
// asks the rules again. Plays count as the listening statistics count them (D9).

// Rules is what a smart playlist picks and in what order.
type Rules struct {
	Match      string      `json:"match"` // all (every condition) or any (one of them)
	Conditions []Condition `json:"conditions"`
	// Sort: random, least_recent (longest unheard first, never played first), recent_added,
	// most_played (over the last SortDays days; 0: ever) or album (album, disc, track).
	Sort     string `json:"sort"`
	SortDays int    `json:"sort_days,omitempty"`
	Limit    int    `json:"limit,omitempty"`   // songs at most (0: no limit)
	Minutes  int    `json:"minutes,omitempty"` // total length at most (0: no limit)
}

// Condition is one rule. Fields and their operators:
//
//	category, album   is / not   IDs     on an album in one of these (not: on none)
//	artist            contains / not  Value
//	kind              is         Value   music or spoken
//	favorite          is / not
//	plays             gte / lte  N       counted plays ever
//	played_within     is / not   N       a play in the last N days (not: none, or never played)
//	never_played      is / not
//	finished          is / not           played to the end at least once
type Condition struct {
	Field string  `json:"field"`
	Op    string  `json:"op"`
	Value string  `json:"value,omitempty"`
	IDs   []int64 `json:"ids,omitempty"`
	N     int     `json:"n,omitempty"`
}

const maxSmart = 1000 // songs a smart playlist lists at most

// where turns the rules into an SQL condition on tracks t.
func (r *Rules) where(now int64) (string, []any, error) {
	if len(r.Conditions) > 20 {
		return "", nil, invalid("at most 20 conditions")
	}
	var parts []string
	var args []any
	for _, c := range r.Conditions {
		not := c.Op == "not"
		if c.Op != "is" && c.Op != "not" && !(c.Field == "artist" && c.Op == "contains") && !(c.Field == "plays" && (c.Op == "gte" || c.Op == "lte")) {
			return "", nil, invalid("%s cannot be %q", c.Field, c.Op)
		}
		var q string
		switch c.Field {
		case "category", "album":
			if len(c.IDs) == 0 || len(c.IDs) > 200 {
				return "", nil, invalid("choose from 1 to 200 %ss", c.Field)
			}
			in := `?` + strings.Repeat(", ?", len(c.IDs)-1)
			if c.Field == "category" {
				q = `EXISTS (SELECT 1 FROM album_entries e JOIN albums al ON al.id = e.album_id JOIN album_categories ac ON ac.album_id = al.id
					WHERE e.track_id = t.id AND ac.category_id IN (` + in + `))`
			} else {
				q = `EXISTS (SELECT 1 FROM album_entries e WHERE e.track_id = t.id AND e.album_id IN (` + in + `))`
			}
			for _, id := range c.IDs {
				args = append(args, id)
			}
		case "artist":
			v := strings.TrimSpace(c.Value)
			if v == "" {
				return "", nil, invalid("give an artist")
			}
			q = `t.artist LIKE ? ESCAPE '\'`
			args = append(args, "%"+strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(v)+"%")
		case "kind":
			if c.Value != "music" && c.Value != "spoken" {
				return "", nil, invalid("kind is music or spoken")
			}
			q = `t.kind = ?`
			args = append(args, c.Value)
		case "favorite":
			q = `EXISTS (SELECT 1 FROM favorite_tracks f WHERE f.track_id = t.id)`
		case "plays":
			if c.N < 0 {
				return "", nil, invalid("plays is a number")
			}
			op := ">="
			if c.Op == "lte" {
				op = "<="
			}
			q = `(SELECT count(*) FROM plays p WHERE p.track_id = t.id AND p.counted = 1) ` + op + ` ?`
			args = append(args, c.N)
		case "played_within":
			if c.N < 1 || c.N > 3650 {
				return "", nil, invalid("days are from 1 to 3650")
			}
			q = `EXISTS (SELECT 1 FROM plays p WHERE p.track_id = t.id AND p.updated_at >= ?)`
			args = append(args, now-int64(c.N)*24*3600*1000)
		case "never_played":
			q = `NOT EXISTS (SELECT 1 FROM plays p WHERE p.track_id = t.id)`
		case "finished":
			q = `EXISTS (SELECT 1 FROM plays p WHERE p.track_id = t.id AND p.finished = 1)`
		default:
			return "", nil, invalid("unknown field %q", c.Field)
		}
		if not {
			q = `NOT ` + q
		}
		parts = append(parts, q)
	}
	if len(parts) == 0 {
		return "1 = 1", args, nil
	}
	join := " AND "
	if r.Match == "any" {
		join = " OR "
	}
	return "(" + strings.Join(parts, join) + ")", args, nil
}

func (r *Rules) order(now int64) (string, []any, error) {
	switch r.Sort {
	case "", "random":
		return `random()`, nil, nil
	case "least_recent":
		return `(SELECT max(p.updated_at) FROM plays p WHERE p.track_id = t.id) ASC NULLS FIRST, t.id`, nil, nil
	case "recent_added":
		return `t.created_at DESC, t.id DESC`, nil, nil
	case "most_played":
		if r.SortDays > 0 {
			return `(SELECT count(*) FROM plays p WHERE p.track_id = t.id AND p.counted = 1 AND p.updated_at >= ?) DESC, t.id`,
				[]any{now - int64(r.SortDays)*24*3600*1000}, nil
		}
		return `(SELECT count(*) FROM plays p WHERE p.track_id = t.id AND p.counted = 1) DESC, t.id`, nil, nil
	case "album":
		return `fa.album_artist, fa.title, fa.id, (SELECT e.disc_no FROM album_entries e WHERE e.track_id = t.id AND e.album_id = fa.id LIMIT 1),
			(SELECT e.track_no FROM album_entries e WHERE e.track_id = t.id AND e.album_id = fa.id LIMIT 1), t.title`, nil, nil
	}
	return "", nil, invalid("unknown order %q", r.Sort)
}

// Check says what is wrong with rules, before they are kept.
func (r *Rules) Check() error {
	if r.Match != "" && r.Match != "all" && r.Match != "any" {
		return invalid("match is all or any")
	}
	if r.Limit < 0 || r.Limit > maxSmart || r.Minutes < 0 || r.Minutes > 100_000 || r.SortDays < 0 || r.SortDays > 3650 {
		return invalid("limits are out of range")
	}
	if _, _, err := r.where(0); err != nil {
		return err
	}
	_, _, err := r.order(0)
	return err
}

// SmartTracks are the songs the rules pick, in their order and within their limits, leaving out
// those in exclude (the songs just played); matches is how many songs fit the conditions.
func (s *Store) SmartTracks(ctx context.Context, r Rules, exclude []int64, upTo int) (tracks []TrackItem, matches int, err error) {
	now := db.Now()
	cond, args, err := r.where(now)
	if err != nil {
		return nil, 0, err
	}
	order, orderArgs, err := r.order(now)
	if err != nil {
		return nil, 0, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM (`+trackSQL+` WHERE `+cond+`)`, args...).Scan(&matches); err != nil {
		return nil, 0, err
	}
	limit := maxSmart
	if r.Limit > 0 {
		limit = r.Limit
	}
	if upTo > 0 && upTo < limit {
		limit = upTo
	}
	q := trackSQL + ` WHERE ` + cond
	all := append([]any{}, args...)
	if len(exclude) > 0 {
		q += ` AND t.id NOT IN (?` + strings.Repeat(", ?", len(exclude)-1) + `)`
		for _, id := range exclude {
			all = append(all, id)
		}
	}
	all = append(all, orderArgs...)
	all = append(all, limit)
	tracks, err = scanTracks(s.db.QueryContext(ctx, q+` ORDER BY `+order+` LIMIT ?`, all...))
	if err != nil {
		return nil, 0, err
	}
	if r.Minutes > 0 { // whole songs while they fit
		var total int64
		for i, t := range tracks {
			total += t.Asset.DurationMS
			if total > int64(r.Minutes)*60_000 {
				tracks = tracks[:max(i, 1)]
				break
			}
		}
	}
	return tracks, matches, nil
}

// SmartNext picks the next n songs of rules played on and on, leaving out the songs of not (the
// latest first: those queued to come, the one playing, then those played). When the rules match
// only songs of not, it goes round again rather than stop (review #105).
func (s *Store) SmartNext(ctx context.Context, r Rules, not []int64, n int) ([]TrackItem, error) {
	r.Limit, r.Minutes = 0, 0 // going on: the limits are for the list
	tracks, matches, err := s.SmartTracks(ctx, r, not, n)
	if err != nil || len(tracks) > 0 || matches == 0 || len(not) == 0 {
		return tracks, err
	}
	all, _, err := s.SmartTracks(ctx, r, nil, 0) // the rules match no more songs than not has
	if err != nil {
		return nil, err
	}
	byID := map[int64]TrackItem{}
	var left []int64
	for _, t := range all {
		byID[t.ID] = t
		left = append(left, t.ID)
	}
	for _, id := range roundAgain(left, not) {
		if len(tracks) == n {
			break
		}
		tracks = append(tracks, byID[id])
	}
	return tracks, nil
}

// roundAgain orders the songs of ids that are in not for going round again: not lists the latest
// first, so those played longest ago come first, and the latest one (queued to come, or playing)
// only when it is all there is (reviews #72, #105).
func roundAgain(ids, not []int64) []int64 {
	pos := map[int64]int{}
	for i, id := range not {
		if _, ok := pos[id]; !ok {
			pos[id] = i
		}
	}
	var out []int64
	latest := false
	for _, id := range ids {
		switch i, ok := pos[id]; {
		case ok && i == 0:
			latest = true
		case ok:
			out = append(out, id)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return pos[out[i]] > pos[out[j]] })
	if len(out) == 0 && latest {
		out = []int64{not[0]}
	}
	return out
}

func int64s(ids []any) []int64 {
	out := make([]int64, len(ids))
	for i, id := range ids {
		out[i] = id.(int64)
	}
	return out
}

func encodeRules(r Rules) (string, error) {
	if err := r.Check(); err != nil {
		return "", err
	}
	if r.Match == "" {
		r.Match = "all"
	}
	if r.Sort == "" {
		r.Sort = "random"
	}
	b, err := json.Marshal(r)
	if utf8.RuneCount(b) > 20_000 {
		return "", invalid("rules are too long")
	}
	return string(b), err
}

// CreateSmartPlaylist keeps rules as a playlist.
func (s *Store) CreateSmartPlaylist(ctx context.Context, name, description string, r Rules) (int64, error) {
	name, err := cleanName(name)
	if err != nil {
		return 0, err
	}
	raw, err := encodeRules(r)
	if err != nil {
		return 0, err
	}
	now := db.Now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO playlists (name, description, rules, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		name, strings.TrimSpace(description), raw, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetPlaylistRules changes a smart playlist's rules.
func (s *Store) SetPlaylistRules(ctx context.Context, id int64, r Rules) error {
	raw, err := encodeRules(r)
	if err != nil {
		return err
	}
	return s.affectOne(s.db.ExecContext(ctx, `UPDATE playlists SET rules = ?, updated_at = ? WHERE id = ? AND rules IS NOT NULL`, raw, db.Now(), id))
}

// PlaylistRules are a playlist's rules, nil for an ordinary one.
func (s *Store) PlaylistRules(ctx context.Context, id int64) (*Rules, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT rules FROM playlists WHERE id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil || !raw.Valid {
		return nil, err
	}
	var r Rules
	if err := json.Unmarshal([]byte(raw.String), &r); err != nil {
		return nil, fmt.Errorf("playlist %d rules: %w", id, err)
	}
	return &r, nil
}
