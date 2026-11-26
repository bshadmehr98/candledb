package domain

import (
	"cmp"
	"math"
	"slices"
	"strings"
	"time"
)

// Kind identifies the type class of a Value. The numeric order of the
// constants is the order values of different kinds sort in, so a query that
// orders by a field holding mixed types still has one well-defined order.
type Kind int

const (
	KindNull Kind = iota
	KindBool
	KindNumber
	KindTimestamp
	KindString
	KindArray
	KindMap
)

func (k Kind) String() string {
	switch k {
	case KindNull:
		return "null"
	case KindBool:
		return "bool"
	case KindNumber:
		return "number"
	case KindTimestamp:
		return "timestamp"
	case KindString:
		return "string"
	case KindArray:
		return "list"
	case KindMap:
		return "map"
	}
	return "unknown"
}

// Value is one field value inside a document. It is a closed set of types:
// the unexported method means no other package can add an implementation,
// so a type switch over the types below is exhaustive.
//
// Values are treated as immutable. Code that needs a changed value builds a
// new one; it never modifies an Array or Map it was handed.
type Value interface {
	Kind() Kind
	sealed()
}

type (
	// Null is the JSON null value.
	Null struct{}
	// Bool is a boolean.
	Bool bool
	// Int is a 64-bit integer.
	Int int64
	// Double is a finite 64-bit float. NaN and infinities are rejected at
	// the edges of the system because JSON cannot carry them.
	Double float64
	// Timestamp is an instant in UTC with microsecond precision.
	Timestamp struct{ t time.Time }
	// String is UTF-8 text.
	String string
	// Array is an ordered list of values.
	Array []Value
	// Map is a set of named fields.
	Map map[string]Value
)

func (Null) Kind() Kind      { return KindNull }
func (Bool) Kind() Kind      { return KindBool }
func (Int) Kind() Kind       { return KindNumber }
func (Double) Kind() Kind    { return KindNumber }
func (Timestamp) Kind() Kind { return KindTimestamp }
func (String) Kind() Kind    { return KindString }
func (Array) Kind() Kind     { return KindArray }
func (Map) Kind() Kind       { return KindMap }

func (Null) sealed()      {}
func (Bool) sealed()      {}
func (Int) sealed()       {}
func (Double) sealed()    {}
func (Timestamp) sealed() {}
func (String) sealed()    {}
func (Array) sealed()     {}
func (Map) sealed()       {}

// NewTimestamp converts t to a Timestamp, normalising it to UTC and
// truncating it to microseconds so that it survives a round trip through
// storage and JSON unchanged.
func NewTimestamp(t time.Time) Timestamp {
	return Timestamp{t: t.UTC().Truncate(time.Microsecond)}
}

// Time returns the instant as a time.Time in UTC.
func (ts Timestamp) Time() time.Time { return ts.t }

// Compare orders two values. It returns a negative number when a sorts
// before b, zero when they are equal and a positive number otherwise.
//
// Values of different kinds sort by Kind. Within a kind: false < true,
// numbers by numeric value (Int and Double compare with each other),
// timestamps chronologically, strings by UTF-8 bytes, arrays element by
// element, and maps by their sorted keys and then values.
func Compare(a, b Value) int {
	if c := cmp.Compare(a.Kind(), b.Kind()); c != 0 {
		return c
	}
	switch av := a.(type) {
	case Null:
		return 0
	case Bool:
		bv := b.(Bool)
		switch {
		case av == bv:
			return 0
		case !bool(av):
			return -1
		}
		return 1
	case Int, Double:
		af, ai := NumberKey(a)
		bf, bi := NumberKey(b)
		if c := cmp.Compare(af, bf); c != 0 {
			return c
		}
		return cmp.Compare(ai, bi)
	case Timestamp:
		return av.t.Compare(b.(Timestamp).t)
	case String:
		return strings.Compare(string(av), string(b.(String)))
	case Array:
		bv := b.(Array)
		for i := range min(len(av), len(bv)) {
			if c := Compare(av[i], bv[i]); c != 0 {
				return c
			}
		}
		return cmp.Compare(len(av), len(bv))
	case Map:
		bv := b.(Map)
		ak, bk := av.Keys(), bv.Keys()
		for i := range min(len(ak), len(bk)) {
			if c := strings.Compare(ak[i], bk[i]); c != 0 {
				return c
			}
			if c := Compare(av[ak[i]], bv[bk[i]]); c != 0 {
				return c
			}
		}
		return cmp.Compare(len(ak), len(bk))
	}
	panic("domain: unknown value type")
}

// Equal reports whether two values are the same. Int(1) equals Double(1).
func Equal(a, b Value) bool { return Compare(a, b) == 0 }

// NumberKey returns the pair that defines the order of numbers: the value as
// a float64 and, to break ties between large integers that round to the same
// float, the value as a saturated int64. Comparing the pairs
// lexicographically gives an exact total order across Int and Double.
//
// The storage adapter encodes the same pair into its index keys, which is
// why this function is exported.
func NumberKey(v Value) (float64, int64) {
	switch n := v.(type) {
	case Int:
		return float64(n), int64(n)
	case Double:
		f := float64(n)
		if f == 0 {
			f = 0 // fold -0.0 into +0.0
		}
		switch {
		case f >= math.MaxInt64:
			return f, math.MaxInt64
		case f <= math.MinInt64:
			return f, math.MinInt64
		}
		return f, int64(f)
	}
	panic("domain: NumberKey called on a non-number")
}

// Keys returns the map's field names in sorted order.
func (m Map) Keys() []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// Contains reports whether the array holds an element equal to v.
func (a Array) Contains(v Value) bool {
	for _, e := range a {
		if Equal(e, v) {
			return true
		}
	}
	return false
}

// Size estimates the number of bytes a value occupies. It is used to enforce
// the maximum document size, so it only needs to be proportional to the
// real encoded size, not identical to it.
func Size(v Value) int {
	switch x := v.(type) {
	case Null, Bool:
		return 1
	case Int, Double, Timestamp:
		return 8
	case String:
		return len(x) + 1
	case Array:
		n := 1
		for _, e := range x {
			n += Size(e)
		}
		return n
	case Map:
		n := 1
		for k, e := range x {
			n += len(k) + 1 + Size(e)
		}
		return n
	}
	return 0
}

// Depth returns how deeply arrays and maps are nested inside v. A scalar has
// depth 0.
func Depth(v Value) int {
	deepest := 0
	switch x := v.(type) {
	case Array:
		for _, e := range x {
			deepest = max(deepest, Depth(e)+1)
		}
		return max(deepest, 1)
	case Map:
		for _, e := range x {
			deepest = max(deepest, Depth(e)+1)
		}
		return max(deepest, 1)
	}
	return 0
}
