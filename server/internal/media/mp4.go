package media

import (
	"encoding/binary"
	"errors"
	"io"
	"strings"
)

const maxMoov = 64 << 20

type box struct {
	typ  string
	body []byte
}

// boxes splits a byte slice into its child boxes.
func boxes(b []byte) []box {
	var out []box
	for len(b) >= 8 {
		size := int64(binary.BigEndian.Uint32(b))
		typ := string(b[4:8])
		hdr := int64(8)
		switch size {
		case 0:
			size = int64(len(b))
		case 1:
			if len(b) < 16 {
				return out
			}
			size = int64(binary.BigEndian.Uint64(b[8:]))
			hdr = 16
		}
		if size < hdr || size > int64(len(b)) {
			return out
		}
		out = append(out, box{typ: typ, body: b[hdr:size]})
		b = b[size:]
	}
	return out
}

func child(b []byte, typ string) []byte {
	for _, bx := range boxes(b) {
		if bx.typ == typ {
			return bx.body
		}
	}
	return nil
}

func probeMP4(r io.ReaderAt, size int64, info *Info) error {
	info.Format = "m4a"
	// Walk top-level boxes to find moov without reading mdat.
	var moov []byte
	for pos := int64(0); pos+8 <= size; {
		h, err := readAt(r, pos, 16)
		if err != nil {
			h, err = readAt(r, pos, 8)
			if err != nil {
				return err
			}
		}
		n := int64(binary.BigEndian.Uint32(h))
		hdr := int64(8)
		if n == 1 && len(h) >= 16 {
			n, hdr = int64(binary.BigEndian.Uint64(h[8:])), 16
		} else if n == 0 {
			n = size - pos
		}
		if n < hdr || pos+n > size {
			return errors.New("mp4: malformed box")
		}
		if string(h[4:8]) == "moov" {
			if n-hdr > maxMoov {
				return errors.New("mp4: moov box too large")
			}
			if moov, err = readAt(r, pos+hdr, int(n-hdr)); err != nil {
				return err
			}
			break
		}
		pos += n
	}
	if moov == nil {
		return errors.New("mp4: no moov box")
	}

	for _, trak := range boxes(moov) {
		if trak.typ != "trak" {
			continue
		}
		mdia := child(trak.body, "mdia")
		if hdlr := child(mdia, "hdlr"); len(hdlr) < 12 || string(hdlr[8:12]) != "soun" {
			continue
		}
		if mdhd := child(mdia, "mdhd"); len(mdhd) > 0 {
			if scale, dur := mediaHeaderTimes(mdhd); scale > 0 {
				info.DurationMS = int64(dur * 1000 / scale)
			}
		}
		stsd := child(child(child(mdia, "minf"), "stbl"), "stsd")
		if len(stsd) < 8 {
			continue
		}
		entries := boxes(stsd[8:])
		if len(entries) == 0 {
			continue
		}
		e := entries[0]
		switch e.typ {
		case "mp4a":
			info.Codec = "aac"
		case "alac":
			info.Codec = "alac"
		case "fLaC":
			info.Codec = "flac"
		case "Opus":
			info.Codec = "opus"
		default:
			info.Codec = strings.TrimSpace(e.typ)
		}
		if len(e.body) >= 28 { // AudioSampleEntry
			info.Channels = int(binary.BigEndian.Uint16(e.body[16:]))
			info.BitDepth = int(binary.BigEndian.Uint16(e.body[18:]))
			info.SampleRate = int(binary.BigEndian.Uint32(e.body[24:]) >> 16)
		}
		if info.Codec == "aac" {
			info.BitDepth = 0 // lossy: the field is a nominal 16
		}
		break
	}
	if info.Codec == "" {
		return errors.New("mp4: no audio track")
	}
	if info.DurationMS == 0 {
		if mvhd := child(moov, "mvhd"); len(mvhd) > 0 {
			if scale, dur := mediaHeaderTimes(mvhd); scale > 0 {
				info.DurationMS = int64(dur * 1000 / scale)
			}
		}
	}

	if meta := child(child(moov, "udta"), "meta"); len(meta) > 4 {
		parseIlst(child(meta[4:], "ilst"), info) // meta is a full box: skip version/flags
	}
	return nil
}

// mediaHeaderTimes reads timescale and duration from mvhd or mdhd (version 0 or 1).
func mediaHeaderTimes(b []byte) (scale, dur uint64) {
	be := binary.BigEndian
	if len(b) >= 32 && b[0] == 1 {
		return uint64(be.Uint32(b[20:])), be.Uint64(b[24:])
	}
	if len(b) >= 20 {
		return uint64(be.Uint32(b[12:])), uint64(be.Uint32(b[16:]))
	}
	return 0, 0
}

var ilstNames = map[string]string{
	"\xa9nam": "TITLE", "\xa9ART": "ARTIST", "\xa9alb": "ALBUM", "aART": "ALBUMARTIST",
	"\xa9day": "DATE", "\xa9gen": "GENRE",
}

func parseIlst(ilst []byte, info *Info) {
	for _, item := range boxes(ilst) {
		data := child(item.body, "data")
		if len(data) < 8 {
			continue
		}
		kind, value := binary.BigEndian.Uint32(data)&0xFFFFFF, data[8:]
		key := item.typ
		switch {
		case item.typ == "----": // freeform: mean, name, data
			name := child(item.body, "name")
			if len(name) <= 4 {
				continue
			}
			key = "----:" + strings.ToUpper(string(name[4:]))
			v, enc := UTF8OrLegacy(value)
			info.addLegacy(key, value, enc)
			info.addRaw(key, v)
			info.Tags.applyField(strings.TrimPrefix(key, "----:"), v)
		case item.typ == "trkn" || item.typ == "disk":
			if len(value) >= 6 {
				n, total := int(binary.BigEndian.Uint16(value[2:])), int(binary.BigEndian.Uint16(value[4:]))
				if item.typ == "trkn" {
					info.Tags.TrackNo, info.Tags.TrackTotal = n, total
				} else {
					info.Tags.DiscNo, info.Tags.DiscTotal = n, total
				}
			}
		case item.typ == "covr":
			mime := "image/jpeg"
			if kind == 14 {
				mime = "image/png"
			}
			info.setPicture(Picture{Type: 3, MIME: mime, Data: append([]byte(nil), value...)})
		default:
			name, ok := ilstNames[item.typ]
			if !ok || kind != 1 { // 1 = UTF-8 text
				continue
			}
			v, enc := UTF8OrLegacy(value)
			info.addLegacy(key, value, enc)
			info.addRaw(key, v)
			info.Tags.applyField(name, v)
		}
	}
}
