package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"unicode/utf16"
)

func syncsafe(b []byte) int64 {
	return int64(b[0]&0x7f)<<21 | int64(b[1]&0x7f)<<14 | int64(b[2]&0x7f)<<7 | int64(b[3]&0x7f)
}

// id3Size returns the total length of an ID3v2 tag (header, body and footer).
func id3Size(h []byte) (int64, error) {
	if len(h) < 10 || string(h[:3]) != "ID3" {
		return 0, errors.New("id3: no tag")
	}
	n := 10 + syncsafe(h[6:10])
	if h[5]&0x10 != 0 { // footer present (v2.4)
		n += 10
	}
	return n, nil
}

// removeUnsync reverses ID3 unsynchronisation (0xFF 0x00 → 0xFF).
func removeUnsync(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte{0xFF, 0x00}, []byte{0xFF})
}

// id3FieldNames maps text frames onto the Vorbis-style names Tags.applyField understands.
var id3FieldNames = map[string]string{
	"TIT2": "TITLE", "TT2": "TITLE",
	"TPE1": "ARTIST", "TP1": "ARTIST",
	"TALB": "ALBUM", "TAL": "ALBUM",
	"TPE2": "ALBUMARTIST", "TP2": "ALBUMARTIST",
	"TDRC": "DATE", "TYER": "DATE", "TYE": "DATE", "TDOR": "ORIGINALDATE",
	"TCON": "GENRE", "TCO": "GENRE",
	"TRCK": "TRACKNUMBER", "TRK": "TRACKNUMBER",
	"TPOS": "DISCNUMBER", "TPA": "DISCNUMBER",
}

// decodeID3Text decodes a text payload whose first byte is the ID3 encoding marker.
// ISO-8859-1 payloads go through DecodeLegacy because Japanese taggers often put CP932 there.
func decodeID3Text(b []byte) (values []string, raw []byte, enc string) {
	if len(b) == 0 {
		return nil, nil, EncUTF8
	}
	marker, body := b[0], b[1:]
	var s string
	switch marker {
	case 0:
		s, enc = DecodeLegacy(body)
		raw = body
	case 1, 2:
		s, enc = decodeUTF16(body, marker == 2), EncUTF8
	case 3:
		s, enc = string(trimNUL(body)), EncUTF8
	default:
		s, enc = DecodeLegacy(body)
		raw = body
	}
	// ID3v2.4 separates multiple values with NUL.
	for _, v := range strings.Split(strings.TrimRight(s, "\x00"), "\x00") {
		if v = strings.TrimSpace(v); v != "" {
			values = append(values, v)
		}
	}
	return values, raw, enc
}

func decodeUTF16(b []byte, bigEndianNoBOM bool) string {
	var order binary.ByteOrder = binary.LittleEndian
	if bigEndianNoBOM {
		order = binary.BigEndian
	}
	var units []uint16
	for i := 0; i+1 < len(b); i += 2 {
		u := order.Uint16(b[i:])
		switch {
		case u == 0xFEFF && !bigEndianNoBOM:
			order = binary.LittleEndian // BOM as read little-endian means LE
			continue
		case u == 0xFFFE && !bigEndianNoBOM:
			order = binary.BigEndian
			continue
		}
		units = append(units, u)
	}
	// A list of values each with its own BOM is common; NULs separate them.
	return string(utf16.Decode(units))
}

// skipEncodedString skips a NUL-terminated string in the given ID3 encoding.
func skipEncodedString(b []byte, marker byte) int {
	if marker == 1 || marker == 2 {
		for i := 0; i+1 < len(b); i += 2 {
			if b[i] == 0 && b[i+1] == 0 {
				return i + 2
			}
		}
		return len(b)
	}
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return i + 1
	}
	return len(b)
}

// parseID3v2 reads text frames and pictures from the tag at the start of the file.
// It returns the tag length so callers can find the audio after it.
func parseID3v2(r io.ReaderAt, size int64, info *Info) (int64, error) {
	h, err := readAt(r, 0, 10)
	if err != nil {
		return 0, err
	}
	total, err := id3Size(h)
	if err != nil {
		return 0, err
	}
	if total > size {
		return 0, errors.New("id3: tag runs past the end of the file")
	}
	ver, flags := h[3], h[5]
	if ver < 2 || ver > 4 {
		return total, nil // unknown version: skip it, the audio still plays
	}
	body, err := readAt(r, 10, int(syncsafe(h[6:10])))
	if err != nil {
		return 0, err
	}
	if flags&0x80 != 0 && ver < 4 { // whole-tag unsynchronisation (v2.2/v2.3)
		body = removeUnsync(body)
	}
	if flags&0x40 != 0 && ver == 3 && len(body) >= 4 { // extended header
		ext := int(binary.BigEndian.Uint32(body)) + 4
		if ext <= len(body) {
			body = body[ext:]
		}
	} else if flags&0x40 != 0 && ver == 4 && len(body) >= 4 {
		ext := int(syncsafe(body))
		if ext <= len(body) {
			body = body[ext:]
		}
	}

	idLen, hdrLen := 4, 10
	if ver == 2 {
		idLen, hdrLen = 3, 6
	}
	for q := 0; q+hdrLen <= len(body); {
		id := string(body[q : q+idLen])
		if id[0] == 0 {
			break // padding
		}
		var n int
		var fflags uint16
		switch ver {
		case 2:
			n = int(body[q+3])<<16 | int(body[q+4])<<8 | int(body[q+5])
		case 3:
			n = int(binary.BigEndian.Uint32(body[q+4:]))
			fflags = binary.BigEndian.Uint16(body[q+8:])
		case 4:
			n = int(syncsafe(body[q+4:]))
			fflags = binary.BigEndian.Uint16(body[q+8:])
		}
		q += hdrLen
		if n < 0 || q+n > len(body) {
			break
		}
		data := body[q : q+n]
		q += n
		if ver == 3 && fflags&0x00C0 != 0 { // compressed or encrypted
			continue
		}
		if ver == 4 {
			if fflags&0x000C != 0 { // compressed or encrypted
				continue
			}
			if fflags&0x0001 != 0 && len(data) >= 4 { // data length indicator
				data = data[4:]
			}
			if fflags&0x0002 != 0 {
				data = removeUnsync(data)
			}
		}
		switch {
		case id == "TXXX" || id == "TXX":
			if len(data) < 2 {
				continue
			}
			descEnd := 1 + skipEncodedString(data[1:], data[0])
			desc, _, _ := decodeID3Text(append([]byte{data[0]}, data[1:descEnd]...))
			vals, raw, enc := decodeID3Text(append([]byte{data[0]}, data[descEnd:]...))
			if len(desc) == 0 {
				continue
			}
			key := strings.ToUpper(desc[0])
			info.addLegacy("TXXX:"+key, raw, enc)
			for _, v := range vals {
				info.addRaw("TXXX:"+key, v)
				info.Tags.applyField(key, v) // e.g. TXXX:ALBUMARTIST, TXXX:DISCTOTAL
			}
		case strings.HasPrefix(id, "T"):
			vals, raw, enc := decodeID3Text(data)
			info.addLegacy(id, raw, enc)
			for _, v := range vals {
				info.addRaw(id, v)
				if name, ok := id3FieldNames[id]; ok {
					info.Tags.applyField(name, v)
				}
			}
		case id == "APIC" && len(data) > 2:
			marker := data[0]
			mimeEnd := bytes.IndexByte(data[1:], 0)
			if mimeEnd < 0 || 1+mimeEnd+2 > len(data) {
				continue
			}
			mime := string(data[1 : 1+mimeEnd])
			p := 1 + mimeEnd + 1
			ptype := int(data[p])
			p++
			p += skipEncodedString(data[p:], marker)
			if p < len(data) {
				info.setPicture(Picture{Type: ptype, MIME: mime, Data: append([]byte(nil), data[p:]...)})
			}
		case id == "PIC" && len(data) > 5:
			marker, format, ptype := data[0], strings.ToLower(string(data[1:4])), int(data[4])
			p := 5 + skipEncodedString(data[5:], marker)
			mime := "image/" + map[string]string{"jpg": "jpeg", "png": "png"}[format]
			if p < len(data) {
				info.setPicture(Picture{Type: ptype, MIME: mime, Data: append([]byte(nil), data[p:]...)})
			}
		}
	}
	return total, nil
}

// parseID3v1 reads the 128-byte tag at the end of the file. It is used only when there is
// no ID3v2 tag. Its fields have no declared encoding, so they go through DecodeLegacy.
func parseID3v1(r io.ReaderAt, size int64, info *Info) bool {
	if size < 128 {
		return false
	}
	b, err := readAt(r, size-128, 128)
	if err != nil || string(b[:3]) != "TAG" {
		return false
	}
	field := func(key, name string, raw []byte) {
		raw = bytes.TrimRight(raw, " \x00")
		if len(raw) == 0 {
			return
		}
		v, enc := DecodeLegacy(raw)
		info.addLegacy(key, raw, enc)
		info.addRaw(key, v)
		info.Tags.applyField(name, v)
	}
	field("ID3v1:TITLE", "TITLE", b[3:33])
	field("ID3v1:ARTIST", "ARTIST", b[33:63])
	field("ID3v1:ALBUM", "ALBUM", b[63:93])
	field("ID3v1:YEAR", "DATE", b[93:97])
	if b[125] == 0 && b[126] != 0 && info.Tags.TrackNo == 0 { // ID3v1.1 track number
		info.Tags.TrackNo = int(b[126])
	}
	return true
}
