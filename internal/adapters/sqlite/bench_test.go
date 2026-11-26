package sqlite_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/domain"
)

// Benchmarks measure speed instead of checking behaviour. Run them with
//
//	go test ./internal/adapters/sqlite -bench . -benchmem
//
// The numbers to compare are in each benchmark's comment.

func seed(b *testing.B, n int) app.Store {
	b.Helper()
	ctx := context.Background()
	db := openDB(b)
	const chunk = 500
	for start := 0; start < n; start += chunk {
		var batch []app.Mutation
		for i := start; i < min(start+chunk, n); i++ {
			doc := &domain.Document{
				Path: domain.DocPath(fmt.Sprintf("items/i%07d", i)),
				Data: domain.Map{
					"n":     domain.Int(i),
					"group": domain.Int(i % 100),
					"name":  domain.String(fmt.Sprintf("item %d", i)),
				},
				Version: 1,
			}
			batch = append(batch, app.Mutation{Path: doc.Path, Doc: doc})
		}
		if err := db.Apply(ctx, int64(start/chunk+1), batch); err != nil {
			b.Fatal(err)
		}
	}
	return db
}

// BenchmarkQueryTopTen shows the point of an ordered index scan: fetching
// the ten largest values takes about the same time from 1,000 documents as
// from 50,000, because the scan stops after ten rows.
func BenchmarkQueryTopTen(b *testing.B) {
	for _, size := range []int{1_000, 50_000} {
		b.Run(fmt.Sprintf("docs=%d", size), func(b *testing.B) {
			db := seed(b, size)
			q := domain.Query{
				Collection: "items",
				Orders:     []domain.Order{{Field: domain.FieldPath{"n"}, Desc: true}},
				Limit:      10,
			}
			ctx := context.Background()
			// b.Loop runs the body as many times as needed for a stable
			// measurement and excludes the setup above from the timing.
			for b.Loop() {
				docs, err := db.Query(ctx, q, nil)
				if err != nil || len(docs) != 10 {
					b.Fatalf("%d docs, %v", len(docs), err)
				}
			}
		})
	}
}

// BenchmarkQueryEquality reads the 1% of documents in one group through the
// index on "group".
func BenchmarkQueryEquality(b *testing.B) {
	db := seed(b, 50_000)
	q := domain.Query{
		Collection: "items",
		Filters:    []domain.Filter{{Field: domain.FieldPath{"group"}, Op: domain.OpEq, Value: domain.Int(7)}},
	}
	ctx := context.Background()
	for b.Loop() {
		docs, err := db.Query(ctx, q, nil)
		if err != nil || len(docs) != 500 {
			b.Fatalf("%d docs, %v", len(docs), err)
		}
	}
}

func BenchmarkGet(b *testing.B) {
	db := seed(b, 50_000)
	ctx := context.Background()
	for b.Loop() {
		if _, err := db.Get(ctx, "items/i0025000"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkApply writes one document per commit, the worst case for write
// throughput: every iteration is its own durable transaction.
func BenchmarkApply(b *testing.B) {
	db := seed(b, 0)
	ctx := context.Background()
	doc := &domain.Document{Path: "items/x", Data: domain.Map{"n": domain.Int(1), "name": domain.String("x")}}
	version := int64(0)
	for b.Loop() {
		version++
		if err := db.Apply(ctx, version, []app.Mutation{{Path: doc.Path, Doc: doc}}); err != nil {
			b.Fatal(err)
		}
	}
}
