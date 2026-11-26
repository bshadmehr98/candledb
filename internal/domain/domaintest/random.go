// Package domaintest generates random domain values for property tests:
// tests that check a rule ("encoding preserves order") against many
// generated inputs instead of a handful of hand-picked ones.
package domaintest

import (
	"math"
	"math/rand/v2"
	"time"

	"github.com/bshadmehr98/candledb/internal/domain"
)

// interesting numbers sit on the boundaries where number handling tends to
// break: zero, the edge of exact float integers (2^53) and the int64 limits.
var interestingInts = []int64{
	0, 1, -1, 2, 10, 1 << 53, 1<<53 + 1, -(1 << 53), -(1<<53 + 1),
	math.MaxInt64, math.MaxInt64 - 1, math.MinInt64, math.MinInt64 + 1,
}

var interestingFloats = []float64{
	0, 1, -1, 0.5, -0.5, 1.5, 1 << 53, 1<<53 + 2, -(1 << 53),
	math.MaxInt64, math.MinInt64, 1e300, -1e300, math.SmallestNonzeroFloat64,
}

var interestingStrings = []string{
	"", "a", "b", "ab", "a\x00", "a\x00b", "a\x01", "\x00", "é", "z", "日本", "😀", "￿",
}

// Scalar returns a random value that is not an array or a map.
func Scalar(rng *rand.Rand) domain.Value {
	switch rng.IntN(8) {
	case 0:
		return domain.Null{}
	case 1:
		return domain.Bool(rng.IntN(2) == 0)
	case 2:
		return domain.Int(interestingInts[rng.IntN(len(interestingInts))])
	case 3:
		return domain.Int(rng.IntN(20) - 10)
	case 4:
		return domain.Double(interestingFloats[rng.IntN(len(interestingFloats))])
	case 5:
		return domain.Double(float64(rng.IntN(40)-20) / 4)
	case 6:
		return domain.NewTimestamp(time.Unix(int64(rng.IntN(2000))-1000, int64(rng.IntN(3))*1000))
	}
	return domain.String(interestingStrings[rng.IntN(len(interestingStrings))])
}

// Value returns a random value nested at most depth levels deep.
func Value(rng *rand.Rand, depth int) domain.Value {
	if depth <= 0 || rng.IntN(4) != 0 {
		return Scalar(rng)
	}
	if rng.IntN(2) == 0 {
		arr := make(domain.Array, rng.IntN(4))
		for i := range arr {
			arr[i] = Value(rng, depth-1)
		}
		return arr
	}
	return Map(rng, depth-1)
}

// Map returns a random map with field names drawn from a small alphabet, so
// that independently generated maps often share fields.
func Map(rng *rand.Rand, depth int) domain.Map {
	names := []string{"a", "b", "c", "d"}
	m := domain.Map{}
	for range rng.IntN(4) {
		m[names[rng.IntN(len(names))]] = Value(rng, depth)
	}
	return m
}
