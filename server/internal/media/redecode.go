package media

import (
	"bytes"
	"errors"
	"strings"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

// Encodings a user can pick for a batch whose legacy fields were guessed wrong (decision D2 §4).
var encodings = map[string]encoding.Encoding{
	EncCP932:  japanese.ShiftJIS,
	"gbk":     simplifiedchinese.GBK,
	"big5":    traditionalchinese.Big5,
	"latin1":  charmap.ISO8859_1,
	EncCP1252: charmap.Windows1252,
}

// Encodings lists the names Redecode accepts.
func Encodings() []string { return []string{EncCP932, "gbk", "big5", "latin1"} }

// legacyField names the Tags field a Legacy key was read into.
func legacyField(key string) string {
	switch {
	case strings.HasPrefix(key, "TXXX:"):
		return key[5:]
	case strings.HasPrefix(key, "----:"):
		return key[5:]
	case strings.HasPrefix(key, "ID3v1:"):
		if k := key[6:]; k == "YEAR" {
			return "DATE"
		} else {
			return k
		}
	case key == "USLT" || key == "ULT":
		return "LYRICS"
	}
	if n, ok := id3FieldNames[key]; ok {
		return n
	}
	if n, ok := ilstNames[key]; ok {
		return n
	}
	return key // Vorbis comments use the names directly
}

// clearField empties the Tags field applyField would fill for name.
func (t *Tags) clearField(name string) {
	switch strings.ToUpper(name) {
	case "TITLE":
		t.Title = ""
	case "ARTIST":
		t.Artist = ""
	case "ALBUM":
		t.Album = ""
	case "ALBUMARTIST", "ALBUM ARTIST", "ALBUM_ARTIST":
		t.AlbumArtist = ""
	case "DATE", "YEAR", "ORIGINALDATE":
		t.Date = ""
	case "GENRE":
		t.Genre = ""
	case "LYRICS", "UNSYNCEDLYRICS", "UNSYNCED LYRICS":
		t.Lyrics = ""
	}
}

// Redecode decodes the fields kept as raw bytes again with the named encoding, replacing what the
// automatic guess made of them. Fields that were valid UTF-8 (or UTF-16) are not in Legacy and stay.
func (info *Info) Redecode(name string) error {
	enc, ok := encodings[name]
	if !ok {
		return errors.New("unknown encoding " + name)
	}
	if len(info.Legacy) == 0 {
		return nil
	}
	for key := range info.Legacy {
		info.Tags.clearField(legacyField(key))
	}
	for key, raw := range info.Legacy {
		for _, part := range bytes.Split(bytes.TrimRight(raw, "\x00"), []byte{0}) {
			if v, err := enc.NewDecoder().Bytes(part); err == nil {
				info.Tags.applyField(legacyField(key), string(v))
			}
		}
	}
	info.Encoding = name
	return nil
}
