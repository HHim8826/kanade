package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/HHim8826/kanade/server/internal/db"
)

// Credits (decision D13): who sang a song, as which character, and who wrote, composed and arranged
// it, each name an artist of its own. A track keeps the names as written, one string per role;
// credits are worked out from them, and from the user's word on how a string divides into names
// (artist_splits), never guessed silently: Split only proposes.

// Roles, in the order a song's credits are told.
const (
	RoleArtist   = "artist"   // sang or played it, as credited (a character, a unit)
	RoleVoice    = "voice"    // voiced the character credited at the same position
	RoleComposer = "composer" // wrote the music
	RoleLyricist = "lyricist" // wrote the words
	RoleArranger = "arranger"
)

// roleColumns are the track's columns holding each role's names as written.
var roleColumns = []struct{ role, column string }{
	{RoleArtist, "artist"}, {RoleComposer, "composer"}, {RoleLyricist, "lyricist"}, {RoleArranger, "arranger"},
}

// Artist kinds, when known.
const (
	KindPerson    = "person"
	KindCharacter = "character"
)

// Part is one name a credit divides into: an artist, or a character and the one who voiced it.
type Part struct {
	Name string `json:"name"`
	CV   string `json:"cv,omitempty"`
}

var (
	// "Name (CV: Someone)", "Name（CV：Someone）", "Name [CV.Someone]".
	cvPart = regexp.MustCompile(`^(.+?)\s*[(（\[［]\s*(?i:cv|c\.v\.|ｃｖ)\s*[:：.．]?\s*(.+?)\s*[)）\]］]$`)
	// "Name (feat. Someone)": the one featured is a name of its own.
	featPart = regexp.MustCompile(`^(.+?)\s*[(（\[［]\s*(?i:feat\.?|ft\.?|featuring)\s+(.+?)\s*[)）\]］]$`)
	// Between names, outside brackets.
	between = regexp.MustCompile(`\s+(?i:feat\.?|ft\.?|featuring|with|vs\.?)\s+|\s+[x×&＆/／]\s+|[、，,;；／/×＆&]`)
)

var opens, closes = "(（[［「『【〈《", ")）]］」』】〉》"

// Split proposes how a credited string divides into names: at the separators outside brackets
// ("、", "/", " & ", " feat. ", " x " …), a featured name in brackets apart, and a character's voice
// "(CV: …)" with it. A string it does not divide is one part. It is a proposal: "Earth, Wind & Fire"
// divides here too, and the user says it does not.
func Split(s string) []Part {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []Part
	seen := map[string]bool{}
	var add func(string)
	add = func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if m := featPart.FindStringSubmatch(name); m != nil {
			add(m[1])
			for _, p := range Split(m[2]) {
				if !seen[p.Name] {
					seen[p.Name] = true
					out = append(out, p)
				}
			}
			return
		}
		p := Part{Name: name}
		if m := cvPart.FindStringSubmatch(name); m != nil {
			p = Part{Name: strings.TrimSpace(m[1]), CV: strings.TrimSpace(m[2])}
		}
		if !seen[p.Name] {
			seen[p.Name] = true
			out = append(out, p)
		}
	}
	for _, piece := range splitOutside(s) {
		add(piece)
	}
	return out
}

// splitOutside cuts s at the separators that are outside brackets.
func splitOutside(s string) []string {
	// What is inside brackets is masked, byte for byte, so the separators there are not seen.
	var b strings.Builder
	depth := 0
	for i := 0; i < len(s); {
		r, w := utf8.DecodeRuneInString(s[i:])
		switch {
		case strings.ContainsRune(opens, r):
			depth++
		case strings.ContainsRune(closes, r) && depth > 0:
			depth--
		case depth > 0:
			b.WriteString(strings.Repeat("_", w))
			i += w
			continue
		}
		b.WriteString(s[i : i+w])
		i += w
	}
	m := b.String()
	var out []string
	at := 0
	for _, loc := range between.FindAllStringIndex(m, -1) {
		out = append(out, s[at:loc[0]])
		at = loc[1]
	}
	return append(out, s[at:])
}

// divides says whether Split has something to propose for s: more than one name, or a voice.
func divides(parts []Part) bool {
	return len(parts) > 1 || len(parts) == 1 && parts[0].CV != ""
}

// partsOf is how a credited string divides by the user's word, else one name as written.
func partsOf(ctx context.Context, q querier, name string) ([]Part, error) {
	if name = strings.TrimSpace(name); name == "" {
		return nil, nil
	}
	var parts sql.NullString
	err := q.QueryRowContext(ctx, `SELECT parts FROM artist_splits WHERE name = ?`, name).Scan(&parts)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if parts.Valid {
		var out []Part
		if json.Unmarshal([]byte(parts.String), &out) == nil && len(out) > 0 {
			return out, nil
		}
	}
	return []Part{{Name: name}}, nil
}

// credit links a track to the artists its names are, role by role (D13); a character's voice is a
// credit of its own, at the character's position.
func credit(ctx context.Context, tx *sql.Tx, trackID int64) error {
	var names [4]string
	err := tx.QueryRowContext(ctx, `SELECT artist, composer, lyricist, arranger FROM tracks WHERE id = ?`, trackID).
		Scan(&names[0], &names[1], &names[2], &names[3])
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM credits WHERE track_id = ?`, trackID); err != nil {
		return err
	}
	put := func(role string, pos int, name, as string) (int64, error) {
		id, err := upsertArtist(ctx, tx, name)
		if err != nil {
			return 0, err
		}
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO credits (track_id, role, position, artist_id, credited_as) VALUES (?, ?, ?, ?, ?)`,
			trackID, role, pos, id, as)
		return id, err
	}
	for i, rc := range roleColumns {
		parts, err := partsOf(ctx, tx, names[i])
		if err != nil {
			return err
		}
		for pos, p := range parts {
			id, err := put(rc.role, pos, p.Name, "")
			if err != nil {
				return err
			}
			if p.CV == "" || rc.role != RoleArtist {
				continue
			}
			cv, err := put(RoleVoice, pos, p.CV, p.Name)
			if err != nil {
				return err
			}
			if err := knownAs(ctx, tx, id, KindCharacter); err != nil {
				return err
			}
			if err := knownAs(ctx, tx, cv, KindPerson); err != nil {
				return err
			}
		}
	}
	return nil
}

// knownAs says what an artist is, unless that was known already.
func knownAs(ctx context.Context, tx *sql.Tx, id int64, kind string) error {
	_, err := tx.ExecContext(ctx, `UPDATE artists SET kind = ? WHERE id = ? AND kind = ''`, kind, id)
	return err
}

// creditTracksNamed works the credits out again for every track with name in a role, after the
// user's word on how it divides changed.
func creditTracksNamed(ctx context.Context, tx *sql.Tx, name string) error {
	ids, err := idsTx(ctx, tx, `SELECT id FROM tracks WHERE artist = ?1 OR composer = ?1 OR lyricist = ?1 OR arranger = ?1`, name)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := credit(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

// checkParts validates parts as a user gives them, and puts them in stored form: JSON, [] for a
// name that is one artist.
func checkParts(v string) (*string, error) {
	var parts []Part
	if err := json.Unmarshal([]byte(v), &parts); err != nil {
		return nil, invalid("parts: a list of names")
	}
	if len(parts) > 30 {
		return nil, invalid("parts: at most 30 names")
	}
	clean := []Part{}
	seen := map[string]bool{}
	for _, p := range parts {
		p.Name, p.CV = strings.TrimSpace(p.Name), strings.TrimSpace(p.CV)
		if p.Name == "" || utf8.RuneCountInString(p.Name) > 200 || utf8.RuneCountInString(p.CV) > 200 {
			return nil, invalid("parts: each name has 1 to 200 characters")
		}
		if !seen[p.Name] {
			seen[p.Name] = true
			clean = append(clean, p)
		}
	}
	b, _ := json.Marshal(clean)
	return Str(string(b)), nil
}

// SplitName is a credited string, how it divides (proposed or said) and how much it is used.
type SplitName struct {
	Name   string   `json:"name"`
	Parts  []Part   `json:"parts"` // [] for one artist
	Tracks int      `json:"tracks"`
	Roles  []string `json:"roles"` // the roles it is credited in
}

// Splits are the credited strings Split divides that the user has not said anything about yet
// (proposals, the most used first), and those the user has (decided).
func (s *Store) Splits(ctx context.Context) (proposals, decided []SplitName, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT u.name, u.role, u.n, sp.parts FROM (
			SELECT artist AS name, 'artist' AS role, count(*) AS n FROM tracks WHERE artist != '' GROUP BY artist
			UNION ALL SELECT composer, 'composer', count(*) FROM tracks WHERE composer != '' GROUP BY composer
			UNION ALL SELECT lyricist, 'lyricist', count(*) FROM tracks WHERE lyricist != '' GROUP BY lyricist
			UNION ALL SELECT arranger, 'arranger', count(*) FROM tracks WHERE arranger != '' GROUP BY arranger
		) u LEFT JOIN artist_splits sp ON sp.name = u.name`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	byName := map[string]*SplitName{}
	said := map[string]bool{}
	var order []string
	for rows.Next() {
		var name, role string
		var n int
		var parts sql.NullString
		if err := rows.Scan(&name, &role, &n, &parts); err != nil {
			return nil, nil, err
		}
		sn := byName[name]
		if sn == nil {
			sn = &SplitName{Name: name}
			if parts.Valid {
				said[name] = true
				if json.Unmarshal([]byte(parts.String), &sn.Parts) != nil || sn.Parts == nil {
					sn.Parts = []Part{}
				}
			} else if sn.Parts = Split(name); !divides(sn.Parts) {
				continue
			}
			byName[name] = sn
			order = append(order, name)
		}
		sn.Tracks += n
		sn.Roles = append(sn.Roles, role)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	proposals, decided = []SplitName{}, []SplitName{}
	for _, name := range order {
		if said[name] {
			decided = append(decided, *byName[name])
		} else {
			proposals = append(proposals, *byName[name])
		}
	}
	for _, list := range [][]SplitName{proposals, decided} {
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].Tracks != list[j].Tracks {
				return list[i].Tracks > list[j].Tracks
			}
			return list[i].Name < list[j].Name
		})
	}
	return proposals, decided, nil
}

// SplitDecision is the user's word on a name: Parts is how it divides, [] for one artist, nil to
// take back what was said.
type SplitDecision struct {
	Name  string  `json:"name"`
	Parts *[]Part `json:"parts"`
}

// DecideSplits stores the user's word on how names divide, as one change that can be undone; the
// credits of every track with those names follow, and those imported later.
func (s *Store) DecideSplits(ctx context.Context, list []SplitDecision) (int64, error) {
	if len(list) == 0 || len(list) > 500 {
		return 0, invalid("1 to 500 names at a time")
	}
	summary := "拆分歌手名稱"
	if len(list) == 1 {
		summary = "拆分「" + list[0].Name + "」"
	}
	return s.edit(ctx, SourceUser, summary, func(e *editor) error {
		for _, d := range list {
			name := strings.TrimSpace(d.Name)
			if name == "" {
				return invalid("a name to divide")
			}
			now := db.Now()
			if _, err := e.tx.ExecContext(ctx, `INSERT OR IGNORE INTO artist_splits (name, created_at, updated_at) VALUES (?, ?, ?)`,
				name, now, now); err != nil {
				return err
			}
			var id int64
			if err := e.tx.QueryRowContext(ctx, `SELECT id FROM artist_splits WHERE name = ?`, name).Scan(&id); err != nil {
				return err
			}
			var v *string
			if d.Parts != nil {
				b, _ := json.Marshal(*d.Parts)
				v = Str(string(b))
			}
			if err := e.set(Change{Target: "split", ID: id, Field: "parts", Value: v}); err != nil {
				return err
			}
		}
		return nil
	})
}

// Credit is one of a song's credits, for its page.
type Credit struct {
	Role       string `json:"role"`
	ArtistID   int64  `json:"artist_id"`
	Name       string `json:"name"`
	CreditedAs string `json:"credited_as,omitempty"`
}

// trackCredits are a track's credits in order: by role, then position.
func trackCredits(ctx context.Context, q querier, trackID int64) ([]Credit, error) {
	rows, err := q.QueryContext(ctx, `SELECT c.role, c.artist_id, ar.name, c.credited_as FROM credits c JOIN artists ar ON ar.id = c.artist_id
		WHERE c.track_id = ? ORDER BY CASE c.role WHEN 'artist' THEN 0 WHEN 'voice' THEN 1 WHEN 'composer' THEN 2
			WHEN 'lyricist' THEN 3 ELSE 4 END, c.position`, trackID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Credit{}
	for rows.Next() {
		var c Credit
		if err := rows.Scan(&c.Role, &c.ArtistID, &c.Name, &c.CreditedAs); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RoleNames are a track's names as written for the roles beside its artist.
type RoleNames struct{ Composer, Lyricist, Arranger string }

// creditNamesVersion is the settings key "credit_names" once FillRoleNames ran.
const creditNamesVersion = "1"

// RoleNamesFilled says whether FillRoleNames already ran.
func (s *Store) RoleNamesFilled(ctx context.Context) bool {
	var v string
	s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'credit_names'`).Scan(&v)
	return v == creditNamesVersion
}

// FillRoleNames gives the tracks imported before Kanade kept them their composer, lyricist and
// arranger, as read from their files then (D13); a name already there stays. Their credits and
// search follow. Once: it is noted in the settings.
func (s *Store) FillRoleNames(ctx context.Context, names map[int64]RoleNames) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, n := range names {
		r, err := tx.ExecContext(ctx, `UPDATE tracks SET composer = CASE composer WHEN '' THEN ?1 ELSE composer END,
				lyricist = CASE lyricist WHEN '' THEN ?2 ELSE lyricist END, arranger = CASE arranger WHEN '' THEN ?3 ELSE arranger END
			WHERE id = ?4 AND (composer = '' AND ?1 != '' OR lyricist = '' AND ?2 != '' OR arranger = '' AND ?3 != '')`,
			n.Composer, n.Lyricist, n.Arranger, id)
		if err != nil {
			return err
		}
		if k, _ := r.RowsAffected(); k == 0 {
			continue
		}
		if err := credit(ctx, tx, id); err != nil {
			return err
		}
		if err := reindex(ctx, tx, "track", id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('credit_names', ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, creditNamesVersion); err != nil {
		return err
	}
	return tx.Commit()
}

// renameInSplits renames an artist in the user's word on how names divide: as one of the names, and
// as the voice of a character.
func renameInSplits(e *editor, old, name string) error {
	rows, err := e.tx.QueryContext(e.ctx, `SELECT id, parts FROM artist_splits WHERE parts IS NOT NULL`)
	if err != nil {
		return err
	}
	changed := map[int64][]Part{}
	for rows.Next() {
		var id int64
		var v string
		if err := rows.Scan(&id, &v); err != nil {
			rows.Close()
			return err
		}
		var parts []Part
		if json.Unmarshal([]byte(v), &parts) != nil {
			continue
		}
		hit := false
		for i := range parts {
			if parts[i].Name == old {
				parts[i].Name, hit = name, true
			}
			if parts[i].CV == old {
				parts[i].CV, hit = name, true
			}
		}
		if hit {
			changed[id] = parts
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for id, parts := range changed {
		b, _ := json.Marshal(parts)
		if err := e.set(Change{Target: "split", ID: id, Field: "parts", Value: Str(string(b))}); err != nil {
			return err
		}
	}
	return nil
}
