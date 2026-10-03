package library

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Named sections of an album (review #82): a disc can have a name ("Episode 1"), shown instead of
// "Disc N". In the edit log an album's sections are one value, a JSON object of disc numbers to
// names ("{}" for none), so a change to them is undone as a whole.

func sectionNames(ctx context.Context, q querier, albumID int64) (map[int]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT disc_no, name FROM album_sections WHERE album_id = ? ORDER BY disc_no`, albumID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var d int
		var n string
		if err := rows.Scan(&d, &n); err != nil {
			return nil, err
		}
		out[d] = n
	}
	return out, rows.Err()
}

// encodeSections is the stored form: keys in disc order, empty names left out.
func encodeSections(names map[int]string) string {
	clean := map[string]string{}
	for d, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			clean[strconv.Itoa(d)] = n
		}
	}
	b, _ := json.Marshal(clean)
	return string(b)
}

func decodeSections(v string) (map[int]string, error) {
	out := map[int]string{}
	if strings.TrimSpace(v) == "" {
		return out, nil
	}
	var raw map[string]string
	if err := json.Unmarshal([]byte(v), &raw); err != nil {
		return nil, invalid("sections: a JSON object of disc numbers to names")
	}
	for k, n := range raw {
		d, err := strconv.Atoi(k)
		if err != nil || d < 1 || d > 99 {
			return nil, invalid("sections: discs are 1 to 99")
		}
		if n = strings.TrimSpace(n); n != "" {
			if utf8.RuneCountInString(n) > 200 {
				return nil, invalid("a section name is too long")
			}
			out[d] = n
		}
	}
	return out, nil
}

// SetSections names an album's discs as one edit; a disc not given keeps its name, an empty name
// takes it away.
func (s *Store) SetSections(ctx context.Context, albumID int64, names map[int]string) (int64, error) {
	title, err := s.name(ctx, "album", albumID)
	if err != nil {
		return 0, err
	}
	return s.edit(ctx, SourceUser, "編輯「"+title+"」的區段名稱", func(e *editor) error {
		return e.nameSections(albumID, names)
	})
}

// nameSections changes some discs' names (an empty one takes the name away).
func (e *editor) nameSections(albumID int64, names map[int]string) error {
	cur, err := sectionNames(e.ctx, e.tx, albumID)
	if err != nil {
		return err
	}
	for d, n := range names {
		if n = strings.TrimSpace(n); n == "" {
			delete(cur, d)
		} else {
			cur[d] = n
		}
	}
	return e.set(Change{"album", albumID, "sections", Str(encodeSections(cur))})
}

// NameSectionIfNone names a disc of an album unless it has a name (an import of a collection, not
// an edit: the user's names stay).
func (s *Store) NameSectionIfNone(ctx context.Context, albumID int64, disc int, name string) error {
	if name = strings.TrimSpace(name); name == "" || disc < 1 || disc > 99 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO album_sections (album_id, disc_no, name) VALUES (?, ?, ?)`, albumID, disc, name)
	return err
}
