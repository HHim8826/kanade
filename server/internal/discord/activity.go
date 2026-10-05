package discord

import (
	"time"
	"unicode/utf8"

	"github.com/HHim8826/kanade/server/internal/presence"
)

// text fits Discord's activity fields: 2 to 128 characters.
func text(s string) string {
	if utf8.RuneCountInString(s) > 128 {
		s = string([]rune(s)[:127]) + "…"
	}
	if utf8.RuneCountInString(s) < 2 {
		s += " ♪"
	}
	return s
}

// Activity is the Discord activity for what is shown (nil: nothing): listening to the song, by its
// artist, from its album, and where in it as of now. image, when set, is the picture (an asset of
// the application or an https address); the album is then its caption, else it follows the artist.
func Activity(s presence.Shown, image string, now time.Time) map[string]any {
	if s.State != presence.Playing && s.State != presence.Paused || s.Title == "" {
		return nil
	}
	a := map[string]any{"type": 2, "name": "Kanade", "details": text(s.Title)}
	state := s.Artist
	if image == "" && s.Album != "" {
		if state != "" {
			state += " · "
		}
		state += s.Album
	}
	if s.State == presence.Paused {
		if state != "" {
			state += " · "
		}
		state += "已暫停"
	}
	if state != "" {
		a["state"] = text(state)
	}
	if image != "" {
		assets := map[string]any{"large_image": image}
		if s.Album != "" {
			assets["large_text"] = text(s.Album)
		}
		a["assets"] = assets
	}
	if s.State == presence.Playing && s.DurationMS > 0 {
		start := now.UnixMilli() - s.PositionMS
		a["timestamps"] = map[string]any{"start": start, "end": start + s.DurationMS}
	}
	return a
}
