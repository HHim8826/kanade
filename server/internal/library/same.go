package library

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/HHim8826/kanade/server/internal/db"
)

// One artist under two names (D13): a song tagged with the romanized name, another with the name in
// Japanese, the same name spelled two ways. The user's word that one name is the other artist is
// kept with the words on how names divide (artist_splits: the name, divided into the one other
// name), so it holds wherever the name is credited, and for songs imported later; the songs' tags
// say what they said. SameArtists proposes pairs that look alike; the user says.

// MergeArtists says the artist drop is keep: every credit of drop's name goes to keep, and keep
// takes drop's other names and drop's own as one more, so searching either finds it. One change in
// the edit log, undone as one.
func (s *Store) MergeArtists(ctx context.Context, dropID, keepID int64) (int64, error) {
	if dropID == keepID {
		return 0, invalid("two different artists")
	}
	drop, err := s.name(ctx, "artist", dropID)
	if err != nil {
		return 0, err
	}
	keep, err := s.name(ctx, "artist", keepID)
	if err != nil {
		return 0, err
	}
	if to, err := sameAs(ctx, s.db, keep); err != nil {
		return 0, err
	} else if to == drop {
		return 0, invalid("「%s」已經記為「%s」", keep, drop)
	}
	return s.edit(ctx, SourceUser, fmt.Sprintf("合併歌手：%s → %s", drop, keep), func(e *editor) error {
		now := db.Now()
		if _, err := e.tx.ExecContext(ctx, `INSERT OR IGNORE INTO artist_splits (name, created_at, updated_at) VALUES (?, ?, ?)`,
			drop, now, now); err != nil {
			return err
		}
		var split int64
		if err := e.tx.QueryRowContext(ctx, `SELECT id FROM artist_splits WHERE name = ?`, drop).Scan(&split); err != nil {
			return err
		}
		b, _ := json.Marshal([]Part{{Name: keep}})
		if err := e.set(Change{Target: "split", ID: split, Field: "parts", Value: Str(string(b))}); err != nil {
			return err
		}
		dropAliases, err := aliasNames(ctx, e.tx, "artist", dropID)
		if err != nil {
			return err
		}
		keepAliases, err := aliasNames(ctx, e.tx, "artist", keepID)
		if err != nil {
			return err
		}
		merged := cleanAliases(append(append(keepAliases, dropAliases...), drop))
		return e.set(Change{"artist", keepID, "aliases", Str(strings.Join(merged, "\n"))})
	})
}

// SameArtist is a pair of artists that may be one: why it looks so, and whether that is near
// certain (the same letters, the same reading in full) or a guess (the given name's reading).
type SameArtist struct {
	A      Artist `json:"a"`
	B      Artist `json:"b"`
	Reason string `json:"reason"` // case | order | reading | given
	Sure   bool   `json:"sure"`
}

// SameArtists are the pairs of artists with songs that look like one artist, the surest and most
// used first, leaving out the pairs the user said are two; and how many were left out.
func (s *Store) SameArtists(ctx context.Context) ([]SameArtist, int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ar.id, ar.name, ar.kind, count(DISTINCT c.track_id) FROM artists ar
		JOIN credits c ON c.artist_id = ar.id GROUP BY ar.id`)
	if err != nil {
		return nil, 0, err
	}
	var all []Artist
	for rows.Next() {
		var a Artist
		if err := rows.Scan(&a.ID, &a.Name, &a.Kind, &a.Tracks); err != nil {
			rows.Close()
			return nil, 0, err
		}
		all = append(all, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	two := map[[2]string]bool{}
	rows, err = s.db.QueryContext(ctx, `SELECT a, b FROM artists_not_same`)
	if err != nil {
		return nil, 0, err
	}
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			rows.Close()
			return nil, 0, err
		}
		two[[2]string{a, b}] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	// The names are compared with those that share a key: the letters folded, a reading, a word.
	keys := make([]nameKeys, len(all))
	buckets := map[string][]int{}
	add := func(key string, i int) { buckets[key] = append(buckets[key], i) }
	for i, a := range all {
		k := keysOf(a.Name)
		keys[i] = k
		if k.folded != "" {
			add("f:"+k.folded, i)
		}
		for _, r := range k.readings {
			add("r:"+r, i)
		}
		if len(k.words) == 2 {
			add("g:"+k.words[0], i)
			add("g:"+k.words[1], i)
		}
		if k.given != "" {
			add("g:"+k.given, i)
		}
	}
	out := []SameArtist{}
	left := 0
	tried := map[[2]int]bool{}
	for _, list := range buckets {
		if len(list) > 200 { // a word half the names have says nothing
			continue
		}
		for x := 0; x < len(list); x++ {
			for y := x + 1; y < len(list); y++ {
				i, j := list[x], list[y]
				if tried[[2]int{i, j}] {
					continue
				}
				tried[[2]int{i, j}] = true
				reason := alike(keys[i], keys[j])
				if reason == "" {
					continue
				}
				a, b := all[i], all[j]
				if a.Tracks < b.Tracks || a.Tracks == b.Tracks && a.Name > b.Name { // the one more used first
					a, b = b, a
				}
				if two[pairOf(a.Name, b.Name)] {
					left++
					continue
				}
				out = append(out, SameArtist{A: a, B: b, Reason: reason, Sure: reason != "given"})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sure != out[j].Sure {
			return out[i].Sure
		}
		if n, m := out[i].A.Tracks+out[i].B.Tracks, out[j].A.Tracks+out[j].B.Tracks; n != m {
			return n > m
		}
		if out[i].A.Name != out[j].A.Name { // compared as they are: joined, they took most of the time (#282)
			return out[i].A.Name < out[j].A.Name
		}
		return out[i].B.Name < out[j].B.Name
	})
	return out, left, nil
}

// NotSame keeps the pair of names (artists') from being proposed again.
func (s *Store) NotSame(ctx context.Context, a, b string) error {
	if a = strings.TrimSpace(a); a == "" || strings.TrimSpace(b) == "" || a == strings.TrimSpace(b) {
		return invalid("two different names")
	}
	p := pairOf(a, strings.TrimSpace(b))
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO artists_not_same (a, b, created_at) VALUES (?, ?, ?)`, p[0], p[1], db.Now())
	return err
}

// ProposeSameAgain forgets every pair the user said is two artists.
func (s *Store) ProposeSameAgain(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM artists_not_same`)
	return err
}

func pairOf(a, b string) [2]string {
	if a > b {
		a, b = b, a
	}
	return [2]string{a, b}
}

// ---- telling names alike ----

// nameKeys are the ways a name is compared: its letters folded as search folds them (width, case,
// katakana as hiragana, spaces and dots gone), its reading in romaji when it has one (the name in
// kana, or in Latin letters, either order of its words), and the reading of a given name written in
// kana after a family name in kanji (星野あおい: "aoi").
type nameKeys struct {
	folded   string
	readings []string // full readings
	words    []string // a Latin name's words, read: for the given name
	given    string
}

func keysOf(name string) nameKeys {
	k := nameKeys{folded: Normalize(name)}
	switch {
	case isLatin(name):
		var words []string
		for _, w := range strings.FieldsFunc(name, func(r rune) bool { return !unicode.IsLetter(r) && r != '\'' }) {
			if w = romaji(w); w != "" {
				words = append(words, w)
			}
		}
		if len(words) == 0 {
			break
		}
		k.words = words
		k.readings = []string{strings.Join(words, "")}
		if len(words) > 1 {
			rev := make([]string, len(words))
			for i, w := range words {
				rev[len(words)-1-i] = w
			}
			k.readings = append(k.readings, strings.Join(rev, ""))
		}
	case isKana(name):
		if r := romaji(kanaToRomaji(name)); r != "" {
			k.readings = []string{r}
		}
	default:
		// A family name in kanji (one to four) and a given name in kana (two or more).
		rs := []rune(strings.TrimSpace(norm.NFKC.String(name)))
		i := 0
		for i < len(rs) && unicode.Is(unicode.Han, rs[i]) {
			i++
		}
		rest := strings.TrimSpace(string(rs[i:]))
		if i >= 1 && i <= 4 && len([]rune(rest)) >= 2 && isKana(rest) {
			k.given = romaji(kanaToRomaji(rest))
		}
	}
	return k
}

// alike says why two names look like one artist, or "".
func alike(a, b nameKeys) string {
	if a.folded != "" && a.folded == b.folded {
		return "case"
	}
	for _, x := range a.readings {
		for _, y := range b.readings {
			if x != y || len(x) < 3 {
				continue
			}
			if len(a.words) > 1 && len(b.words) > 1 {
				return "order" // both in Latin letters, the words the other way round
			}
			return "reading"
		}
	}
	for _, p := range [][2]nameKeys{{a, b}, {b, a}} {
		if g := p[0].given; len(g) >= 3 && len(p[1].words) == 2 && (p[1].words[0] == g || p[1].words[1] == g) {
			return "given"
		}
	}
	return ""
}

func isLatin(s string) bool {
	letters := 0
	for _, r := range norm.NFKD.String(s) {
		switch {
		case r < 0x80 && unicode.IsLetter(r):
			letters++
		case unicode.Is(unicode.Mn, r), unicode.IsSpace(r), unicode.IsPunct(r), r < 0x80:
		default:
			return false
		}
	}
	return letters > 0
}

func isKana(s string) bool {
	kana := 0
	for _, r := range norm.NFKC.String(s) {
		switch {
		case unicode.In(r, unicode.Hiragana, unicode.Katakana) || r == 'ー':
			kana++
		case r == '・' || r == '･' || unicode.IsSpace(r):
		default:
			return false
		}
	}
	return kana > 0
}

// romaji puts a reading in Latin letters in one spelling: lower case, no marks or signs, long vowels
// written once (ō, ou, oo: o), n before b, m and p written n.
func romaji(s string) string {
	var b strings.Builder
	for _, r := range norm.NFKD.String(strings.ToLower(s)) {
		if r >= 'a' && r <= 'z' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	for _, x := range [][2]string{{"ou", "o"}, {"oo", "o"}, {"uu", "u"}, {"aa", "a"}, {"ii", "i"}, {"ee", "e"},
		{"mb", "nb"}, {"mp", "np"}, {"mm", "nm"}} {
		out = strings.ReplaceAll(out, x[0], x[1])
	}
	return out
}

// kanaToRomaji reads kana in Hepburn; ー repeats the vowel before it, and っ doubles the consonant
// after it.
func kanaToRomaji(s string) string {
	rs := []rune(norm.NFKC.String(s))
	for i, r := range rs { // katakana as hiragana
		if r >= 'ァ' && r <= 'ヶ' {
			rs[i] = r - 0x60
		}
	}
	var b strings.Builder
	double := false
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		var syl string
		if i+1 < len(rs) {
			if v, ok := kanaPairs[string(rs[i:i+2])]; ok {
				syl = v
				i++
			}
		}
		if syl == "" {
			switch {
			case r == 'っ':
				double = true
				continue
			case r == 'ー':
				if str := b.String(); str != "" {
					b.WriteByte(str[len(str)-1])
				}
				continue
			default:
				syl = kana[r]
			}
		}
		if syl == "" {
			continue
		}
		if double {
			if strings.HasPrefix(syl, "ch") {
				b.WriteByte('t')
			} else {
				b.WriteByte(syl[0])
			}
			double = false
		}
		b.WriteString(syl)
	}
	return b.String()
}

var kana = func() map[rune]string {
	m := map[rune]string{}
	rows := []struct {
		kana string
		read []string
	}{
		{"あいうえお", []string{"a", "i", "u", "e", "o"}},
		{"かきくけこ", []string{"ka", "ki", "ku", "ke", "ko"}},
		{"がぎぐげご", []string{"ga", "gi", "gu", "ge", "go"}},
		{"さしすせそ", []string{"sa", "shi", "su", "se", "so"}},
		{"ざじずぜぞ", []string{"za", "ji", "zu", "ze", "zo"}},
		{"たちつてと", []string{"ta", "chi", "tsu", "te", "to"}},
		{"だぢづでど", []string{"da", "ji", "zu", "de", "do"}},
		{"なにぬねの", []string{"na", "ni", "nu", "ne", "no"}},
		{"はひふへほ", []string{"ha", "hi", "fu", "he", "ho"}},
		{"ばびぶべぼ", []string{"ba", "bi", "bu", "be", "bo"}},
		{"ぱぴぷぺぽ", []string{"pa", "pi", "pu", "pe", "po"}},
		{"まみむめも", []string{"ma", "mi", "mu", "me", "mo"}},
		{"やゆよ", []string{"ya", "yu", "yo"}},
		{"らりるれろ", []string{"ra", "ri", "ru", "re", "ro"}},
		{"わゐゑを", []string{"wa", "i", "e", "o"}},
		{"ぁぃぅぇぉ", []string{"a", "i", "u", "e", "o"}},
		{"ゃゅょゎ", []string{"ya", "yu", "yo", "wa"}},
		{"んゔ", []string{"n", "vu"}},
	}
	for _, row := range rows {
		for i, r := range []rune(row.kana) {
			m[r] = row.read[i]
		}
	}
	return m
}()

// kanaPairs are the syllables written with a small kana: きゃ, しゅ, ティ, ファ…
var kanaPairs = func() map[string]string {
	m := map[string]string{}
	for _, c := range []struct{ kana, cons string }{
		{"き", "ky"}, {"ぎ", "gy"}, {"し", "sh"}, {"じ", "j"}, {"ち", "ch"}, {"ぢ", "j"}, {"に", "ny"}, {"ひ", "hy"},
		{"び", "by"}, {"ぴ", "py"}, {"み", "my"}, {"り", "ry"},
	} {
		m[c.kana+"ゃ"], m[c.kana+"ゅ"], m[c.kana+"ょ"] = c.cons+"a", c.cons+"u", c.cons+"o"
	}
	m["しぇ"], m["じぇ"], m["ちぇ"] = "she", "je", "che"
	for _, c := range []struct{ kana, cons string }{{"ふ", "f"}, {"ゔ", "v"}, {"う", "w"}} {
		m[c.kana+"ぁ"], m[c.kana+"ぃ"], m[c.kana+"ぇ"], m[c.kana+"ぉ"] = c.cons+"a", c.cons+"i", c.cons+"e", c.cons+"o"
	}
	m["てぃ"], m["でぃ"], m["とぅ"], m["どぅ"] = "ti", "di", "tu", "du"
	return m
}()
