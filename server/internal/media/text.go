package media

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	xunicode "golang.org/x/text/encoding/unicode"
)

// Text encodings reported in Info.Encoding (decision D2).
const (
	EncUTF8    = "utf-8"
	EncCP932   = "cp932"
	EncCP1252  = "cp1252"
	EncUnknown = ""
)

// DecodeLegacy decodes bytes that a tag format declared as ISO-8859-1 (or does not declare
// at all, like ID3v1). In practice Japanese files often hold UTF-8 or CP932 there.
// Order: valid UTF-8 → plausible CP932 → CP1252.
func DecodeLegacy(b []byte) (string, string) {
	b = trimNUL(b)
	if utf8.Valid(b) {
		return string(b), EncUTF8
	}
	if s, err := japanese.ShiftJIS.NewDecoder().Bytes(b); err == nil && plausibleJapanese(string(s)) {
		return string(s), EncCP932
	}
	s, _ := charmap.Windows1252.NewDecoder().Bytes(b)
	return string(s), EncCP1252
}

// plausibleJapanese rejects CP932 decodings that are most likely accidents: replacement
// characters, or text made mostly of scattered half-width katakana (CP1252's 0xB7 "·"
// decodes to "ｷ", as seen in the library survey).
func plausibleJapanese(s string) bool {
	if strings.ContainsRune(s, utf8.RuneError) {
		return false
	}
	var wide, halfKana int
	for _, r := range s {
		switch {
		case r >= 0xFF61 && r <= 0xFF9F:
			halfKana++
		case unicode.In(r, unicode.Hiragana, unicode.Katakana, unicode.Han) || (r >= 0x3000 && r <= 0x303F) || (r >= 0xFF01 && r <= 0xFF5E):
			wide++
		}
	}
	return wide > 0 && halfKana <= wide/2
}

// DecodeText decodes a whole text file (LRC, CUE, LOG, M3U) by decision D2: a BOM decides
// (EAC logs are often UTF-16LE); otherwise valid UTF-8, then plausible CP932, then CP1252.
func DecodeText(b []byte) (string, string) {
	var dec interface{ Bytes([]byte) ([]byte, error) }
	switch {
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		return string(b[3:]), EncUTF8
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
		dec = xunicode.UTF16(xunicode.LittleEndian, xunicode.ExpectBOM).NewDecoder()
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		dec = xunicode.UTF16(xunicode.BigEndian, xunicode.ExpectBOM).NewDecoder()
	}
	if dec != nil {
		if out, err := dec.Bytes(b); err == nil {
			return string(out), "utf-16"
		}
	}
	return DecodeLegacy(b)
}

// UTF8OrLegacy is for formats that require UTF-8 (Vorbis comments, MP4) but sometimes do not get it.
func UTF8OrLegacy(b []byte) (string, string) {
	if utf8.Valid(b) {
		return string(trimNUL(b)), EncUTF8
	}
	return DecodeLegacy(b)
}

func trimNUL(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return b
}

// LooksLost reports text whose characters were replaced by placeholders before it reached us,
// e.g. a CUE sheet saved in CP1252 by EAC: the Japanese is gone and cannot be recovered.
func LooksLost(s string) bool {
	var letters, placeholders int
	for _, r := range s {
		switch {
		case r == '.' || r == '?' || r == '·':
			placeholders++
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			letters++
		}
	}
	return placeholders >= 4 && placeholders > letters
}
