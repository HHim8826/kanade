package rss

import (
	"strings"

	"github.com/HHim8826/kanade/server/internal/library"
)

// Rules are lines of words. A line matches a title when every word of it is in the title, compared
// the way library search compares (width, case, kana, spaces and punctuation do not matter); a set
// of rules matches when any line does.
func rulesMatch(rules, title string) bool {
	t := library.Normalize(title)
	for _, line := range strings.Split(rules, "\n") {
		words := strings.Fields(line)
		if len(words) == 0 {
			continue
		}
		all := true
		for _, w := range words {
			if n := library.Normalize(w); n != "" && !strings.Contains(t, n) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func hasRules(rules string) bool { return strings.TrimSpace(rules) != "" }

// Match classifies a title: "excluded" when an exclude rule matches, "included" when an include
// rule matches, "" otherwise (including when there are no include rules).
func Match(include, exclude, title string) string {
	switch {
	case hasRules(exclude) && rulesMatch(exclude, title):
		return "excluded"
	case hasRules(include) && rulesMatch(include, title):
		return "included"
	}
	return ""
}
