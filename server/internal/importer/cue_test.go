package importer

import "testing"

const sampleCue = "\xef\xbb\xbfREM GENRE Anime\r\nREM DATE 2006\r\nREM DISCNUMBER 2\r\nCATALOG 4988002501234\r\n" +
	"PERFORMER \"牧野由依\"\r\nTITLE \"ユーフォリア\"\r\nFILE \"ユーフォリア.wav\" WAVE\r\n" +
	"  TRACK 01 AUDIO\r\n    TITLE \"ユーフォリア\"\r\n    INDEX 00 00:00:00\r\n    INDEX 01 00:05:00\r\n" +
	"  TRACK 02 AUDIO\r\n    TITLE \"雨降花\"\r\n    PERFORMER \"Other\"\r\n    INDEX 00 04:58:30\r\n    INDEX 01 05:00:00\r\n" +
	"  TRACK 03 AUDIO\r\n    TITLE \"Instrumental\"\r\n    INDEX 01 09:30:74\r\n"

func TestParseCueAndPieces(t *testing.T) {
	c := parseCue(sampleCue)
	if c.Title != "ユーフォリア" || c.Performer != "牧野由依" || c.Date != "2006" || c.Disc != 2 || c.Catalog != "4988002501234" || c.Genre != "Anime" {
		t.Fatalf("sheet %+v", c)
	}
	if len(c.Files) != 1 || c.Files[0].Name != "ユーフォリア.wav" || len(c.Files[0].Tracks) != 3 {
		t.Fatalf("files %+v", c.Files)
	}
	tr := c.Files[0].Tracks[1]
	if tr.Title != "雨降花" || tr.Performer != "Other" || tr.Index0 != (4*60+58)*75+30 || tr.Index1 != 5*60*75 {
		t.Fatalf("track 2 %+v", tr)
	}
	p := c.Files[0].pieces(44100)
	// 5 s before track 1 is a hidden track; track 2's pregap (INDEX 00) stays with track 1.
	if len(p) != 4 || p[0].Number != 0 || p[0].End != 5*44100 || p[1].Start != 5*44100 || p[1].End != 300*44100 ||
		p[2].Start != 300*44100 || p[3].Start != ((9*60+30)*75+74)*588 || p[3].End != 0 {
		t.Fatalf("pieces %+v", p)
	}
	// A two-second gap is part of track 1, not a hidden track.
	c.Files[0].Tracks[0].Index1 = 150
	if p := c.Files[0].pieces(44100); len(p) != 3 || p[0].Number != 1 || p[0].Start != 0 {
		t.Fatalf("short gap %+v", p)
	}
	if p := parseCue("FILE a.flac WAVE\nTRACK 01 AUDIO\nINDEX 01 00:10:00\nTRACK 02 AUDIO\nINDEX 01 00:05:00\n").Files[0].pieces(44100); p != nil {
		t.Fatalf("out of order accepted: %+v", p)
	}
	if f := parseCue("FILE \"a b.flac\" WAVE\n").Files[0]; f.Name != "a b.flac" {
		t.Fatalf("name %q", f.Name)
	}
}
