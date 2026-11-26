package sqlite

// This file is in package sqlite, not sqlite_test: it tests encodeKey and
// planQuery, which are unexported. An "internal" test like this can reach
// everything in the package.

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bshadmehr98/candledb/internal/adapters/memory"
	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/domain"
	"github.com/bshadmehr98/candledb/internal/domain/domaintest"
)

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// TestKeyEncodingPreservesOrder is the property the whole index design
// rests on: comparing two encoded keys byte by byte gives the same answer
// as comparing the two values.
func TestKeyEncodingPreservesOrder(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	values := make([]domain.Value, 400)
	for i := range values {
		values[i] = domaintest.Value(rng, 3)
	}
	for _, a := range values {
		for _, b := range values {
			want := sign(domain.Compare(a, b))
			got := sign(bytes.Compare(encodeKey(a), encodeKey(b)))
			if got != want {
				t.Fatalf("order mismatch\n a = %#v -> % x\n b = %#v -> % x\n values compare %d, keys compare %d",
					a, encodeKey(a), b, encodeKey(b), want, got)
			}
		}
	}
}

// TestQueriesAgreeWithReference is a differential test. The memory store
// answers queries by brute force with domain.Query.Apply, which is the
// definition of the right answer. The SQLite store takes a completely
// different route (index plans, range scans, paging). Feeding both the same
// random data and the same random queries and demanding identical results
// checks the planner far more thoroughly than hand-written cases could.
func TestQueriesAgreeWithReference(t *testing.T) {
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(42, 43))

	db, err := Open(ctx, filepath.Join(t.TempDir(), "diff.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reference := memory.NewStore()

	var batch []app.Mutation
	var ids []string
	for i := range 250 {
		id := fmt.Sprintf("d%03d", i)
		ids = append(ids, id)
		doc := &domain.Document{Path: domain.DocPath("c/" + id), Data: domaintest.Map(rng, 2), Version: 1}
		batch = append(batch, app.Mutation{Path: doc.Path, Doc: doc})
	}
	for _, store := range []app.Store{db, reference} {
		if err := store.Apply(ctx, 1, batch); err != nil {
			t.Fatal(err)
		}
	}

	fields := []string{"a", "b", "c", "d", "a.a", "b.c"}
	ops := []domain.Operator{
		domain.OpEq, domain.OpNe, domain.OpLt, domain.OpLe, domain.OpGt, domain.OpGe,
		domain.OpIn, domain.OpNotIn, domain.OpArrayContains, domain.OpArrayContainsAny,
	}
	randomField := func() domain.FieldPath { return strings.Split(fields[rng.IntN(len(fields))], ".") }
	randomCursor := func(orders int) *domain.Cursor {
		c := &domain.Cursor{Inclusive: rng.IntN(2) == 0}
		for range orders {
			c.Values = append(c.Values, domaintest.Scalar(rng))
		}
		if orders == 0 || rng.IntN(2) == 0 {
			c.Values = append(c.Values, domain.String(ids[rng.IntN(len(ids))]))
		}
		return c
	}

	total := 3000
	if testing.Short() {
		total = 300
	}
	checked, nonEmpty := 0, 0
	for checked < total {
		q := domain.Query{Collection: "c"}
		for range rng.IntN(3) {
			f := domain.Filter{Field: randomField(), Op: ops[rng.IntN(len(ops))], Value: domaintest.Value(rng, 1)}
			switch f.Op {
			case domain.OpIn, domain.OpNotIn, domain.OpArrayContainsAny:
				f.Value = domain.Array{domaintest.Scalar(rng), domaintest.Scalar(rng), domaintest.Scalar(rng)}
			}
			q.Filters = append(q.Filters, f)
		}
		for range rng.IntN(3) {
			q.Orders = append(q.Orders, domain.Order{Field: randomField(), Desc: rng.IntN(2) == 0})
		}
		if rng.IntN(2) == 0 {
			q.Limit = 1 + rng.IntN(20)
		}
		if rng.IntN(3) == 0 {
			q.Start = randomCursor(len(q.Orders))
		}
		if rng.IntN(3) == 0 {
			q.End = randomCursor(len(q.Orders))
		}
		if q.Validate() != nil {
			continue // e.g. the same field ordered twice
		}
		checked++

		// Sometimes add a keep filter, as the security rules would.
		var keep func(domain.Document) bool
		if rng.IntN(4) == 0 {
			keep = func(d domain.Document) bool { return d.Path.ID()[3]%2 == 0 }
		}

		want, err := reference.Query(ctx, q, keep)
		if err != nil {
			t.Fatal(err)
		}
		got, err := db.Query(ctx, q, keep)
		if err != nil {
			t.Fatalf("query %+v: %v", q, err)
		}
		if w, g := pathsOf(want), pathsOf(got); w != g {
			t.Fatalf("results differ for query %+v\n plan: %+v\n want: %s\n got:  %s", q, planQuery(q), w, g)
		}
		if len(want) > 0 {
			nonEmpty++
		}
	}
	// Guard against the test passing only because every query came back
	// empty, which would compare nothing.
	if nonEmpty < total/4 {
		t.Errorf("only %d of %d random queries returned any documents; the generator is too narrow", nonEmpty, total)
	}
	t.Logf("%d of %d queries returned documents", nonEmpty, total)
}

func pathsOf(docs []domain.Document) string {
	ids := make([]string, len(docs))
	for i, d := range docs {
		ids[i] = d.Path.ID()
	}
	return strings.Join(ids, " ")
}

func TestPlanChoosesOrderedScans(t *testing.T) {
	eq := domain.Filter{Field: domain.FieldPath{"a"}, Op: domain.OpEq, Value: domain.Int(1)}
	gt := domain.Filter{Field: domain.FieldPath{"a"}, Op: domain.OpGt, Value: domain.Int(1)}
	in := domain.Filter{Field: domain.FieldPath{"a"}, Op: domain.OpIn, Value: domain.Array{domain.Int(1), domain.Int(2)}}
	byA := domain.Order{Field: domain.FieldPath{"a"}}
	byB := domain.Order{Field: domain.FieldPath{"b"}}

	tests := []struct {
		name        string
		q           domain.Query
		wantIndex   bool
		wantOrdered bool // an ordered plan can stop at the limit
		wantSpans   int
	}{
		{"no filter", domain.Query{}, false, true, 0},
		{"equality", domain.Query{Filters: []domain.Filter{eq}}, true, true, 1},
		{"range without order", domain.Query{Filters: []domain.Filter{gt}}, true, false, 1},
		{"in", domain.Query{Filters: []domain.Filter{in}}, true, false, 2},
		{"one ordering", domain.Query{Orders: []domain.Order{byA}}, true, true, 1},
		{"range and order on one field", domain.Query{Filters: []domain.Filter{gt}, Orders: []domain.Order{byA}}, true, true, 1},
		{"two orderings", domain.Query{Orders: []domain.Order{byA, byB}}, true, false, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := planQuery(tt.q)
			if p.useIndex != tt.wantIndex || p.ordered != tt.wantOrdered || len(p.spans) != tt.wantSpans {
				t.Errorf("plan = %+v", p)
			}
		})
	}
}
