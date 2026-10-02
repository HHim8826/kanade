package media

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/simplifiedchinese"
)

func probeFile(t *testing.T, name string) *Info {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	info, err := Probe(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return info
}

func TestFixtures(t *testing.T) {
	cases := []struct {
		file, format, codec string
		playable            bool
		rate, depth         int
		tags                bool // has the full Japanese tag set
		cover               bool
	}{
		{"tone.flac", "flac", "flac", true, 44100, 16, true, true},
		{"tone-hires.flac", "flac", "flac", true, 96000, 24, true, false},
		{"tone-cbr.mp3", "mp3", "mp3", true, 44100, 0, true, true},
		{"tone-vbr.mp3", "mp3", "mp3", true, 44100, 0, true, false},
		{"tone-notag.mp3", "mp3", "mp3", true, 44100, 0, false, false},
		{"tone.m4a", "m4a", "aac", true, 44100, 0, true, true},
		{"tone-alac.m4a", "m4a", "alac", false, 44100, 16, true, false},
		{"tone.ogg", "ogg", "vorbis", true, 44100, 0, true, false},
		{"tone.opus", "opus", "opus", true, 48000, 0, true, false},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			info := probeFile(t, c.file)
			if info.Format != c.format || info.Codec != c.codec || info.Playable != c.playable {
				t.Fatalf("got %s/%s playable=%v", info.Format, info.Codec, info.Playable)
			}
			if info.SampleRate != c.rate || info.BitDepth != c.depth || info.Channels != 1 {
				t.Fatalf("got %d Hz %d bit %d ch", info.SampleRate, info.BitDepth, info.Channels)
			}
			if d := info.DurationMS; d < 2950 || d > 3150 {
				t.Fatalf("duration %d ms, want about 3000", d)
			}
			if info.Bitrate <= 0 {
				t.Fatal("no bitrate")
			}
			if c.tags {
				want := Tags{Title: "テスト曲", Artist: "歌手", Album: "アルバム", AlbumArtist: "Various Artists",
					Date: "2025", TrackNo: 3, TrackTotal: 10, DiscNo: 1, DiscTotal: 2}
				if info.Tags != want {
					t.Fatalf("tags\n got %+v\nwant %+v", info.Tags, want)
				}
				if info.Encoding != "" {
					t.Fatalf("encoding %q for clean UTF-8 tags", info.Encoding)
				}
			}
			if (info.Cover != nil) != c.cover {
				t.Fatalf("cover present = %v", info.Cover != nil)
			}
			if c.cover && !bytes.HasPrefix(info.Cover.Data, []byte("\x89PNG")) {
				t.Fatalf("cover is not the PNG we embedded (%s, %d bytes)", info.Cover.MIME, len(info.Cover.Data))
			}
		})
	}
}

func TestFLACAudioMD5(t *testing.T) {
	if info := probeFile(t, "tone.flac"); len(info.AudioMD5) != 32 {
		t.Fatalf("audio md5 = %q", info.AudioMD5)
	}
}

func TestUnsupportedRecognized(t *testing.T) {
	info := probeFile(t, "tone.wav")
	if info.Format != "wav" || info.Playable {
		t.Fatalf("got %s playable=%v", info.Format, info.Playable)
	}
	if _, err := Probe(bytes.NewReader([]byte("plain text, not audio at all......")), 34); err != ErrUnknownFormat {
		t.Fatalf("err = %v", err)
	}
}

// id3Frame builds an ID3v2.3 text frame.
func id3Frame(id string, payload []byte) []byte {
	n := len(payload)
	return append([]byte{id[0], id[1], id[2], id[3], byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n), 0, 0}, payload...)
}

func TestLegacyEncodingsInID3(t *testing.T) {
	sjis, _ := japanese.ShiftJIS.NewEncoder().Bytes([]byte("テスト曲"))
	var body []byte
	body = append(body, id3Frame("TIT2", append([]byte{0}, sjis...))...)           // CP932 declared as Latin-1
	body = append(body, id3Frame("TPE1", []byte{0, 'C', 'a', 'f', 0xE9})...)       // real CP1252
	body = append(body, id3Frame("TALB", append([]byte{0}, []byte("アルバム")...))...) // UTF-8 declared as Latin-1
	body = append(body, id3Frame("TRCK", append([]byte{3}, []byte("7/12")...))...) // proper UTF-8
	n := len(body)
	header := []byte{'I', 'D', '3', 3, 0, 0, byte(n >> 21 & 0x7f), byte(n >> 14 & 0x7f), byte(n >> 7 & 0x7f), byte(n & 0x7f)}
	audio, err := os.ReadFile("testdata/tone-notag.mp3")
	if err != nil {
		t.Fatal(err)
	}
	file := append(append(header, body...), audio...)
	info, err := Probe(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	if info.Tags.Title != "テスト曲" || info.Tags.Artist != "Café" || info.Tags.Album != "アルバム" || info.Tags.TrackNo != 7 {
		t.Fatalf("tags %+v", info.Tags)
	}
	if info.Encoding != EncCP932 {
		t.Fatalf("encoding %q, want cp932", info.Encoding)
	}
	if !bytes.Equal(info.Legacy["TIT2"], sjis) {
		t.Fatal("original CP932 bytes of TIT2 not kept")
	}
	if _, ok := info.Legacy["TALB"]; ok {
		t.Fatal("UTF-8 field should not be recorded as legacy")
	}
	if d := info.DurationMS; d < 2950 || d > 3150 {
		t.Fatalf("CBR duration estimate %d ms", d)
	}
}

func TestCP1252NotMistakenForCP932(t *testing.T) {
	// From the library survey: an EAC cue in CP1252 whose Japanese was already replaced by dots.
	raw := []byte("01 - ..... .... ... .... - Navigation.1 .. .........\xb7\xb7\xb7.wav")
	s, enc := DecodeLegacy(raw)
	if enc != EncCP1252 {
		t.Fatalf("encoding %q (%q), want cp1252", enc, s)
	}
	if !LooksLost(s) {
		t.Fatalf("%q should be flagged as lost", s)
	}
	if LooksLost("ARIA The ORIGINATION Drama CD") {
		t.Fatal("ordinary title flagged as lost")
	}
}

func TestTruncatedFilesDoNotPanic(t *testing.T) {
	files, _ := filepath.Glob("testdata/*.*")
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, n := range []int{0, 1, 3, 4, 10, 11, 27, 64, 100, 500, 2000, len(b) / 2, len(b) - 1} {
			if n < 0 || n > len(b) {
				continue
			}
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("%s truncated to %d: panic %v", f, n, r)
					}
				}()
				Probe(bytes.NewReader(b[:n]), int64(n))
			}()
		}
	}
}

func TestLyricsInID3(t *testing.T) {
	// USLT: encoding, language, content descriptor (empty here), text. UTF-16 with BOM.
	text := []byte{0xFF, 0xFE}
	for _, r := range "[00:01.00]歌詞\n[00:02.00]two" {
		text = append(text, byte(r), byte(r>>8))
	}
	payload := append([]byte{1, 'j', 'p', 'n', 0xFF, 0xFE, 0, 0}, text...)
	body := id3Frame("USLT", payload)
	n := len(body)
	header := []byte{'I', 'D', '3', 3, 0, 0, byte(n >> 21 & 0x7f), byte(n >> 14 & 0x7f), byte(n >> 7 & 0x7f), byte(n & 0x7f)}
	audio, err := os.ReadFile("testdata/tone-notag.mp3")
	if err != nil {
		t.Fatal(err)
	}
	file := append(append(header, body...), audio...)
	info, err := Probe(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	if info.Tags.Lyrics != "[00:01.00]歌詞\n[00:02.00]two" {
		t.Fatalf("lyrics %q", info.Tags.Lyrics)
	}
}

func TestDecodeText(t *testing.T) {
	sjis, _ := japanese.ShiftJIS.NewEncoder().Bytes([]byte("[00:01.00]春の歌"))
	if s, enc := DecodeText(sjis); s != "[00:01.00]春の歌" || enc != EncCP932 {
		t.Fatalf("cp932: %q %s", s, enc)
	}
	utf16 := []byte{0xFF, 0xFE}
	for _, r := range "EAC ログ" {
		utf16 = append(utf16, byte(r), byte(r>>8))
	}
	if s, _ := DecodeText(utf16); s != "EAC ログ" {
		t.Fatalf("utf-16le: %q", s)
	}
	if s, _ := DecodeText(append([]byte{0xEF, 0xBB, 0xBF}, "bom"...)); s != "bom" {
		t.Fatalf("utf-8 bom: %q", s)
	}
}

func TestRedecodeWithChosenEncoding(t *testing.T) {
	gbk, _ := simplifiedchinese.GBK.NewEncoder().Bytes([]byte("测试歌曲"))
	var body []byte
	body = append(body, id3Frame("TIT2", append([]byte{0}, gbk...))...)
	body = append(body, id3Frame("TALB", append([]byte{3}, []byte("アルバム")...))...) // UTF-8 stays as it is
	n := len(body)
	header := []byte{'I', 'D', '3', 3, 0, 0, byte(n >> 21 & 0x7f), byte(n >> 14 & 0x7f), byte(n >> 7 & 0x7f), byte(n & 0x7f)}
	audio, _ := os.ReadFile("testdata/tone-notag.mp3")
	file := append(append(header, body...), audio...)
	info, err := Probe(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	if info.Tags.Title == "测试歌曲" {
		t.Fatal("GBK should not be guessed automatically")
	}
	if err := info.Redecode("gbk"); err != nil {
		t.Fatal(err)
	}
	if info.Tags.Title != "测试歌曲" || info.Tags.Album != "アルバム" || info.Encoding != "gbk" {
		t.Fatalf("after redecode %+v %q", info.Tags, info.Encoding)
	}
	if err := info.Redecode("ebcdic"); err == nil {
		t.Fatal("unknown encoding accepted")
	}
}
