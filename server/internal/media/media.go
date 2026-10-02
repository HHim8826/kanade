// Package media identifies audio files and reads their tags and technical details
// with pure Go parsers. Formats follow decision D2: MP3, AAC (M4A), FLAC, Ogg Vorbis
// and Ogg Opus are playable; other formats are recognized so they can be reported.
package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

var ErrUnknownFormat = errors.New("not a recognized audio format")

type Picture struct {
	Type int    `json:"type"` // ID3/FLAC picture type; 3 is the front cover
	MIME string `json:"mime"`
	Data []byte `json:"-"`
}

type Tags struct {
	Title       string `json:"title,omitempty"`
	Artist      string `json:"artist,omitempty"`
	Album       string `json:"album,omitempty"`
	AlbumArtist string `json:"album_artist,omitempty"`
	Date        string `json:"date,omitempty"`
	Genre       string `json:"genre,omitempty"`
	TrackNo     int    `json:"track_no,omitempty"`
	TrackTotal  int    `json:"track_total,omitempty"`
	DiscNo      int    `json:"disc_no,omitempty"`
	DiscTotal   int    `json:"disc_total,omitempty"`
}

type Info struct {
	Format     string `json:"format"` // flac, mp3, m4a, ogg, opus; or the unsupported format's name
	Codec      string `json:"codec"`
	Playable   bool   `json:"playable"`
	SampleRate int    `json:"sample_rate,omitempty"`
	Channels   int    `json:"channels,omitempty"`
	BitDepth   int    `json:"bit_depth,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Bitrate    int    `json:"bitrate,omitempty"` // average bits per second
	AudioMD5   string `json:"audio_md5,omitempty"`
	Tags       Tags   `json:"tags"`
	// Raw holds every text field as read, keyed by the format's own field name.
	Raw map[string][]string `json:"raw,omitempty"`
	// Legacy holds the original bytes of fields that needed DecodeLegacy, so the
	// decoding can be redone with another encoding later (D2: keep the raw bytes).
	Legacy   map[string][]byte `json:"legacy,omitempty"`
	Encoding string            `json:"encoding,omitempty"` // encoding applied to legacy fields
	Cover    *Picture          `json:"cover,omitempty"`
}

// maxPicture caps embedded cover art we keep in memory.
const maxPicture = 16 << 20

func readAt(r io.ReaderAt, off int64, n int) ([]byte, error) {
	if n < 0 {
		return nil, errors.New("negative length")
	}
	b := make([]byte, n)
	got, err := r.ReadAt(b, off)
	if got == n {
		return b, nil
	}
	if err == nil || err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return nil, err
}

// Probe identifies the file and reads its metadata. size is the file length.
func Probe(r io.ReaderAt, size int64) (*Info, error) {
	head, err := readAt(r, 0, int(min(size, 64)))
	if err != nil {
		return nil, err
	}
	info := &Info{Raw: map[string][]string{}}
	switch {
	case bytes.HasPrefix(head, []byte("ID3")):
		tagEnd, err := id3Size(head)
		if err != nil {
			return nil, err
		}
		if magic, _ := readAt(r, tagEnd, 4); string(magic) == "fLaC" {
			err = probeFLAC(r, size, tagEnd+4, info)
		} else {
			err = probeMP3(r, size, info)
		}
		if err != nil {
			return nil, err
		}
	case bytes.HasPrefix(head, []byte("fLaC")):
		if err := probeFLAC(r, size, 4, info); err != nil {
			return nil, err
		}
	case bytes.HasPrefix(head, []byte("OggS")):
		if err := probeOgg(r, size, info); err != nil {
			return nil, err
		}
	case len(head) >= 8 && string(head[4:8]) == "ftyp":
		if err := probeMP4(r, size, info); err != nil {
			return nil, err
		}
	case len(head) >= 2 && head[0] == 0xFF && head[1]&0xE0 == 0xE0:
		if err := probeMP3(r, size, info); err != nil {
			return nil, err
		}
	default:
		if name := unsupportedFormat(head); name != "" {
			info.Format, info.Codec = name, name
			return info, nil
		}
		return nil, ErrUnknownFormat
	}
	info.Playable = info.Format == "flac" || info.Format == "mp3" || info.Format == "ogg" || info.Format == "opus" ||
		(info.Format == "m4a" && info.Codec == "aac")
	if info.Bitrate == 0 && info.DurationMS > 0 {
		info.Bitrate = int(size * 8 * 1000 / info.DurationMS)
	}
	return info, nil
}

// unsupportedFormat names formats that D2 converts or rejects, so the import can say why.
func unsupportedFormat(h []byte) string {
	s := string(h)
	switch {
	case strings.HasPrefix(s, "RIFF") && len(s) >= 12 && s[8:12] == "WAVE":
		return "wav"
	case strings.HasPrefix(s, "FORM") && len(s) >= 12 && (s[8:12] == "AIFF" || s[8:12] == "AIFC"):
		return "aiff"
	case strings.HasPrefix(s, "MAC "):
		return "ape"
	case strings.HasPrefix(s, "tBaK"):
		return "tak"
	case strings.HasPrefix(s, "wvpk"):
		return "wavpack"
	case strings.HasPrefix(s, "TTA1"):
		return "tta"
	case strings.HasPrefix(s, "DSD "):
		return "dsf"
	case strings.HasPrefix(s, "FRM8"):
		return "dff"
	case bytes.HasPrefix(h, []byte{0x30, 0x26, 0xB2, 0x75, 0x8E, 0x66, 0xCF, 0x11}):
		return "wma"
	}
	return ""
}

func (info *Info) addRaw(key, value string) {
	info.Raw[key] = append(info.Raw[key], value)
}

func (info *Info) addLegacy(key string, raw []byte, enc string) {
	if enc == EncUTF8 {
		return
	}
	if info.Legacy == nil {
		info.Legacy = map[string][]byte{}
	}
	info.Legacy[key] = append([]byte(nil), raw...)
	if info.Encoding == "" || info.Encoding == EncCP1252 {
		info.Encoding = enc // prefer reporting cp932 if any field used it
	}
}

func (info *Info) setPicture(p Picture) {
	if len(p.Data) == 0 || len(p.Data) > maxPicture {
		return
	}
	if info.Cover == nil || (p.Type == 3 && info.Cover.Type != 3) {
		info.Cover = &p
	}
}

// applyField maps a Vorbis-style field name onto Tags. ID3 and MP4 fields are translated
// to these names before calling it.
func (t *Tags) applyField(key, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	setIfEmpty := func(dst *string) {
		if *dst == "" {
			*dst = value
		}
	}
	switch strings.ToUpper(key) {
	case "TITLE":
		setIfEmpty(&t.Title)
	case "ARTIST":
		setIfEmpty(&t.Artist)
	case "ALBUM":
		setIfEmpty(&t.Album)
	case "ALBUMARTIST", "ALBUM ARTIST", "ALBUM_ARTIST":
		setIfEmpty(&t.AlbumArtist)
	case "DATE", "YEAR", "ORIGINALDATE":
		setIfEmpty(&t.Date)
	case "GENRE":
		setIfEmpty(&t.Genre)
	case "TRACKNUMBER", "TRACK":
		n, total := splitNumber(value)
		if t.TrackNo == 0 {
			t.TrackNo = n
		}
		if t.TrackTotal == 0 {
			t.TrackTotal = total
		}
	case "TRACKTOTAL", "TOTALTRACKS":
		if t.TrackTotal == 0 {
			t.TrackTotal, _ = splitNumber(value)
		}
	case "DISCNUMBER", "DISC":
		n, total := splitNumber(value)
		if t.DiscNo == 0 {
			t.DiscNo = n
		}
		if t.DiscTotal == 0 {
			t.DiscTotal = total
		}
	case "DISCTOTAL", "TOTALDISCS":
		if t.DiscTotal == 0 {
			t.DiscTotal, _ = splitNumber(value)
		}
	}
}

// splitNumber parses "3", "03" or "3/12".
func splitNumber(s string) (n, total int) {
	a, b, _ := strings.Cut(strings.TrimSpace(s), "/")
	n, _ = strconv.Atoi(strings.TrimSpace(a))
	total, _ = strconv.Atoi(strings.TrimSpace(b))
	return n, total
}

// parsePictureBlock decodes the FLAC PICTURE structure (also base64-embedded in Vorbis
// comments as METADATA_BLOCK_PICTURE).
func parsePictureBlock(b []byte) (Picture, error) {
	be := binary.BigEndian
	p := Picture{}
	if len(b) < 8 {
		return p, errors.New("short picture block")
	}
	p.Type = int(be.Uint32(b))
	q := 4
	field := func() ([]byte, error) {
		if q+4 > len(b) {
			return nil, errors.New("short picture block")
		}
		n := int(be.Uint32(b[q:]))
		q += 4
		if n < 0 || q+n > len(b) {
			return nil, errors.New("short picture block")
		}
		v := b[q : q+n]
		q += n
		return v, nil
	}
	mime, err := field()
	if err != nil {
		return p, err
	}
	p.MIME = string(mime)
	if _, err := field(); err != nil { // description
		return p, err
	}
	if q+16 > len(b) {
		return p, errors.New("short picture block")
	}
	q += 16 // width, height, depth, colors
	data, err := field()
	if err != nil {
		return p, err
	}
	p.Data = append([]byte(nil), data...)
	return p, nil
}

func (info *Info) String() string {
	return fmt.Sprintf("%s/%s %dHz %dch %dbit %.1fs %q - %q", info.Format, info.Codec, info.SampleRate,
		info.Channels, info.BitDepth, float64(info.DurationMS)/1000, info.Tags.Artist, info.Tags.Title)
}
