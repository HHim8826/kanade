package media

import (
	"encoding/binary"
	"errors"
	"io"
)

type mpegHeader struct {
	version    int // 1, 2 or 25 (MPEG 2.5)
	bitrate    int // bits per second
	sampleRate int
	channels   int
	padding    int
	frameLen   int
	spf        int // samples per frame
}

var (
	l3BitratesV1 = [16]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}
	l3BitratesV2 = [16]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0}
	sampleRates  = [3]int{44100, 48000, 32000}
)

// parseMPEGHeader decodes a Layer III frame header; other layers are rejected.
func parseMPEGHeader(b []byte) (mpegHeader, bool) {
	if len(b) < 4 || b[0] != 0xFF || b[1]&0xE0 != 0xE0 {
		return mpegHeader{}, false
	}
	var h mpegHeader
	switch (b[1] >> 3) & 3 {
	case 0:
		h.version = 25
	case 2:
		h.version = 2
	case 3:
		h.version = 1
	default:
		return h, false
	}
	if (b[1]>>1)&3 != 1 { // Layer III only
		return h, false
	}
	bi, si := b[2]>>4, (b[2]>>2)&3
	if si == 3 {
		return h, false
	}
	h.sampleRate = sampleRates[si]
	if h.version == 1 {
		h.bitrate = l3BitratesV1[bi] * 1000
		h.spf = 1152
	} else {
		h.bitrate = l3BitratesV2[bi] * 1000
		h.spf = 576
		h.sampleRate /= 2
		if h.version == 25 {
			h.sampleRate /= 2
		}
	}
	if h.bitrate == 0 {
		return h, false // free-format or invalid
	}
	h.padding = int((b[2] >> 1) & 1)
	h.channels = 2
	if (b[3]>>6)&3 == 3 {
		h.channels = 1
	}
	h.frameLen = h.spf/8*h.bitrate/h.sampleRate + h.padding
	return h, h.frameLen > 4
}

func probeMP3(r io.ReaderAt, size int64, info *Info) error {
	info.Format, info.Codec = "mp3", "mp3"
	var audioStart int64
	if h, err := readAt(r, 0, 3); err == nil && string(h) == "ID3" {
		end, err := parseID3v2(r, size, info)
		if err != nil {
			return err
		}
		audioStart = end
	}
	hasV1 := false
	if info.Tags == (Tags{}) {
		hasV1 = parseID3v1(r, size, info)
	} else if b, err := readAt(r, size-128, 3); err == nil && string(b) == "TAG" {
		hasV1 = true
	}
	audioEnd := size
	if hasV1 {
		audioEnd -= 128
	}

	// Find the first real frame: one whose successor is also a frame header.
	scanLen := int(min(audioEnd-audioStart, 256<<10))
	if scanLen < 4 {
		return errors.New("mp3: no audio data")
	}
	buf, err := readAt(r, audioStart, scanLen)
	if err != nil {
		return err
	}
	for i := 0; i+4 <= len(buf); i++ {
		h, ok := parseMPEGHeader(buf[i:])
		if !ok {
			continue
		}
		next := i + h.frameLen
		if next+4 <= len(buf) {
			if _, ok2 := parseMPEGHeader(buf[next:]); !ok2 {
				continue
			}
		}
		frameStart := audioStart + int64(i)
		info.SampleRate, info.Channels = h.sampleRate, h.channels
		frame, err := readAt(r, frameStart, int(min(int64(h.frameLen+64), audioEnd-frameStart)))
		if err != nil {
			frame = buf[i:]
		}
		if frames := vbrFrames(frame, h); frames > 0 {
			info.DurationMS = frames * int64(h.spf) * 1000 / int64(h.sampleRate)
		} else {
			info.Bitrate = h.bitrate
			info.DurationMS = (audioEnd - frameStart) * 8 * 1000 / int64(h.bitrate)
		}
		return nil
	}
	return errors.New("mp3: no MPEG Layer III frame found")
}

// vbrFrames reads the frame count from a Xing/Info or VBRI header in the first frame.
func vbrFrames(frame []byte, h mpegHeader) int64 {
	side := 32
	switch {
	case h.version == 1 && h.channels == 1:
		side = 17
	case h.version != 1 && h.channels == 2:
		side = 17
	case h.version != 1 && h.channels == 1:
		side = 9
	}
	if off := 4 + side; off+12 <= len(frame) {
		tag := string(frame[off : off+4])
		if tag == "Xing" || tag == "Info" {
			flags := binary.BigEndian.Uint32(frame[off+4:])
			if flags&1 != 0 {
				return int64(binary.BigEndian.Uint32(frame[off+8:]))
			}
		}
	}
	if off := 4 + 32; off+18 <= len(frame) && string(frame[off:off+4]) == "VBRI" {
		return int64(binary.BigEndian.Uint32(frame[off+14:]))
	}
	return 0
}
