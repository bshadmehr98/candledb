package sqlite

import (
	"bytes"
	"context"
	"slices"
	"strings"

	"github.com/bshadmehr98/candledb/internal/domain"
)

// This file answers queries. The work is split in two:
//
//   - planQuery picks an access path: which index (if any) to scan, between
//     which keys, in which direction. This is the strategy pattern in its
//     plainest Go form: the plan is a value describing the chosen strategy,
//     and one executor carries out whichever plan it is given.
//   - Query executes the plan, and then applies the full query definition
//     from the domain package to every row it reads.
//
// That second step is what keeps the planner simple and safe. The plan only
// has to produce a superset of the answer, in a known order; domain.Query
// decides what really matches. A planning mistake can make a query slower
// but cannot make it return a wrong document.

// pageSize is how many rows are read from SQLite at a time.
const pageSize = 500

// plan describes one way to read candidate documents for a query.
type plan struct {
	// useIndex selects the idx table; otherwise the docs table is scanned
	// in document ID order.
	useIndex bool
	field    string
	kind     int
	// spans are the key ranges to scan. Most plans have one; "in" and
	// "array-contains-any" have one per listed value.
	spans []span
	// idFrom and idTo bound a docs-table scan (inclusive, "" = open).
	idFrom, idTo string
	desc         bool
	// ordered reports that rows arrive in the query's final order. Then the
	// scan can stop as soon as the limit is reached, which is what makes
	// "the first 10 of a million" cheap.
	ordered bool
}

// span is a range of index keys. A nil bound is open.
type span struct {
	lo, hi         []byte
	loOpen, hiOpen bool // true: the bound itself is excluded
}

// narrow shrinks the span to its intersection with another.
func (s *span) narrow(o span) {
	if o.lo != nil {
		if c := bytes.Compare(o.lo, s.lo); s.lo == nil || c > 0 || (c == 0 && o.loOpen) {
			s.lo, s.loOpen = o.lo, o.loOpen
		}
	}
	if o.hi != nil {
		if c := bytes.Compare(o.hi, s.hi); s.hi == nil || c < 0 || (c == 0 && o.hiOpen) {
			s.hi, s.hiOpen = o.hi, o.hiOpen
		}
	}
}

// filterSpan converts a comparison into the key range that contains every
// value able to satisfy it. Ordering comparisons only match values of the
// same kind, so their range is also clipped to that kind's tag.
func filterSpan(f domain.Filter) (span, bool) {
	key := encodeKey(f.Value)
	tag := kindTag(f.Value.Kind())
	kindOnly := span{lo: []byte{tag}, hi: []byte{tag + 1}, hiOpen: true}
	switch f.Op {
	case domain.OpEq:
		return span{lo: key, hi: key}, true
	case domain.OpGt, domain.OpGe:
		kindOnly.narrow(span{lo: key})
		return kindOnly, true
	case domain.OpLt, domain.OpLe:
		kindOnly.narrow(span{hi: key})
		return kindOnly, true
	}
	return span{}, false
}

func sameField(a domain.FieldPath, b domain.FieldPath) bool { return slices.Equal(a, b) }

// planQuery chooses the access path for q.
func planQuery(q domain.Query) plan {
	// With an ordering, walk the index of the first ordered field in the
	// requested direction, restricted by any filters on that same field.
	if len(q.Orders) > 0 {
		first := q.Orders[0]
		sp := span{}
		for _, f := range q.Filters {
			if fs, ok := filterSpan(f); ok && sameField(f.Field, first.Field) {
				sp.narrow(fs)
			}
		}
		// The first cursor value is a bound on the same field. Using it as
		// the scan's starting key is what makes fetching page 1000 cost
		// about the same as fetching page 1.
		from, to := q.Start, q.End
		if first.Desc {
			from, to = to, from
		}
		if from != nil {
			sp.narrow(span{lo: encodeKey(from.Values[0])})
		}
		if to != nil {
			sp.narrow(span{hi: encodeKey(to.Values[0])})
		}
		return plan{
			useIndex: true, field: first.Field.String(), kind: kindValue,
			spans: []span{sp}, desc: first.Desc,
			// With a second ordering the index order is only a prefix of
			// the final order, so the rows have to be sorted afterwards.
			ordered: len(q.Orders) == 1,
		}
	}

	// Without an ordering the result is in document ID order. An equality
	// on an index yields exactly that order (all keys are equal, so entries
	// sort by ID), which makes it the best choice when available.
	for _, f := range q.Filters {
		switch f.Op {
		case domain.OpEq:
			key := encodeKey(f.Value)
			return plan{useIndex: true, field: f.Field.String(), kind: kindValue,
				spans: []span{{lo: key, hi: key}}, ordered: true}
		case domain.OpArrayContains:
			key := encodeKey(f.Value)
			return plan{useIndex: true, field: f.Field.String(), kind: kindElement,
				spans: []span{{lo: key, hi: key}}, ordered: true}
		}
	}
	for _, f := range q.Filters {
		kind := kindValue
		switch f.Op {
		case domain.OpArrayContainsAny:
			kind = kindElement
			fallthrough
		case domain.OpIn:
			p := plan{useIndex: true, field: f.Field.String(), kind: kind}
			for _, v := range f.Value.(domain.Array) {
				key := encodeKey(v)
				p.spans = append(p.spans, span{lo: key, hi: key})
			}
			return p
		}
	}
	for _, f := range q.Filters {
		if sp, ok := filterSpan(f); ok {
			for _, other := range q.Filters {
				if os, ok := filterSpan(other); ok && sameField(other.Field, f.Field) {
					sp.narrow(os)
				}
			}
			return plan{useIndex: true, field: f.Field.String(), kind: kindValue, spans: []span{sp}}
		}
	}

	// Nothing usable: read the collection itself, in ID order.
	p := plan{ordered: true}
	if q.Start != nil {
		p.idFrom = string(q.Start.Values[0].(domain.String))
	}
	if q.End != nil {
		p.idTo = string(q.End.Values[0].(domain.String))
	}
	return p
}

// position is where a scan has got to: the last row it returned.
type position struct {
	key []byte
	id  string
}

// statement builds the SQL for one page of a scan. Only fixed text is ever
// concatenated; every value from the query travels as a bound parameter.
func (p plan) statement(collection domain.CollectionPath, sp span, after *position, limit int) (string, []any) {
	var sql strings.Builder
	var args []any
	cmpAfter, direction := ">", "ASC"
	if p.desc {
		cmpAfter, direction = "<", "DESC"
	}

	if !p.useIndex {
		sql.WriteString("SELECT NULL, " + docColumns + " FROM docs d WHERE d.parent = ?")
		args = append(args, string(collection))
		if p.idFrom != "" {
			sql.WriteString(" AND d.id >= ?")
			args = append(args, p.idFrom)
		}
		if p.idTo != "" {
			sql.WriteString(" AND d.id <= ?")
			args = append(args, p.idTo)
		}
		if after != nil {
			sql.WriteString(" AND d.id > ?")
			args = append(args, after.id)
		}
		sql.WriteString(" ORDER BY d.id LIMIT ?")
		return sql.String(), append(args, limit)
	}

	sql.WriteString(`SELECT i.key, d.id, d.data, d.create_time, d.update_time, d.version
		FROM idx i JOIN docs d ON d.parent = i.parent AND d.id = i.id
		WHERE i.parent = ? AND i.field = ? AND i.kind = ?`)
	args = append(args, string(collection), p.field, p.kind)
	if sp.lo != nil {
		sql.WriteString(" AND i.key " + pick(sp.loOpen, ">", ">=") + " ?")
		args = append(args, sp.lo)
	}
	if sp.hi != nil {
		sql.WriteString(" AND i.key " + pick(sp.hiOpen, "<", "<=") + " ?")
		args = append(args, sp.hi)
	}
	if after != nil {
		// A row-value comparison: continue strictly after the last row in
		// (key, id) order.
		sql.WriteString(" AND (i.key, i.id) " + cmpAfter + " (?, ?)")
		args = append(args, after.key, after.id)
	}
	sql.WriteString(" ORDER BY i.key " + direction + ", i.id " + direction + " LIMIT ?")
	return sql.String(), append(args, limit)
}

func pick(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}

// page reads up to limit rows of a span, starting after the given position.
func (db *DB) page(ctx context.Context, q domain.Query, p plan, sp span, after *position, limit int) ([]domain.Document, *position, error) {
	query, args := p.statement(q.Collection, sp, after, limit)
	rows, err := db.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var (
		docs []domain.Document
		last *position
	)
	for rows.Next() {
		var key []byte
		doc, err := scanDoc(prefixed{rows, &key}, q.Collection)
		if err != nil {
			return nil, nil, err
		}
		docs = append(docs, doc)
		last = &position{key: key, id: doc.Path.ID()}
	}
	return docs, last, rows.Err()
}

// prefixed adapts a row whose first column is the index key to scanDoc,
// which expects only the document columns.
type prefixed struct {
	row scanner
	key *[]byte
}

func (p prefixed) Scan(dest ...any) error {
	return p.row.Scan(append([]any{p.key}, dest...)...)
}

// Query runs q and returns its results.
//
// Rows are read a page at a time and the database connection is released
// between pages. keep may itself read from the database (a security rule
// calling get), and holding one connection while waiting for another is how
// a connection pool deadlocks under load.
func (db *DB) Query(ctx context.Context, q domain.Query, keep func(domain.Document) bool) ([]domain.Document, error) {
	p := planQuery(q)
	spans := p.spans
	if !p.useIndex {
		spans = []span{{}}
	}
	// A document can appear under several spans (an array holding two of
	// the wanted values), so multi-span plans remember what they have seen.
	var seen map[domain.DocPath]bool
	if len(spans) > 1 {
		seen = map[domain.DocPath]bool{}
	}
	size := pageSize
	if p.ordered && q.Limit > 0 {
		// A small limit needs a small page, but not too small: rows that
		// fail the remaining filters still have to be read past.
		size = min(size, max(q.Limit, 64))
	}

	out := []domain.Document{}
scan:
	for _, sp := range spans {
		var after *position
		for {
			docs, last, err := db.page(ctx, q, p, sp, after, size)
			if err != nil {
				return nil, err
			}
			for _, doc := range docs {
				if !q.Matches(doc) {
					continue
				}
				if p.ordered && !q.BeforeEnd(doc) {
					break scan // in an ordered scan nothing later can qualify
				}
				if !q.InWindow(doc) || seen[doc.Path] {
					continue
				}
				if seen != nil {
					seen[doc.Path] = true
				}
				if keep != nil && !keep(doc) {
					continue
				}
				out = append(out, doc)
				if p.ordered && q.Limit > 0 && len(out) == q.Limit {
					break scan
				}
			}
			if len(docs) < size {
				break
			}
			after = last
		}
	}

	if !p.ordered {
		slices.SortFunc(out, q.Compare)
		if q.Limit > 0 && len(out) > q.Limit {
			out = out[:q.Limit]
		}
	}
	return out, nil
}
