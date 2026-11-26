package domain_test

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/bshadmehr98/candledb/internal/domain"
	"github.com/bshadmehr98/candledb/internal/domain/domaintest"
)

// The tests live in package domain_test, not domain. Such an "external"
// test package can only use what the package exports, so the tests exercise
// the package exactly as other packages will.

func ts(sec int64) domain.Timestamp { return domain.NewTimestamp(time.Unix(sec, 0)) }

// TestCompareOrder lists values in the order they must sort and checks every
// pair. A table like this is the usual shape of a Go test: the data sits in
// a slice and one loop does the checking.
func TestCompareOrder(t *testing.T) {
	ascending := []domain.Value{
		domain.Null{},
		domain.Bool(false),
		domain.Bool(true),
		domain.Double(-1e300),
		domain.Int(math.MinInt64),
		domain.Int(-1),
		domain.Double(-0.5),
		domain.Int(0),
		domain.Double(0.5),
		domain.Int(1),
		domain.Double(1.5),
		domain.Int(1 << 53),
		domain.Int(1<<53 + 1), // rounds to the same float as 2^53; the integer tie-break orders it
		domain.Double(1<<53 + 2),
		domain.Int(math.MaxInt64),
		domain.Double(1e300),
		ts(-5),
		ts(0),
		ts(5),
		domain.String(""),
		domain.String("a"),
		domain.String("a\x00"),
		domain.String("ab"),
		domain.String("b"),
		domain.String("é"),
		domain.Array{},
		domain.Array{domain.Int(1)},
		domain.Array{domain.Int(1), domain.Int(0)},
		domain.Array{domain.Int(2)},
		domain.Map{},
		domain.Map{"a": domain.Int(1)},
		domain.Map{"a": domain.Int(1), "b": domain.Int(0)},
		domain.Map{"a": domain.Int(2)},
		domain.Map{"b": domain.Int(0)},
	}
	for i, a := range ascending {
		for j, b := range ascending {
			got := domain.Compare(a, b)
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if sign(got) != want {
				t.Errorf("Compare(%#v, %#v) = %d, want sign %d", a, b, got, want)
			}
		}
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

func TestCompareNumbersAcrossTypes(t *testing.T) {
	equal := [][2]domain.Value{
		{domain.Int(1), domain.Double(1)},
		{domain.Int(0), domain.Double(math.Copysign(0, -1))}, // negative zero
		{domain.Int(1 << 53), domain.Double(1 << 53)},
		{domain.Int(-7), domain.Double(-7)},
	}
	for _, pair := range equal {
		if !domain.Equal(pair[0], pair[1]) {
			t.Errorf("%#v and %#v should be equal", pair[0], pair[1])
		}
	}
	if domain.Equal(domain.Int(1<<53+1), domain.Double(1<<53)) {
		t.Error("2^53+1 must not equal the double 2^53")
	}
}

// TestCompareIsATotalOrder checks, on random values, the properties sorting
// relies on. Sorting with a comparison that breaks them gives results that
// depend on the input order.
func TestCompareIsATotalOrder(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2)) // fixed seed: failures are reproducible
	values := make([]domain.Value, 300)
	for i := range values {
		values[i] = domaintest.Value(rng, 3)
	}
	for _, a := range values {
		if domain.Compare(a, a) != 0 {
			t.Fatalf("Compare(%#v, itself) != 0", a)
		}
		for _, b := range values {
			ab, ba := sign(domain.Compare(a, b)), sign(domain.Compare(b, a))
			if ab != -ba {
				t.Fatalf("Compare is not antisymmetric for %#v and %#v", a, b)
			}
		}
	}
	// Transitivity: after sorting, every earlier element must compare <=
	// every later one, not only its neighbour.
	slices.SortFunc(values, domain.Compare)
	for i := range values {
		for j := i + 1; j < len(values); j++ {
			if domain.Compare(values[i], values[j]) > 0 {
				t.Fatalf("order is not transitive: %#v sorted before %#v", values[i], values[j])
			}
		}
	}
}

func TestJSONRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for range 2000 {
		original := domaintest.Value(rng, 4)
		encoded, err := json.Marshal(original)
		if err != nil {
			t.Fatalf("marshal %#v: %v", original, err)
		}
		decoded, err := domain.ParseJSON(encoded)
		if err != nil {
			t.Fatalf("parse %s: %v", encoded, err)
		}
		if !domain.Equal(original, decoded) {
			t.Fatalf("round trip changed the value\n before: %#v\n json:   %s\n after:  %#v", original, encoded, decoded)
		}
	}
}

func TestJSONKeepsNumberTypes(t *testing.T) {
	tests := []struct {
		json string
		want domain.Value
	}{
		{`1`, domain.Int(1)},
		{`1.0`, domain.Double(1)},
		{`-3`, domain.Int(-3)},
		{`1e3`, domain.Double(1000)},
		{`9007199254740993`, domain.Int(9007199254740993)},
		{`9223372036854775808`, domain.Double(9223372036854775808)}, // one past int64
	}
	for _, tt := range tests {
		got, err := domain.ParseJSON([]byte(tt.json))
		if err != nil {
			t.Errorf("ParseJSON(%s): %v", tt.json, err)
			continue
		}
		// Comparing with == checks the Go type as well as the number.
		if got != tt.want {
			t.Errorf("ParseJSON(%s) = %#v, want %#v", tt.json, got, tt.want)
		}
	}

	// A double keeps its decimal point when written, so it reads back as a
	// double.
	out, _ := json.Marshal(domain.Map{"d": domain.Double(2), "i": domain.Int(2)})
	if string(out) != `{"d":2.0,"i":2}` {
		t.Errorf("marshal = %s", out)
	}
}

func TestJSONSpecialForms(t *testing.T) {
	v, err := domain.ParseJSON([]byte(`{"at": {"$timestamp": "2026-03-04T05:06:07.123456789+02:00"}}`))
	if err != nil {
		t.Fatal(err)
	}
	got := v.(domain.Map)["at"].(domain.Timestamp).Time()
	want := time.Date(2026, 3, 4, 3, 6, 7, 123456000, time.UTC) // UTC, cut to microseconds
	if !got.Equal(want) {
		t.Errorf("timestamp = %v, want %v", got, want)
	}

	empty, _ := json.Marshal(domain.Map{"list": domain.Array(nil), "map": domain.Map(nil)})
	if string(empty) != `{"list":[],"map":{}}` {
		t.Errorf("nil containers marshal as %s", empty)
	}
}

func TestParseJSONRejects(t *testing.T) {
	deep := ""
	for range domain.MaxDepth + 2 {
		deep += `{"a":`
	}
	deep += "1"
	for range domain.MaxDepth + 2 {
		deep += "}"
	}
	bad := map[string]string{
		"malformed":         `{"a":`,
		"trailing data":     `{} {}`,
		"empty field name":  `{"": 1}`,
		"dot in field name": `{"a.b": 1}`,
		"reserved prefix":   `{"$x": 1}`,
		"bad timestamp":     `{"$timestamp": "yesterday"}`,
		"huge number":       `1e999`,
		"too deep":          deep,
	}
	for name, input := range bad {
		// t.Run gives each case its own name in the output and lets
		// "go test -run TestParseJSONRejects/too_deep" run one alone.
		t.Run(name, func(t *testing.T) {
			if _, err := domain.ParseJSON([]byte(input)); err == nil {
				t.Errorf("ParseJSON accepted %s", input)
			}
		})
	}
}

func TestSizeAndDepth(t *testing.T) {
	v := domain.Map{"a": domain.Array{domain.Map{"b": domain.Int(1)}}}
	if got := domain.Depth(v); got != 3 {
		t.Errorf("Depth = %d, want 3", got)
	}
	if got := domain.Depth(domain.Int(1)); got != 0 {
		t.Errorf("Depth of a scalar = %d, want 0", got)
	}
	big := domain.Map{"blob": domain.String(make([]byte, domain.MaxDocumentBytes))}
	if err := domain.ValidateData(big); err == nil {
		t.Error("ValidateData accepted an oversized document")
	}
}
