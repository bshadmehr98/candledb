package instrumented_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bshadmehr98/candledb/internal/adapters/instrumented"
	"github.com/bshadmehr98/candledb/internal/adapters/memory"
	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/app/storetest"
	"github.com/bshadmehr98/candledb/internal/domain"
)

// A decorator must be invisible: wrapped, the store still honours the whole
// storage contract.
func TestWrappedStoreHonoursContract(t *testing.T) {
	storetest.Run(t, func(*testing.T) app.Store { return instrumented.Wrap(memory.NewStore()) })
}

// failing is a store whose Apply and Query always fail. Embedding the
// app.Store interface gives it every method; only the ones under test are
// overridden.
type failing struct{ app.Store }

func (failing) Apply(context.Context, int64, []app.Mutation) error { return errors.New("disk full") }
func (failing) Query(context.Context, domain.Query, func(domain.Document) bool) ([]domain.Document, error) {
	return nil, errors.New("disk gone")
}

func TestCounters(t *testing.T) {
	ctx := context.Background()
	s := instrumented.Wrap(memory.NewStore())

	doc := &domain.Document{Path: "a/b", Data: domain.Map{}}
	if err := s.Apply(ctx, 1, []app.Mutation{{Path: doc.Path, Doc: doc}}); err != nil {
		t.Fatal(err)
	}
	s.Get(ctx, "a/b")
	s.Get(ctx, "a/missing") // not found: counted, but not as an error
	s.Query(ctx, domain.Query{Collection: "a"}, nil)

	if got := s.Applies.Count.Load(); got != 1 {
		t.Errorf("applies = %d", got)
	}
	if got, errs := s.Gets.Count.Load(), s.Gets.Errors.Load(); got != 2 || errs != 0 {
		t.Errorf("gets = %d with %d errors, want 2 and 0", got, errs)
	}
	if got := s.Queries.Count.Load(); got != 1 {
		t.Errorf("queries = %d", got)
	}

	bad := instrumented.Wrap(failing{memory.NewStore()})
	if err := bad.Apply(ctx, 1, nil); err == nil {
		t.Error("the wrapper swallowed the error")
	}
	bad.Query(ctx, domain.Query{Collection: "a"}, nil)
	if bad.Applies.Errors.Load() != 1 || bad.Queries.Errors.Load() != 1 {
		t.Errorf("errors: applies %d, queries %d; want 1 and 1", bad.Applies.Errors.Load(), bad.Queries.Errors.Load())
	}
}
