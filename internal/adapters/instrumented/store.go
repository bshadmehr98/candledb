// Package instrumented wraps an app.Store to count and time its operations.
//
// It is an example of the decorator pattern. The wrapper implements the
// same interface as the thing it wraps and forwards every call, adding
// behaviour before and after. Neither the code that uses the store nor the
// store itself changes: the wrapper is slotted in between where the program
// is assembled (cmd/candledb).
package instrumented

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/domain"
)

// OpStats holds the counters for one kind of operation. The fields are
// atomic so they can be updated from many goroutines without a lock.
type OpStats struct {
	Count  atomic.Int64
	Errors atomic.Int64
	Nanos  atomic.Int64 // total time spent
}

func (o *OpStats) record(start time.Time, err error) {
	o.Count.Add(1)
	o.Nanos.Add(int64(time.Since(start)))
	if err != nil {
		o.Errors.Add(1)
	}
}

// Store is an app.Store that records statistics about another one.
type Store struct {
	next app.Store

	Gets    OpStats
	Queries OpStats
	Applies OpStats
}

var _ app.Store = (*Store)(nil)

// Wrap returns a Store that forwards to next.
func Wrap(next app.Store) *Store { return &Store{next: next} }

func (s *Store) Get(ctx context.Context, path domain.DocPath) (doc domain.Document, err error) {
	// "Not found" is an ordinary answer, not a storage failure.
	defer func(start time.Time) {
		if err != nil && isNotFound(err) {
			s.Gets.record(start, nil)
			return
		}
		s.Gets.record(start, err)
	}(time.Now())
	return s.next.Get(ctx, path)
}

func (s *Store) Query(ctx context.Context, q domain.Query, keep func(domain.Document) bool) (docs []domain.Document, err error) {
	defer func(start time.Time) { s.Queries.record(start, err) }(time.Now())
	return s.next.Query(ctx, q, keep)
}

func (s *Store) Apply(ctx context.Context, version int64, batch []app.Mutation) (err error) {
	defer func(start time.Time) { s.Applies.record(start, err) }(time.Now())
	return s.next.Apply(ctx, version, batch)
}

func (s *Store) Version(ctx context.Context) (int64, error) { return s.next.Version(ctx) }

func (s *Store) Close() error { return s.next.Close() }

func isNotFound(err error) bool { return errors.Is(err, domain.ErrNotFound) }
