// Package ffmpeg runs FFmpeg for what decision D2 needs it for: converting lossless formats that
// players cannot all play (APE, TAK, WavPack, TTA, ALAC, WAV, AIFF) to FLAC, and splitting a
// whole-disc image by its CUE sheet. One job at a time, at low CPU priority (D2 §7).
package ffmpeg

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/HHim8826/kanade/server/internal/proc"
)

// Tool is an FFmpeg installation.
type Tool struct {
	ffmpeg, ffprobe string
	nice            string // nice(1), when the system has it
	mu              sync.Mutex
}

// Find locates FFmpeg: $KANADE_FFMPEG (the ffmpeg binary, with ffprobe beside it), then
// tools/ffmpeg/bin next to the data directory, then PATH. It returns nil when there is none; the
// files that need it then wait with a reason and everything else imports as usual.
func Find(dataDir string) *Tool {
	var candidates []string
	if p := os.Getenv("KANADE_FFMPEG"); p != "" {
		candidates = append(candidates, p)
	}
	candidates = append(candidates, filepath.Join(filepath.Dir(filepath.Clean(dataDir)), "tools", "ffmpeg", "bin", "ffmpeg"))
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		candidates = append(candidates, p)
	}
	for _, p := range candidates {
		probe := filepath.Join(filepath.Dir(p), "ffprobe")
		if isExec(p) && isExec(probe) {
			t := &Tool{ffmpeg: p, ffprobe: probe}
			t.nice, _ = exec.LookPath("nice")
			return t
		}
	}
	return nil
}

func isExec(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0
}

func (t *Tool) Path() string { return t.ffmpeg }

// command builds an FFmpeg or FFprobe run at low priority.
// It is killed if the server dies, so a restarted import never races a leftover run writing the
// same output.
func (t *Tool) command(ctx context.Context, bin string, args ...string) *exec.Cmd {
	var cmd *exec.Cmd
	if t.nice != "" {
		cmd = exec.CommandContext(ctx, t.nice, append([]string{"-n", "10", bin}, args...)...)
	} else {
		cmd = exec.CommandContext(ctx, bin, args...)
	}
	proc.DieWithParent(cmd, syscall.SIGKILL)
	return cmd
}

func (t *Tool) run(ctx context.Context, args ...string) error {
	var stderr bytes.Buffer
	cmd := t.command(ctx, t.ffmpeg, append([]string{"-nostdin", "-hide_banner", "-v", "error", "-y"}, args...)...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("ffmpeg: %v: %s", err, lastLine(stderr.String()))
	}
	return nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// Stream describes a file's first audio stream.
type Stream struct {
	Codec      string
	SampleRate int
	Channels   int
	Bits       int   // bits per sample as stored: 16, 24, 32
	Float      bool  // floating-point samples, which FLAC cannot hold
	Samples    int64 // length in samples, 0 when unknown
	Tags       map[string]string
	Pictures   int64 // bytes of embedded pictures, which conversion and splitting copy
}

var ErrFloat = errors.New("floating-point audio cannot be stored losslessly in FLAC")

// Probe reads the first audio stream of a file.
func (t *Tool) Probe(ctx context.Context, path string) (*Stream, error) {
	out, err := t.command(ctx, t.ffprobe, "-v", "error", "-select_streams", "a:0", "-show_streams", "-show_format",
		"-of", "json", path).Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	var res struct {
		Streams []struct {
			CodecName  string            `json:"codec_name"`
			SampleFmt  string            `json:"sample_fmt"`
			SampleRate string            `json:"sample_rate"`
			Channels   int               `json:"channels"`
			RawBits    string            `json:"bits_per_raw_sample"`
			Bits       int               `json:"bits_per_sample"`
			DurationTS int64             `json:"duration_ts"`
			TimeBase   string            `json:"time_base"`
			Duration   string            `json:"duration"`
			Tags       map[string]string `json:"tags"`
		} `json:"streams"`
		Format struct {
			Tags map[string]string `json:"tags"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	if len(res.Streams) == 0 {
		return nil, errors.New("no audio stream")
	}
	st := res.Streams[0]
	s := &Stream{Codec: st.CodecName, Channels: st.Channels, Tags: map[string]string{}}
	s.SampleRate, _ = strconv.Atoi(st.SampleRate)
	switch st.SampleFmt {
	case "flt", "fltp", "dbl", "dblp":
		s.Float = true
	}
	if n, err := strconv.Atoi(st.RawBits); err == nil && n > 0 {
		s.Bits = n
	} else if st.Bits > 0 {
		s.Bits = st.Bits
	} else if strings.HasPrefix(st.SampleFmt, "s16") || strings.HasPrefix(st.SampleFmt, "u8") {
		s.Bits = 16
	} else {
		s.Bits = 32
	}
	if st.TimeBase == "1/"+st.SampleRate && st.DurationTS > 0 {
		s.Samples = st.DurationTS
	} else if d, err := strconv.ParseFloat(st.Duration, 64); err == nil && s.SampleRate > 0 {
		s.Samples = int64(d*float64(s.SampleRate) + 0.5)
	}
	for _, m := range []map[string]string{res.Format.Tags, st.Tags} {
		for k, v := range m {
			s.Tags[strings.ToLower(k)] = v
		}
	}
	// An embedded picture is one packet of a video stream.
	if out, err := t.command(ctx, t.ffprobe, "-v", "error", "-select_streams", "v", "-show_entries", "packet=size",
		"-of", "csv=p=0", path).Output(); err == nil {
		for _, line := range strings.Fields(string(out)) {
			if n, err := strconv.ParseInt(strings.Trim(line, ","), 10, 64); err == nil {
				s.Pictures += n
			}
		}
	}
	return s, nil
}

// MaxFLAC bounds the size of a FLAC file holding samples of s (all of them when samples is 0)
// with its tags and embedded pictures: FLAC never stores audio in more than a sliver over its raw
// PCM size. srcSize is the fallback when the length is unknown. Staging space is reserved by it,
// and the output is cut off at it (review #4).
func MaxFLAC(s *Stream, samples, srcSize int64) int64 {
	if samples == 0 {
		samples = s.Samples
	}
	const overhead = 256 << 10
	if samples <= 0 || s.Channels <= 0 {
		return 4*srcSize + overhead + s.Pictures
	}
	pcm := samples * int64(s.Channels) * int64((s.Bits+7)/8)
	return pcm + pcm/64 + overhead + s.Pictures
}

// pcm is the little-endian PCM codec and raw format that hold samples of this many bits exactly;
// decoded at that size, a FLAC file's PCM MD5 is the one in its STREAMINFO.
func pcm(bits int) (codec, format string) {
	switch {
	case bits <= 16:
		return "pcm_s16le", "s16le"
	case bits <= 24:
		return "pcm_s24le", "s24le"
	}
	return "pcm_s32le", "s32le"
}

// ToFLAC converts the first audio stream of src to FLAC at the same sample rate, bit depth and
// channels, copying the tags and an embedded cover.
// The output stops at limit bytes (0: no limit); a cut-off file then fails the PCM check.
func (t *Tool) ToFLAC(ctx context.Context, src, dst string, s *Stream, limit int64) error {
	if s.Float {
		return ErrFloat
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	args := []string{"-i", src, "-map", "0:a:0", "-map", "0:v?", "-c:a", "flac", "-c:v", "copy",
		"-disposition:v", "attached_pic", "-map_metadata", "0"}
	if limit > 0 {
		args = append(args, "-fs", strconv.FormatInt(limit, 10))
	}
	return t.run(ctx, append(args, dst)...)
}

// Cut is one piece of a split: samples [Start, End) of the source, End 0 meaning its end. Its
// output stops at Limit bytes when Limit is set.
type Cut struct {
	Start, End int64
	Dst        string
	Tags       map[string]string
	Limit      int64
}

// Split cuts src into pieces in one decoding pass, sample-exact, each to FLAC with its tags and
// the source's embedded cover.
func (t *Tool) Split(ctx context.Context, src string, cuts []Cut, s *Stream) error {
	if s.Float {
		return ErrFloat
	}
	if len(cuts) == 0 {
		return errors.New("nothing to split")
	}
	var graph strings.Builder
	graph.WriteString("[0:a]asplit=" + strconv.Itoa(len(cuts)))
	for i := range cuts {
		fmt.Fprintf(&graph, "[s%d]", i)
	}
	for i, c := range cuts {
		fmt.Fprintf(&graph, ";[s%d]atrim=start_sample=%d", i, c.Start)
		if c.End > 0 {
			fmt.Fprintf(&graph, ":end_sample=%d", c.End)
		}
		fmt.Fprintf(&graph, ",asetpts=PTS-STARTPTS[o%d]", i)
	}
	args := []string{"-i", src, "-filter_complex", graph.String()}
	for i, c := range cuts {
		args = append(args, "-map", fmt.Sprintf("[o%d]", i), "-map", "0:v?", "-c:a", "flac", "-c:v", "copy",
			"-disposition:v", "attached_pic", "-map_metadata", "-1")
		// In a fixed order: the same cut of the same image is then the same bytes, so a later import
		// recognizes it by its checksum.
		for _, k := range slices.Sorted(maps.Keys(c.Tags)) {
			if v := c.Tags[k]; v != "" {
				args = append(args, "-metadata", k+"="+v)
			}
		}
		if c.Limit > 0 {
			args = append(args, "-fs", strconv.FormatInt(c.Limit, 10))
		}
		args = append(args, c.Dst)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.run(ctx, args...)
}

// PCMMD5 decodes files one after another as little-endian PCM of bits and returns the MD5 of all
// of it: one file's PCM MD5, or the MD5 of a split's pieces put back together.
func (t *Tool) PCMMD5(ctx context.Context, bits int, files ...string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	codec, format := pcm(bits)
	h := md5.New()
	for _, f := range files {
		var stderr bytes.Buffer
		cmd := t.command(ctx, t.ffmpeg, "-nostdin", "-hide_banner", "-v", "error", "-i", f, "-map", "0:a:0",
			"-c:a", codec, "-f", format, "-")
		cmd.Stderr = &stderr
		out, err := cmd.StdoutPipe()
		if err != nil {
			return "", err
		}
		if err := cmd.Start(); err != nil {
			return "", err
		}
		_, cerr := io.Copy(h, out)
		if err := cmd.Wait(); err != nil || cerr != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", fmt.Errorf("decoding %s: %v %v %s", filepath.Base(f), err, cerr, lastLine(stderr.String()))
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

var (
	integrated = regexp.MustCompile(`Integrated loudness:\s+I:\s+(-?[0-9.]+|-inf) LUFS`)
	samplePeak = regexp.MustCompile(`Sample peak:\s+Peak:\s+(-?[0-9.]+|-inf) dBFS`)
)

// Silence has no loudness or peak FFmpeg can put a number on; these stand for it.
const (
	SilentLUFS = -70.0
	SilentPeak = -120.0
)

// Loudness measures a song's integrated loudness (EBU R128, LUFS) and sample peak (dBFS) with
// FFmpeg's ebur128 filter, from the file at path or, when in is given, from in (a format that can be
// read without seeking: FLAC, MP3, Ogg). It runs beside the other jobs, at the same low priority:
// most of the time it waits for the file to arrive.
func (t *Tool) Loudness(ctx context.Context, path string, in io.Reader) (lufs, peak float64, err error) {
	src := path
	if in != nil {
		src = "pipe:0"
	}
	var stderr bytes.Buffer
	cmd := t.command(ctx, t.ffmpeg, "-nostdin", "-hide_banner", "-nostats", "-v", "info", "-i", src, "-map", "0:a:0",
		"-af", "ebur128=peak=sample:framelog=quiet", "-f", "null", "-")
	cmd.Stdin = in
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return 0, 0, ctx.Err()
		}
		return 0, 0, fmt.Errorf("measuring loudness: %v: %s", err, lastLine(stderr.String()))
	}
	return parseLoudness(stderr.String())
}

func parseLoudness(log string) (lufs, peak float64, err error) {
	num := func(re *regexp.Regexp, silent float64) (float64, bool) {
		m := re.FindAllStringSubmatch(log, -1)
		if m == nil {
			return 0, false
		}
		v := m[len(m)-1][1] // the summary, at the end
		if v == "-inf" {
			return silent, true
		}
		f, err := strconv.ParseFloat(v, 64)
		return max(f, silent), err == nil
	}
	l, ok1 := num(integrated, SilentLUFS)
	p, ok2 := num(samplePeak, SilentPeak)
	if !ok1 || !ok2 {
		return 0, 0, errors.New("measuring loudness: FFmpeg gave no summary")
	}
	return l, p, nil
}
