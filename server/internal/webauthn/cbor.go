package webauthn

import (
	"encoding/binary"
	"errors"
	"math"
)

var errCBOR = errors.New("malformed CBOR")

// decode reads one CBOR data item (RFC 8949) of the kinds WebAuthn uses: integers, byte and text
// strings, arrays, maps with integer or text keys, and false, true and null. Lengths must be
// definite, as WebAuthn's canonical encoding has them. It returns the item and what follows it.
func decode(b []byte, depth int) (any, []byte, error) {
	if len(b) == 0 || depth > 16 {
		return nil, nil, errCBOR
	}
	major, info := b[0]>>5, b[0]&0x1f
	b = b[1:]
	var n uint64
	switch {
	case info < 24:
		n = uint64(info)
	case info <= 27:
		size := 1 << (info - 24)
		if len(b) < size {
			return nil, nil, errCBOR
		}
		switch size {
		case 1:
			n = uint64(b[0])
		case 2:
			n = uint64(binary.BigEndian.Uint16(b))
		case 4:
			n = uint64(binary.BigEndian.Uint32(b))
		case 8:
			n = binary.BigEndian.Uint64(b)
		}
		b = b[size:]
	default: // indefinite lengths and reserved values
		return nil, nil, errCBOR
	}
	switch major {
	case 0, 1:
		if n > math.MaxInt64 {
			return nil, nil, errCBOR
		}
		if major == 1 {
			return -1 - int64(n), b, nil
		}
		return int64(n), b, nil
	case 2, 3:
		if n > uint64(len(b)) {
			return nil, nil, errCBOR
		}
		if major == 3 {
			return string(b[:n]), b[n:], nil
		}
		return b[:n:n], b[n:], nil
	case 4:
		if n > uint64(len(b)) { // every item takes a byte at least
			return nil, nil, errCBOR
		}
		out := make([]any, 0, n)
		for range n {
			v, rest, err := decode(b, depth+1)
			if err != nil {
				return nil, nil, err
			}
			out, b = append(out, v), rest
		}
		return out, b, nil
	case 5:
		if n > uint64(len(b))/2 {
			return nil, nil, errCBOR
		}
		out := make(map[any]any, n)
		for range n {
			k, rest, err := decode(b, depth+1)
			if err != nil {
				return nil, nil, err
			}
			switch k.(type) {
			case int64, string:
			default:
				return nil, nil, errCBOR
			}
			if _, dup := out[k]; dup {
				return nil, nil, errCBOR
			}
			v, rest, err := decode(rest, depth+1)
			if err != nil {
				return nil, nil, err
			}
			out[k], b = v, rest
		}
		return out, b, nil
	case 7:
		switch info {
		case 20:
			return false, b, nil
		case 21:
			return true, b, nil
		case 22:
			return nil, b, nil
		}
	}
	return nil, nil, errCBOR // tags, floats, undefined
}
