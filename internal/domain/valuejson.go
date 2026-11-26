package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

// Limits on the shape of a document.
const (
	MaxDepth         = 20
	MaxDocumentBytes = 1 << 20 // 1 MiB
	MaxNameBytes     = 1500
)

// timestampKey marks a JSON object that stands for a Timestamp:
//
//	{"$timestamp": "2026-01-02T15:04:05.000000Z"}
//
// JSON has no native time type, so one-key objects whose key starts with "$"
// are reserved for this kind of extension.
const timestampKey = "$timestamp"

// TimestampLayout is the wire format of timestamps: RFC 3339 in UTC with
// exactly six fractional digits.
const TimestampLayout = "2006-01-02T15:04:05.000000Z"

// MarshalJSON implementations. encoding/json calls these whenever it meets
// one of our types, including when they are nested inside an Array or Map.

func (Null) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

func (d Double) MarshalJSON() ([]byte, error) {
	f := float64(d)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, fmt.Errorf("%w: number is not finite", ErrInvalidArgument)
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	// Keep a trace of "this was a double" so 1.0 does not come back as the
	// integer 1 when the document is read again.
	if !strings.ContainsAny(s, ".e") {
		s += ".0"
	}
	return []byte(s), nil
}

func (ts Timestamp) MarshalJSON() ([]byte, error) {
	return []byte(`{"` + timestampKey + `":"` + ts.t.Format(TimestampLayout) + `"}`), nil
}

func (a Array) MarshalJSON() ([]byte, error) {
	if a == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]Value(a))
}

func (m Map) MarshalJSON() ([]byte, error) {
	if m == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(map[string]Value(m))
}

// ParseJSON decodes one JSON value. Integers stay integers: the decoder is
// told to keep numbers as text so that 2^53+1 is not silently rounded.
func ParseJSON(data []byte) (Value, error) {
	tree, err := DecodeJSONTree(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return FromJSON(tree)
}

// ParseMapJSON decodes a JSON object, the form document data is stored in.
func ParseMapJSON(data []byte) (Map, error) {
	v, err := ParseJSON(data)
	if err != nil {
		return nil, err
	}
	m, ok := v.(Map)
	if !ok {
		return nil, fmt.Errorf("%w: document data must be an object", ErrInvalidArgument)
	}
	return m, nil
}

// DecodeJSONTree reads exactly one JSON value from r into the generic shapes
// of encoding/json (map[string]any, []any, json.Number, string, bool, nil).
func DecodeJSONTree(r io.Reader) (any, error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("%w: malformed JSON: %v", ErrInvalidArgument, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: unexpected data after JSON value", ErrInvalidArgument)
	}
	return tree, nil
}

// FromJSON converts a tree produced by DecodeJSONTree into a Value,
// validating field names and nesting depth on the way.
func FromJSON(tree any) (Value, error) { return fromJSON(tree, 0) }

func fromJSON(tree any, depth int) (Value, error) {
	if depth > MaxDepth {
		return nil, fmt.Errorf("%w: value is nested more than %d levels deep", ErrInvalidArgument, MaxDepth)
	}
	switch x := tree.(type) {
	case nil:
		return Null{}, nil
	case bool:
		return Bool(x), nil
	case json.Number:
		return parseNumber(x.String())
	case string:
		return String(x), nil
	case []any:
		out := make(Array, len(x))
		for i, e := range x {
			v, err := fromJSON(e, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	case map[string]any:
		if raw, ok := x[timestampKey]; ok && len(x) == 1 {
			return parseTimestamp(raw)
		}
		out := make(Map, len(x))
		for k, e := range x {
			if err := ValidateFieldName(k); err != nil {
				return nil, err
			}
			v, err := fromJSON(e, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = v
		}
		return out, nil
	}
	return nil, fmt.Errorf("%w: unsupported JSON value of type %T", ErrInvalidArgument, tree)
}

func parseNumber(s string) (Value, error) {
	if !strings.ContainsAny(s, ".eE") {
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return Int(i), nil
		}
		// An integer too large for int64 falls through and becomes a double.
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return nil, fmt.Errorf("%w: number %q is out of range", ErrInvalidArgument, s)
	}
	return Double(f), nil
}

func parseTimestamp(raw any) (Value, error) {
	s, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("%w: %s must be an RFC 3339 string", ErrInvalidArgument, timestampKey)
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil, fmt.Errorf("%w: %s %q is not an RFC 3339 time", ErrInvalidArgument, timestampKey, s)
	}
	return NewTimestamp(t), nil
}

// ValidateFieldName checks one field name. Names may not contain "." (it
// separates the segments of a field path) and may not start with "$" (it is
// reserved for typed values and write transforms).
func ValidateFieldName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: field name is empty", ErrInvalidArgument)
	case len(name) > MaxNameBytes:
		return fmt.Errorf("%w: field name is longer than %d bytes", ErrInvalidArgument, MaxNameBytes)
	case strings.HasPrefix(name, "$"):
		return fmt.Errorf("%w: field name %q starts with the reserved character $", ErrInvalidArgument, name)
	case strings.Contains(name, "."):
		return fmt.Errorf("%w: field name %q contains a dot", ErrInvalidArgument, name)
	}
	return nil
}
