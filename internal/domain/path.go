package domain

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxPathSegments bounds how deeply subcollections can be nested.
const MaxPathSegments = 100

// DocPath is the validated path of a document, such as "users/alice" or
// "users/alice/posts/p1". It always has an even number of segments:
// collection, document, collection, document...
//
// It is a named string type, so it is cheap to copy, comparable and usable
// as a map key, yet the compiler stops a raw string or a CollectionPath from
// being passed where a document path is expected.
type DocPath string

// CollectionPath is the validated path of a collection, such as "users" or
// "users/alice/posts". It always has an odd number of segments.
type CollectionPath string

// ParseDocPath validates s as a document path.
func ParseDocPath(s string) (DocPath, error) {
	n, err := validatePath(s)
	if err != nil {
		return "", err
	}
	if n%2 != 0 {
		return "", fmt.Errorf("%w: %q is a collection path, not a document path", ErrInvalidArgument, s)
	}
	return DocPath(s), nil
}

// ParseCollectionPath validates s as a collection path.
func ParseCollectionPath(s string) (CollectionPath, error) {
	n, err := validatePath(s)
	if err != nil {
		return "", err
	}
	if n%2 != 1 {
		return "", fmt.Errorf("%w: %q is a document path, not a collection path", ErrInvalidArgument, s)
	}
	return CollectionPath(s), nil
}

func validatePath(s string) (segments int, err error) {
	if s == "" {
		return 0, fmt.Errorf("%w: path is empty", ErrInvalidArgument)
	}
	for seg := range strings.SplitSeq(s, "/") {
		segments++
		if err := validateSegment(seg); err != nil {
			return 0, fmt.Errorf("%w in path %q", err, s)
		}
	}
	if segments > MaxPathSegments {
		return 0, fmt.Errorf("%w: path has more than %d segments", ErrInvalidArgument, MaxPathSegments)
	}
	return segments, nil
}

func validateSegment(seg string) error {
	switch {
	case seg == "":
		return fmt.Errorf("%w: empty segment", ErrInvalidArgument)
	case strings.Contains(seg, "/"):
		return fmt.Errorf("%w: segment %q contains a slash", ErrInvalidArgument, seg)
	case seg == "." || seg == "..":
		return fmt.Errorf("%w: segment %q is not allowed", ErrInvalidArgument, seg)
	case len(seg) > MaxNameBytes:
		return fmt.Errorf("%w: segment is longer than %d bytes", ErrInvalidArgument, MaxNameBytes)
	case strings.HasPrefix(seg, "__") && strings.HasSuffix(seg, "__") && len(seg) >= 4:
		return fmt.Errorf("%w: segment %q is reserved", ErrInvalidArgument, seg)
	}
	if !utf8.ValidString(seg) {
		return fmt.Errorf("%w: segment is not valid UTF-8", ErrInvalidArgument)
	}
	for _, r := range seg {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: segment contains a control character", ErrInvalidArgument)
		}
	}
	return nil
}

// Parent returns the collection the document lives in.
func (p DocPath) Parent() CollectionPath {
	return CollectionPath(p[:strings.LastIndexByte(string(p), '/')])
}

// ID returns the last segment of the path, the document's ID within its
// collection.
func (p DocPath) ID() string {
	return string(p[strings.LastIndexByte(string(p), '/')+1:])
}

// Segments splits the path into its parts.
func (p DocPath) Segments() []string { return strings.Split(string(p), "/") }

// Collection returns the path of a subcollection of this document.
func (p DocPath) Collection(name string) (CollectionPath, error) {
	return ParseCollectionPath(string(p) + "/" + name)
}

// Doc returns the path of the document with the given ID in this collection.
func (c CollectionPath) Doc(id string) (DocPath, error) {
	if err := validateSegment(id); err != nil {
		return "", fmt.Errorf("%w in document ID", err)
	}
	return DocPath(string(c) + "/" + id), nil
}

// FieldPath addresses a field inside a document, descending through nested
// maps: FieldPath{"address", "city"} is written "address.city".
type FieldPath []string

// ParseFieldPath splits a dotted path and validates every segment.
func ParseFieldPath(s string) (FieldPath, error) {
	parts := strings.Split(s, ".")
	for _, part := range parts {
		if err := ValidateFieldName(part); err != nil {
			return nil, fmt.Errorf("%w (in field path %q)", err, s)
		}
	}
	if len(parts) > MaxDepth {
		return nil, fmt.Errorf("%w: field path %q is too deep", ErrInvalidArgument, s)
	}
	return FieldPath(parts), nil
}

func (fp FieldPath) String() string { return strings.Join(fp, ".") }

// Get follows the field path into the map. The boolean reports whether the
// field exists; a field that exists and holds null returns (Null{}, true).
func (m Map) Get(fp FieldPath) (Value, bool) {
	var cur Value = m
	for _, name := range fp {
		asMap, ok := cur.(Map)
		if !ok {
			return nil, false
		}
		if cur, ok = asMap[name]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// With returns a copy of the map in which the field at fp holds v. Only the
// maps along the path are copied; everything else is shared with the
// original, which is safe because values are never modified in place.
// Intermediate fields that are missing, or are not maps, become maps.
func (m Map) With(fp FieldPath, v Value) Map {
	out := make(Map, len(m)+1)
	for k, e := range m {
		out[k] = e
	}
	if len(fp) == 1 {
		out[fp[0]] = v
		return out
	}
	child, _ := m[fp[0]].(Map)
	out[fp[0]] = child.With(fp[1:], v)
	return out
}

// Without returns a copy of the map with the field at fp removed. Removing a
// field that does not exist returns the map unchanged.
func (m Map) Without(fp FieldPath) Map {
	if _, ok := m.Get(fp); !ok {
		return m
	}
	out := make(Map, len(m))
	for k, e := range m {
		out[k] = e
	}
	if len(fp) == 1 {
		delete(out, fp[0])
		return out
	}
	out[fp[0]] = m[fp[0]].(Map).Without(fp[1:])
	return out
}
