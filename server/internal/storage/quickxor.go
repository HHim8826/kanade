package storage

import (
	"encoding/base64"
	"encoding/binary"
	"hash"
	"io"
	"strings"
)

// QuickXorHash is OneDrive's own checksum of a file's content (Microsoft's published algorithm):
// 160 bits that each byte is xored into at a place shifted 11 bits further than the last one's,
// with the length xored into the last 8 bytes. It tells a fetched or uploaded copy from a damaged
// one; it is no SHA-256, and is never taken for one.
type quickXor struct {
	data   [3]uint64 // 64 + 64 + 32 bits
	length uint64
	shift  int
}

const (
	qxWidth    = 160
	qxShift    = 11
	qxLastBits = 32 // the last cell holds 160 - 128 bits
)

// NewQuickXorHash returns a hash.Hash that sums to OneDrive's quickXorHash.
func NewQuickXorHash() hash.Hash { return &quickXor{} }

func (q *quickXor) Write(p []byte) (int, error) {
	n := len(p)
	cell := q.shift / 64
	offset := q.shift % 64
	for i := range min(n, qxWidth) {
		last := cell == len(q.data)-1
		bits := 64
		if last {
			bits = qxLastBits
		}
		if offset <= bits-8 {
			for j := i; j < n; j += qxWidth {
				q.data[cell] ^= uint64(p[j]) << offset
			}
		} else {
			next := cell + 1
			if last {
				next = 0
			}
			var x byte
			for j := i; j < n; j += qxWidth {
				x ^= p[j]
			}
			q.data[cell] ^= uint64(x) << offset
			q.data[next] ^= uint64(x) >> (bits - offset)
		}
		offset += qxShift
		for offset >= bits {
			if last {
				cell = 0
			} else {
				cell++
			}
			offset -= bits
		}
	}
	q.shift = (q.shift + qxShift*(n%qxWidth)) % qxWidth
	q.length += uint64(n)
	return n, nil
}

func (q *quickXor) Sum(b []byte) []byte {
	var out [qxWidth / 8]byte
	binary.LittleEndian.PutUint64(out[0:], q.data[0])
	binary.LittleEndian.PutUint64(out[8:], q.data[1])
	binary.LittleEndian.PutUint32(out[16:], uint32(q.data[2]))
	var length [8]byte
	binary.LittleEndian.PutUint64(length[:], q.length)
	for i, c := range length {
		out[len(out)-len(length)+i] ^= c
	}
	return append(b, out[:]...)
}

func (q *quickXor) Reset()         { *q = quickXor{} }
func (q *quickXor) Size() int      { return qxWidth / 8 }
func (q *quickXor) BlockSize() int { return 64 }

// hashQuickXor is the prefix of an Object's Hash that is a quickXorHash (base64, as OneDrive says it).
const hashQuickXor = "quickxor:"

// Checker checks bytes read or written against a store's own checksum of them: Write them all,
// then Matches. A nil *Checker (no checksum kept) matches anything.
type Checker struct {
	h    hash.Hash
	want string
}

// NewChecker is what checks o's bytes; nil when the store keeps no checksum it can be checked by.
func NewChecker(o Object) *Checker {
	if v, ok := strings.CutPrefix(o.Hash, hashQuickXor); ok && v != "" {
		return &Checker{h: NewQuickXorHash(), want: v}
	}
	return nil
}

func (c *Checker) Write(p []byte) (int, error) { return c.h.Write(p) }

// Matches says whether what was written is what the store said.
func (c *Checker) Matches() bool {
	return c == nil || base64.StdEncoding.EncodeToString(c.h.Sum(nil)) == c.want
}

// Writer is w, also writing to the checker when there is one.
func (c *Checker) Writer(w io.Writer) io.Writer {
	if c == nil {
		return w
	}
	return io.MultiWriter(w, c)
}
