package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/bshadmehr98/candledb/internal/domain"
)

func TestParsePaths(t *testing.T) {
	tests := []struct {
		path       string
		document   bool // valid as a document path
		collection bool // valid as a collection path
	}{
		{"users", false, true},
		{"users/alice", true, false},
		{"users/alice/posts", false, true},
		{"users/alice/posts/p1", true, false},
		{"users/a b/posts/é😀", true, false},
		{"", false, false},
		{"/users", false, false},
		{"users/", false, false},
		{"users//alice", false, false},
		{"users/..", false, false},
		{"users/.", false, false},
		{"users/__name__", false, false},
		{"users/a\x00b", false, false},
		{"users/" + strings.Repeat("x", domain.MaxNameBytes+1), false, false},
		{"users/\xff", false, false}, // invalid UTF-8
	}
	for _, tt := range tests {
		_, docErr := domain.ParseDocPath(tt.path)
		if (docErr == nil) != tt.document {
			t.Errorf("ParseDocPath(%q) error = %v, want valid = %v", tt.path, docErr, tt.document)
		}
		_, colErr := domain.ParseCollectionPath(tt.path)
		if (colErr == nil) != tt.collection {
			t.Errorf("ParseCollectionPath(%q) error = %v, want valid = %v", tt.path, colErr, tt.collection)
		}
		if docErr != nil && !errors.Is(docErr, domain.ErrInvalidArgument) {
			t.Errorf("ParseDocPath(%q) error %v does not wrap ErrInvalidArgument", tt.path, docErr)
		}
	}
}

func TestPathParts(t *testing.T) {
	p, err := domain.ParseDocPath("users/alice/posts/p1")
	if err != nil {
		t.Fatal(err)
	}
	if p.Parent() != "users/alice/posts" || p.ID() != "p1" {
		t.Errorf("Parent = %q, ID = %q", p.Parent(), p.ID())
	}
	sub, err := p.Collection("comments")
	if err != nil || sub != "users/alice/posts/p1/comments" {
		t.Errorf("Collection = %q, %v", sub, err)
	}
	doc, err := sub.Doc("c1")
	if err != nil || doc != "users/alice/posts/p1/comments/c1" {
		t.Errorf("Doc = %q, %v", doc, err)
	}
	// An ID containing a slash would silently become a deeper path.
	if _, err := sub.Doc("a/b"); err == nil {
		t.Error("Doc accepted an ID containing a slash")
	}
}

func TestFieldPathGetWithWithout(t *testing.T) {
	city, _ := domain.ParseFieldPath("address.city")
	original := domain.Map{
		"name":    domain.String("Ada"),
		"address": domain.Map{"city": domain.String("London"), "zip": domain.String("N1")},
	}

	if v, ok := original.Get(city); !ok || v != domain.String("London") {
		t.Fatalf("Get = %v, %v", v, ok)
	}
	if _, ok := original.Get(domain.FieldPath{"name", "first"}); ok {
		t.Error("Get descended into a string")
	}

	changed := original.With(city, domain.String("Paris"))
	if v, _ := changed.Get(city); v != domain.String("Paris") {
		t.Errorf("With did not set the field: %v", v)
	}
	// The original must be untouched: values are immutable, and stores hand
	// the same maps to many readers.
	if v, _ := original.Get(city); v != domain.String("London") {
		t.Errorf("With modified its receiver: %v", v)
	}
	if v, _ := changed.Get(domain.FieldPath{"address", "zip"}); v != domain.String("N1") {
		t.Errorf("With lost a sibling field: %v", v)
	}

	created := domain.Map{"x": domain.Int(1)}.With(domain.FieldPath{"x", "y", "z"}, domain.Bool(true))
	if v, ok := created.Get(domain.FieldPath{"x", "y", "z"}); !ok || v != domain.Bool(true) {
		t.Errorf("With did not create intermediate maps: %#v", created)
	}

	removed := original.Without(city)
	if _, ok := removed.Get(city); ok {
		t.Error("Without left the field in place")
	}
	if _, ok := original.Get(city); !ok {
		t.Error("Without modified its receiver")
	}
	if got := original.Without(domain.FieldPath{"missing", "field"}); len(got) != len(original) {
		t.Error("Without of a missing field changed the map")
	}
}

func TestParseFieldPathRejects(t *testing.T) {
	for _, bad := range []string{"", "a..b", ".a", "a.", "$a", "a.$b"} {
		if _, err := domain.ParseFieldPath(bad); err == nil {
			t.Errorf("ParseFieldPath(%q) succeeded", bad)
		}
	}
}
