package sqlite

import (
	"encoding/binary"
	"math"

	"github.com/bshadmehr98/candledb/internal/domain"
)

// This file turns a domain.Value into a byte string whose plain byte order
// (the order memcmp and SQLite's BLOB comparison use) is the same as
// domain.Compare's order of the values:
//
//	Compare(a, b) < 0   exactly when   bytes.Compare(encodeKey(a), encodeKey(b)) < 0
//
// With that property an ordinary B-tree index over the encoded bytes can
// answer "all documents where age >= 18, in order" with a single range scan,
// for any mix of value types.
//
// Each value starts with a tag byte for its kind. The tags increase in the
// order of domain.Kind, which sorts values of different kinds correctly
// before any payload is looked at.
const (
	tagNull      byte = 0x05
	tagBool      byte = 0x10
	tagNumber    byte = 0x20
	tagTimestamp byte = 0x30
	tagString    byte = 0x40
	tagArray     byte = 0x50
	tagMap       byte = 0x60

	// terminator ends arrays and maps. It is lower than every tag, so a
	// container that is a prefix of another sorts first.
	terminator byte = 0x00
)

// kindTag returns the tag byte for a kind.
func kindTag(k domain.Kind) byte {
	switch k {
	case domain.KindNull:
		return tagNull
	case domain.KindBool:
		return tagBool
	case domain.KindNumber:
		return tagNumber
	case domain.KindTimestamp:
		return tagTimestamp
	case domain.KindString:
		return tagString
	case domain.KindArray:
		return tagArray
	}
	return tagMap
}

// encodeKey returns the order-preserving encoding of v.
func encodeKey(v domain.Value) []byte { return appendKey(nil, v) }

// appendKey appends the encoding of v to buf and returns the extended
// slice, in the style of the standard library's strconv.AppendInt. Building
// into one growing buffer avoids allocating a new slice per nested value.
func appendKey(buf []byte, v domain.Value) []byte {
	switch x := v.(type) {
	case domain.Null:
		return append(buf, tagNull)
	case domain.Bool:
		if x {
			return append(buf, tagBool, 1)
		}
		return append(buf, tagBool, 0)
	case domain.Int, domain.Double:
		// The same (float, integer) pair that domain.Compare orders numbers
		// by, each made sortable as unsigned big-endian bytes.
		f, i := domain.NumberKey(v)
		buf = append(buf, tagNumber)
		buf = binary.BigEndian.AppendUint64(buf, sortableFloat(f))
		return binary.BigEndian.AppendUint64(buf, sortableInt(i))
	case domain.Timestamp:
		buf = append(buf, tagTimestamp)
		return binary.BigEndian.AppendUint64(buf, sortableInt(x.Time().UnixMicro()))
	case domain.String:
		buf = append(buf, tagString)
		return appendEscaped(buf, string(x))
	case domain.Array:
		buf = append(buf, tagArray)
		for _, e := range x {
			buf = appendKey(buf, e)
		}
		return append(buf, terminator)
	case domain.Map:
		buf = append(buf, tagMap)
		for _, k := range x.Keys() {
			buf = append(buf, tagString)
			buf = appendEscaped(buf, k)
			buf = appendKey(buf, x[k])
		}
		return append(buf, terminator)
	}
	panic("sqlite: unknown value type")
}

// sortableInt maps int64 to uint64 so that unsigned order equals signed
// order: flipping the sign bit moves the negative half below the positive.
func sortableInt(i int64) uint64 { return uint64(i) ^ (1 << 63) }

// sortableFloat does the same for float64. Positive floats already compare
// correctly as unsigned integers once the sign bit is set; negative floats
// compare backwards, so all their bits are inverted.
func sortableFloat(f float64) uint64 {
	bits := math.Float64bits(f)
	if bits&(1<<63) != 0 {
		return ^bits
	}
	return bits | (1 << 63)
}

// appendEscaped writes a string so that it can be followed by more data
// without ambiguity. The string ends with the two bytes 00 01; a zero byte
// inside the string is written as 00 FF. Because 01 < FF, a string still
// sorts before any longer string it is a prefix of.
func appendEscaped(buf []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		if s[i] == 0x00 {
			buf = append(buf, 0x00, 0xFF)
		} else {
			buf = append(buf, s[i])
		}
	}
	return append(buf, 0x00, 0x01)
}
