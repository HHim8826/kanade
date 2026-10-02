package media

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"strings"
)

const (
	flacStreamInfo    = 0
	flacVorbisComment = 4
	flacPicture       = 6
	maxMetadataBlock  = 1 << 24 // FLAC block lengths are 24-bit
)

// probeFLAC reads metadata blocks starting at pos (just after "fLaC").
func probeFLAC(r io.ReaderAt, size, pos int64, info *Info) error {
	info.Format, info.Codec = "flac", "flac"
	for {
		h, err := readAt(r, pos, 4)
		if err != nil {
			return err
		}
		last, typ := h[0]&0x80 != 0, h[0]&0x7f
		length := int64(h[1])<<16 | int64(h[2])<<8 | int64(h[3])
		pos += 4
		if pos+length > size {
			return errors.New("flac: metadata block runs past the end of the file")
		}
		switch typ {
		case flacStreamInfo:
			b, err := readAt(r, pos, int(length))
			if err != nil {
				return err
			}
			if len(b) < 34 {
				return errors.New("flac: short STREAMINFO")
			}
			x := binary.BigEndian.Uint64(b[10:18])
			info.SampleRate = int(x >> 44)
			info.Channels = int((x>>41)&7) + 1
			info.BitDepth = int((x>>36)&31) + 1
			if total := int64(x & (1<<36 - 1)); total > 0 && info.SampleRate > 0 {
				info.DurationMS = total * 1000 / int64(info.SampleRate)
			}
			if md5 := hex.EncodeToString(b[18:34]); strings.Trim(md5, "0") != "" {
				info.AudioMD5 = md5
			}
		case flacVorbisComment:
			b, err := readAt(r, pos, int(length))
			if err != nil {
				return err
			}
			if err := parseVorbisComment(b, info); err != nil {
				return err
			}
		case flacPicture:
			if length <= maxPicture {
				b, err := readAt(r, pos, int(length))
				if err != nil {
					return err
				}
				if p, err := parsePictureBlock(b); err == nil {
					info.setPicture(p)
				}
			}
		}
		pos += length
		if last {
			break
		}
	}
	if info.SampleRate == 0 {
		return errors.New("flac: no STREAMINFO")
	}
	return nil
}

// parseVorbisComment reads the vendor string and "KEY=value" list used by FLAC, Vorbis and Opus.
func parseVorbisComment(b []byte, info *Info) error {
	le := binary.LittleEndian
	short := errors.New("vorbis comment: truncated")
	if len(b) < 8 {
		return short
	}
	vendorLen := int(le.Uint32(b))
	q := 4 + vendorLen
	if vendorLen < 0 || q+4 > len(b) {
		return short
	}
	count := int(le.Uint32(b[q:]))
	q += 4
	for i := 0; i < count; i++ {
		if q+4 > len(b) {
			return short
		}
		n := int(le.Uint32(b[q:]))
		q += 4
		if n < 0 || q+n > len(b) {
			return short
		}
		field := b[q : q+n]
		q += n
		eq := strings.IndexByte(string(field), '=')
		if eq <= 0 {
			continue
		}
		key := strings.ToUpper(string(field[:eq]))
		raw := field[eq+1:]
		if key == "METADATA_BLOCK_PICTURE" {
			if data, err := base64.StdEncoding.DecodeString(string(raw)); err == nil {
				if p, err := parsePictureBlock(data); err == nil {
					info.setPicture(p)
				}
			}
			continue
		}
		value, enc := UTF8OrLegacy(raw)
		info.addLegacy(key, raw, enc)
		info.addRaw(key, value)
		info.Tags.applyField(key, value)
	}
	return nil
}
