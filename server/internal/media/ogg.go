package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
)

const maxOggHeaders = maxPicture + 1<<20

// oggPackets reassembles the first n packets of the first logical stream.
func oggPackets(r io.ReaderAt, size int64, n int) ([][]byte, uint32, error) {
	var packets [][]byte
	var cur []byte
	var serial uint32
	first := true
	for pos := int64(0); pos+27 <= size && len(packets) < n; {
		h, err := readAt(r, pos, 27)
		if err != nil {
			return nil, 0, err
		}
		if string(h[:4]) != "OggS" {
			return nil, 0, errors.New("ogg: lost page sync")
		}
		s := binary.LittleEndian.Uint32(h[14:])
		nseg := int(h[26])
		lacing, err := readAt(r, pos+27, nseg)
		if err != nil {
			return nil, 0, err
		}
		dataLen := 0
		for _, l := range lacing {
			dataLen += int(l)
		}
		data, err := readAt(r, pos+27+int64(nseg), dataLen)
		if err != nil {
			return nil, 0, err
		}
		pos += 27 + int64(nseg) + int64(dataLen)
		if first {
			serial, first = s, false
		}
		if s != serial {
			continue // another multiplexed stream
		}
		off := 0
		for _, l := range lacing {
			cur = append(cur, data[off:off+int(l)]...)
			off += int(l)
			if len(cur) > maxOggHeaders {
				return nil, 0, errors.New("ogg: header packet too large")
			}
			if l < 255 {
				packets = append(packets, cur)
				cur = nil
				if len(packets) == n {
					break
				}
			}
		}
	}
	if len(packets) < n {
		return nil, 0, errors.New("ogg: missing header packets")
	}
	return packets, serial, nil
}

// lastGranule finds the granule position of the stream's last page.
func lastGranule(r io.ReaderAt, size int64, serial uint32) int64 {
	for tail := int64(64 << 10); ; tail *= 4 {
		start := max(size-tail, 0)
		buf, err := readAt(r, start, int(size-start))
		if err != nil {
			return -1
		}
		for i := bytes.LastIndex(buf, []byte("OggS")); i >= 0; i = bytes.LastIndex(buf[:i], []byte("OggS")) {
			if i+27 > len(buf) {
				continue
			}
			g := int64(binary.LittleEndian.Uint64(buf[i+6:]))
			if binary.LittleEndian.Uint32(buf[i+14:]) == serial && g >= 0 {
				return g
			}
		}
		if start == 0 || tail >= 4<<20 {
			return -1
		}
	}
}

func probeOgg(r io.ReaderAt, size int64, info *Info) error {
	packets, serial, err := oggPackets(r, size, 2)
	if err != nil {
		return err
	}
	id, comments := packets[0], packets[1]
	le := binary.LittleEndian
	var preSkip int64
	switch {
	case len(id) >= 30 && id[0] == 1 && string(id[1:7]) == "vorbis":
		info.Format, info.Codec = "ogg", "vorbis"
		info.Channels = int(id[11])
		info.SampleRate = int(le.Uint32(id[12:]))
		if nominal := int32(le.Uint32(id[20:])); nominal > 0 {
			info.Bitrate = int(nominal)
		}
		if len(comments) < 7 || comments[0] != 3 || string(comments[1:7]) != "vorbis" {
			return errors.New("ogg: missing Vorbis comment header")
		}
		if err := parseVorbisComment(comments[7:], info); err != nil {
			return err
		}
	case len(id) >= 19 && string(id[:8]) == "OpusHead":
		info.Format, info.Codec = "opus", "opus"
		info.Channels = int(id[9])
		preSkip = int64(le.Uint16(id[10:]))
		info.SampleRate = 48000 // Opus always decodes at 48 kHz
		if len(comments) < 8 || string(comments[:8]) != "OpusTags" {
			return errors.New("ogg: missing OpusTags")
		}
		if err := parseVorbisComment(comments[8:], info); err != nil {
			return err
		}
	case len(id) >= 5 && id[0] == 0x7F && string(id[1:5]) == "FLAC":
		info.Format, info.Codec = "ogg-flac", "flac" // FLAC in Ogg: recognized, not in the whitelist
		return nil
	default:
		return errors.New("ogg: unsupported codec")
	}
	if g := lastGranule(r, size, serial); g > preSkip && info.SampleRate > 0 {
		info.DurationMS = (g - preSkip) * 1000 / int64(info.SampleRate)
	}
	return nil
}
