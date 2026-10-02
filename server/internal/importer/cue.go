package importer

import (
	"strconv"
	"strings"
)

// A CUE sheet (decision D2 §3). Times are in CD frames, 75 per second; -1 means absent.
type cueSheet struct {
	Title, Performer, Date, Genre, Catalog string
	Disc                                   int
	Files                                  []cueFile
}

type cueFile struct {
	Name   string
	Tracks []cueTrack
}

type cueTrack struct {
	Number           int
	Title, Performer string
	Index0, Index1   int64
}

// parseCue reads a CUE sheet already decoded to text. It is lenient: unknown commands are skipped.
func parseCue(text string) *cueSheet {
	c := &cueSheet{}
	var file *cueFile
	var track *cueTrack
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		cmd, rest := word(strings.TrimSpace(strings.TrimPrefix(line, "\ufeff")))
		switch strings.ToUpper(cmd) {
		case "REM":
			key, val := word(rest)
			val = unquote(val)
			switch strings.ToUpper(key) {
			case "DATE":
				c.Date = val
			case "GENRE":
				c.Genre = val
			case "DISCNUMBER":
				c.Disc, _ = strconv.Atoi(val)
			}
		case "CATALOG":
			c.Catalog = unquote(rest)
		case "TITLE":
			if track != nil {
				track.Title = unquote(rest)
			} else {
				c.Title = unquote(rest)
			}
		case "PERFORMER":
			if track != nil {
				track.Performer = unquote(rest)
			} else {
				c.Performer = unquote(rest)
			}
		case "FILE":
			name := rest
			if i := strings.LastIndexByte(name, ' '); i > 0 && !strings.HasSuffix(name, `"`) {
				name = name[:i] // FILE name.wav WAVE
			} else if i := strings.LastIndexByte(name, '"'); i > 0 {
				name = name[:i+1]
			}
			c.Files = append(c.Files, cueFile{Name: unquote(name)})
			file, track = &c.Files[len(c.Files)-1], nil
		case "TRACK":
			if file == nil {
				continue
			}
			n, kind := word(rest)
			if !strings.EqualFold(strings.TrimSpace(kind), "AUDIO") {
				track = nil
				continue
			}
			num, _ := strconv.Atoi(n)
			file.Tracks = append(file.Tracks, cueTrack{Number: num, Index0: -1, Index1: -1})
			track = &file.Tracks[len(file.Tracks)-1]
		case "INDEX":
			if track == nil {
				continue
			}
			n, at := word(rest)
			frames, ok := cueTime(strings.TrimSpace(at))
			if !ok {
				continue
			}
			switch n {
			case "00", "0":
				track.Index0 = frames
			case "01", "1":
				track.Index1 = frames
			}
		}
	}
	return c
}

func word(s string) (string, string) {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i], strings.TrimSpace(s[i+1:])
	}
	return s, ""
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	return strings.TrimSpace(s)
}

// cueTime parses mm:ss:ff into frames.
func cueTime(s string) (int64, bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, false
	}
	var n [3]int64
	for i, p := range parts {
		v, err := strconv.ParseInt(p, 10, 64)
		if err != nil || v < 0 {
			return 0, false
		}
		n[i] = v
	}
	if n[1] >= 60 || n[2] >= 75 {
		return 0, false
	}
	return (n[0]*60+n[1])*75 + n[2], true
}

// htoaFrames is the shortest stretch before track 1 that counts as a hidden track (HTOA) rather
// than a gap: four seconds.
const htoaFrames = 4 * 75

// piece is one track of a split, in samples of the image.
type piece struct {
	Number     int // 0 for a hidden track before track 1
	Title      string
	Performer  string
	Start, End int64 // End 0: to the end of the image
}

// pieces lays out the tracks of one image file (D2 §3): each runs from its INDEX 01 to the next
// track's INDEX 01, so a pregap (INDEX 00) belongs to the track before it; audio before track 1's
// INDEX 01 is a track 0 when it is long enough to be a hidden track, else part of track 1.
func (f cueFile) pieces(sampleRate int) []piece {
	perFrame := int64(sampleRate) / 75
	var out []piece
	for i, t := range f.Tracks {
		if t.Index1 < 0 {
			return nil // a track without a start: not something to cut by
		}
		start := t.Index1 * perFrame
		if i == 0 {
			if t.Index1 >= htoaFrames {
				out = append(out, piece{Number: 0, Title: "Hidden track", End: start})
			} else {
				start = 0
			}
		}
		out = append(out, piece{Number: t.Number, Title: t.Title, Performer: t.Performer, Start: start})
		if n := len(out); n > 1 {
			out[n-2].End = start
		}
	}
	for i := 1; i < len(out); i++ {
		if out[i].Start <= out[i-1].Start {
			return nil // times out of order
		}
	}
	return out
}
