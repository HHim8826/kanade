package ffmpeg

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// tool finds the FFmpeg bundled with the project (server/../tools), or skips.
func tool(t *testing.T) *Tool {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	data := filepath.Join(filepath.Dir(file), "..", "..", "..", "var") // tools/ is next to the data directory
	tl := Find(data)
	if tl == nil {
		t.Skip("no ffmpeg")
	}
	return tl
}

// tone makes a test file with FFmpeg's sine source.
func tone(t *testing.T, tl *Tool, dst, codec string, rate, seconds int) {
	t.Helper()
	err := tl.run(context.Background(), "-f", "lavfi", "-i", "sine=frequency=440:sample_rate="+strconv.Itoa(rate)+":duration="+strconv.Itoa(seconds),
		"-ac", "2", "-c:a", codec, "-metadata", "title=Tone", "-metadata", "album=Test", dst)
	if err != nil {
		t.Fatal(err)
	}
}

func TestConvertVerifiesAndKeepsBitDepth(t *testing.T) {
	ctx := context.Background()
	tl := tool(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "hires.wav")
	tl.run(ctx, "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=96000:duration=2", "-ac", "2", "-c:a", "pcm_s24le",
		"-metadata", "title=Hi", src)
	s, err := tl.Probe(ctx, src)
	if err != nil || s.Bits != 24 || s.SampleRate != 96000 || s.Channels != 2 || s.Samples != 192000 || s.Float {
		t.Fatalf("probe %+v %v", s, err)
	}
	dst := filepath.Join(dir, "hires.flac")
	if err := tl.ToFLAC(ctx, src, dst, s, MaxFLAC(s, 0, 0)); err != nil {
		t.Fatal(err)
	}
	out, _ := tl.Probe(ctx, dst)
	if out.Bits != 24 || out.SampleRate != 96000 || out.Tags["title"] != "Hi" {
		t.Fatalf("output %+v", out)
	}
	a, err1 := tl.PCMMD5(ctx, 24, src)
	b, err2 := tl.PCMMD5(ctx, 24, dst)
	if err1 != nil || err2 != nil || a != b {
		t.Fatalf("pcm md5 %s %s %v %v", a, b, err1, err2)
	}

	f := filepath.Join(dir, "float.wav")
	tl.run(ctx, "-f", "lavfi", "-i", "sine=duration=1", "-c:a", "pcm_f32le", f)
	fs, _ := tl.Probe(ctx, f)
	if err := tl.ToFLAC(ctx, f, filepath.Join(dir, "float.flac"), fs, 0); !errors.Is(err, ErrFloat) {
		t.Fatalf("float: %v", err)
	}
}

func TestSplitIsSampleExact(t *testing.T) {
	ctx := context.Background()
	tl := tool(t)
	dir := t.TempDir()
	img := filepath.Join(dir, "image.flac")
	tone(t, tl, img, "flac", 44100, 10)
	s, _ := tl.Probe(ctx, img)
	cuts := []Cut{
		{Start: 0, End: 132300, Dst: filepath.Join(dir, "1.flac"), Tags: map[string]string{"title": "一"}},
		{Start: 132300, End: 264601, Dst: filepath.Join(dir, "2.flac"), Tags: map[string]string{"title": "二"}},
		{Start: 264601, Dst: filepath.Join(dir, "3.flac")},
	}
	if err := tl.Split(ctx, img, cuts, s); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, c := range cuts {
		p, err := tl.Probe(ctx, c.Dst)
		if err != nil {
			t.Fatal(err)
		}
		total += p.Samples
	}
	if total != 441000 {
		t.Fatalf("pieces hold %d samples", total)
	}
	if p, _ := tl.Probe(ctx, cuts[1].Dst); p.Tags["title"] != "二" || p.Tags["album"] != "" {
		t.Fatalf("tags %+v", p.Tags) // only the given tags, not the image's
	}
	whole, _ := tl.PCMMD5(ctx, 16, img)
	joined, _ := tl.PCMMD5(ctx, 16, cuts[0].Dst, cuts[1].Dst, cuts[2].Dst)
	if whole != joined {
		t.Fatalf("md5 %s != %s", whole, joined)
	}
	if _, err := os.Stat(cuts[2].Dst); err != nil {
		t.Fatal(err)
	}
}

// Loudness is measured from a file or from a stream that cannot seek, alike; silence gives the
// stand-in values (review #136).
func TestLoudness(t *testing.T) {
	tl := tool(t)
	ctx := context.Background()
	src := filepath.Join("..", "media", "testdata", "tone.flac")
	lufs, peak, err := tl.Loudness(ctx, src, nil)
	if err != nil || lufs > -10 || lufs < -40 || peak > 0 || peak < -40 {
		t.Fatalf("file: %v LUFS, %v dBFS, %v", lufs, peak, err)
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	l2, p2, err := tl.Loudness(ctx, "", f)
	if err != nil || l2 != lufs || p2 != peak {
		t.Fatalf("stream: %v LUFS, %v dBFS, %v; file gave %v, %v", l2, p2, err, lufs, peak)
	}
	if _, _, err := tl.Loudness(ctx, "", strings.NewReader("not audio")); err == nil {
		t.Fatal("garbage measured")
	}
	l, p, err := parseLoudness("[Parsed_ebur128_0 @ 0x1] Summary:\n\n  Integrated loudness:\n    I:         -70.0 LUFS\n" +
		"    Threshold:   -inf LUFS\n\n  Sample peak:\n    Peak:       -inf dBFS\n")
	if err != nil || l != SilentLUFS || p != SilentPeak {
		t.Fatalf("silence: %v %v %v", l, p, err)
	}
}
