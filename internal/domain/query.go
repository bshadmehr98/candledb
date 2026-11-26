package domain

import (
	"fmt"
	"slices"
	"strings"
)

// Operator is a comparison used in a query filter.
type Operator string

const (
	OpEq               Operator = "=="
	OpNe               Operator = "!="
	OpLt               Operator = "<"
	OpLe               Operator = "<="
	OpGt               Operator = ">"
	OpGe               Operator = ">="
	OpIn               Operator = "in"
	OpNotIn            Operator = "not-in"
	OpArrayContains    Operator = "array-contains"
	OpArrayContainsAny Operator = "array-contains-any"
)

// Limits on the shape of a query.
const (
	MaxFilters      = 30
	MaxOrders       = 10
	MaxDisjunctions = 30 // values in an in / not-in / array-contains-any list
)

// Filter keeps the documents whose field compares to Value as Op demands.
type Filter struct {
	Field FieldPath
	Op    Operator
	Value Value
}

// Order sorts results by one field.
type Order struct {
	Field FieldPath
	Desc  bool
}

// Cursor marks a position in the ordered results. Values line up with the
// query's Orders; one extra trailing String value, if present, is a document
// ID and pins the position to a single document.
type Cursor struct {
	Values []Value
	// Inclusive decides whether a document exactly at the position is part
	// of the results (startAt / endAt) or not (startAfter / endBefore).
	Inclusive bool
}

// Query selects documents from one collection.
//
// Results are ordered by the Orders and then by document ID, in the
// direction of the last Order, so the order is always total. A document that
// lacks a field named in Orders is not part of the results.
type Query struct {
	Collection CollectionPath
	Filters    []Filter
	Orders     []Order
	Limit      int // 0 means no limit
	Start      *Cursor
	End        *Cursor
}

// Validate checks that the query is well formed.
func (q Query) Validate() error {
	if _, err := ParseCollectionPath(string(q.Collection)); err != nil {
		return err
	}
	if len(q.Filters) > MaxFilters {
		return fmt.Errorf("%w: more than %d filters", ErrInvalidArgument, MaxFilters)
	}
	if len(q.Orders) > MaxOrders {
		return fmt.Errorf("%w: more than %d orderings", ErrInvalidArgument, MaxOrders)
	}
	if q.Limit < 0 {
		return fmt.Errorf("%w: limit is negative", ErrInvalidArgument)
	}
	for _, f := range q.Filters {
		if err := f.validate(); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, o := range q.Orders {
		if len(o.Field) == 0 {
			return fmt.Errorf("%w: ordering without a field", ErrInvalidArgument)
		}
		if seen[o.Field.String()] {
			return fmt.Errorf("%w: field %s is ordered twice", ErrInvalidArgument, o.Field)
		}
		seen[o.Field.String()] = true
	}
	for _, c := range []*Cursor{q.Start, q.End} {
		if c == nil {
			continue
		}
		if len(c.Values) == 0 || len(c.Values) > len(q.Orders)+1 {
			return fmt.Errorf("%w: a cursor needs between 1 and %d values for this query",
				ErrInvalidArgument, len(q.Orders)+1)
		}
		if len(c.Values) == len(q.Orders)+1 {
			if _, ok := c.Values[len(q.Orders)].(String); !ok {
				return fmt.Errorf("%w: the last cursor value must be a document ID", ErrInvalidArgument)
			}
		}
	}
	return nil
}

func (f Filter) validate() error {
	if len(f.Field) == 0 {
		return fmt.Errorf("%w: filter without a field", ErrInvalidArgument)
	}
	if f.Value == nil {
		return fmt.Errorf("%w: filter on %s has no value", ErrInvalidArgument, f.Field)
	}
	switch f.Op {
	case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe, OpArrayContains:
		return nil
	case OpIn, OpNotIn, OpArrayContainsAny:
		list, ok := f.Value.(Array)
		if !ok || len(list) == 0 || len(list) > MaxDisjunctions {
			return fmt.Errorf("%w: %q needs a list of 1 to %d values", ErrInvalidArgument, f.Op, MaxDisjunctions)
		}
		return nil
	}
	return fmt.Errorf("%w: unknown operator %q", ErrInvalidArgument, f.Op)
}

// Matches reports whether the data passes the filter. Every operator
// requires the field to exist; the ordering operators also require the
// field to be of the same kind as the filter value, so "age > 5" never
// matches a document whose age is a string.
func (f Filter) Matches(data Map) bool {
	v, ok := data.Get(f.Field)
	if !ok {
		return false
	}
	switch f.Op {
	case OpEq:
		return Equal(v, f.Value)
	case OpNe:
		return !Equal(v, f.Value)
	case OpLt, OpLe, OpGt, OpGe:
		if v.Kind() != f.Value.Kind() {
			return false
		}
		c := Compare(v, f.Value)
		switch f.Op {
		case OpLt:
			return c < 0
		case OpLe:
			return c <= 0
		case OpGt:
			return c > 0
		default:
			return c >= 0
		}
	case OpIn:
		return f.Value.(Array).Contains(v)
	case OpNotIn:
		return !f.Value.(Array).Contains(v)
	case OpArrayContains:
		arr, isArray := v.(Array)
		return isArray && arr.Contains(f.Value)
	case OpArrayContainsAny:
		arr, isArray := v.(Array)
		return isArray && slices.ContainsFunc(f.Value.(Array), arr.Contains)
	}
	return false
}

// Matches reports whether a document belongs to the query, ignoring the
// cursors and the limit: it is in the right collection, passes every filter
// and has every field the query orders by.
func (q Query) Matches(doc Document) bool {
	if doc.Path.Parent() != q.Collection {
		return false
	}
	for _, f := range q.Filters {
		if !f.Matches(doc.Data) {
			return false
		}
	}
	for _, o := range q.Orders {
		if _, ok := doc.Data.Get(o.Field); !ok {
			return false
		}
	}
	return true
}

// IDDesc reports whether the final tie-break on document ID runs in
// descending order. It follows the direction of the last ordering.
func (q Query) IDDesc() bool {
	return len(q.Orders) > 0 && q.Orders[len(q.Orders)-1].Desc
}

// Compare orders two documents that both match the query.
func (q Query) Compare(a, b Document) int {
	for _, o := range q.Orders {
		av, _ := a.Data.Get(o.Field)
		bv, _ := b.Data.Get(o.Field)
		if c := Compare(av, bv); c != 0 {
			return flip(c, o.Desc)
		}
	}
	return flip(strings.Compare(a.Path.ID(), b.Path.ID()), q.IDDesc())
}

func flip(c int, desc bool) int {
	if desc {
		return -c
	}
	return c
}

// compareToCursor places a matching document relative to a cursor: negative
// when the document sorts before the cursor position, zero when it is at it.
// Only as many fields as the cursor has values take part.
func (q Query) compareToCursor(doc Document, c *Cursor) int {
	for i, cv := range c.Values {
		if i == len(q.Orders) {
			return flip(strings.Compare(doc.Path.ID(), string(cv.(String))), q.IDDesc())
		}
		dv, _ := doc.Data.Get(q.Orders[i].Field)
		if r := Compare(dv, cv); r != 0 {
			return flip(r, q.Orders[i].Desc)
		}
	}
	return 0
}

// AfterStart reports whether a matching document is at or past the start
// cursor. With no start cursor every document is.
func (q Query) AfterStart(doc Document) bool {
	if q.Start == nil {
		return true
	}
	c := q.compareToCursor(doc, q.Start)
	return c > 0 || (c == 0 && q.Start.Inclusive)
}

// BeforeEnd reports whether a matching document is before the end cursor.
// With no end cursor every document is.
func (q Query) BeforeEnd(doc Document) bool {
	if q.End == nil {
		return true
	}
	c := q.compareToCursor(doc, q.End)
	return c < 0 || (c == 0 && q.End.Inclusive)
}

// InWindow reports whether a matching document lies between the cursors.
func (q Query) InWindow(doc Document) bool {
	return q.AfterStart(doc) && q.BeforeEnd(doc)
}

// Apply runs the query over a set of candidate documents in memory: filter,
// sort, cut to the cursor window, then limit. keep, when not nil, is an
// extra filter applied before the limit.
//
// This is the reference definition of what a query returns. A storage
// adapter may use indexes to get there faster, but it must return exactly
// what Apply would.
func (q Query) Apply(candidates []Document, keep func(Document) bool) []Document {
	out := make([]Document, 0, len(candidates))
	for _, doc := range candidates {
		if q.Matches(doc) && q.InWindow(doc) && (keep == nil || keep(doc)) {
			out = append(out, doc)
		}
	}
	slices.SortFunc(out, q.Compare)
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out
}
