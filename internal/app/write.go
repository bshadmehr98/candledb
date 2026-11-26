package app

import (
	"context"
	"fmt"
	"time"

	"github.com/bshadmehr98/candledb/internal/domain"
	"github.com/bshadmehr98/candledb/internal/rules"
)

// CommitResult describes a successful commit.
type CommitResult struct {
	// Version is the database version the commit produced. Every document
	// the commit wrote carries it.
	Version int64
	Time    time.Time
}

// Commit applies a batch of writes atomically: either every write takes
// effect or none does.
//
// The whole pipeline (load, check preconditions, compute the new documents,
// consult the rules, store, notify listeners) runs while holding the commit
// lock. There is therefore exactly one writer at a time, which is what makes
// preconditions and rules race-free without any further locking.
func (s *Service) Commit(ctx context.Context, p Principal, writes []domain.Write) (CommitResult, error) {
	if len(writes) == 0 {
		return CommitResult{}, fmt.Errorf("%w: a commit needs at least one write", domain.ErrInvalidArgument)
	}
	if len(writes) > s.limits.MaxBatchWrites {
		return CommitResult{}, fmt.Errorf("%w: a commit may hold at most %d writes",
			domain.ErrInvalidArgument, s.limits.MaxBatchWrites)
	}

	s.commitMu.Lock()
	defer s.commitMu.Unlock()

	now := s.clock.Now().UTC().Truncate(time.Microsecond)
	version := s.version + 1

	// staged tracks documents as the batch changes them, so a later write
	// in the batch sees the effect of an earlier one. A nil entry means
	// "deleted by this batch".
	staged := map[domain.DocPath]*domain.Document{}
	original := map[domain.DocPath]*domain.Document{}
	var order []domain.DocPath

	for i, w := range writes {
		existing, seen := staged[w.Path]
		if !seen {
			var err error
			if existing, err = s.load(ctx, w.Path); err != nil {
				return CommitResult{}, err
			}
			original[w.Path] = existing
			order = append(order, w.Path)
		}
		updated, err := s.stage(ctx, p, w, existing, now, version)
		if err != nil {
			if len(writes) == 1 {
				return CommitResult{}, err
			}
			// In a batch, say which write failed.
			return CommitResult{}, fmt.Errorf("write %d (%s %s): %w", i, w.Op, w.Path, err)
		}
		staged[w.Path] = updated
	}

	batch := make([]Mutation, 0, len(order))
	changes := make([]docChange, 0, len(order))
	for _, path := range order {
		before, after := original[path], staged[path]
		// Listeners hear about every document the commit named, changed or
		// not, so that they learn the new version (see Session.process).
		changes = append(changes, docChange{path: path, before: before, after: after})
		if before == after {
			continue // only verified, or deleted without ever existing
		}
		batch = append(batch, Mutation{Path: path, Doc: after})
	}

	if err := s.store.Apply(ctx, version, batch); err != nil {
		return CommitResult{}, fmt.Errorf("storing commit: %w", err)
	}
	s.version = version
	s.hub.publish(&commitBatch{version: version, changes: changes})
	return CommitResult{Version: version, Time: now}, nil
}

// stage computes the document one write produces and checks that the caller
// is allowed to make that change. It returns nil for a delete.
func (s *Service) stage(ctx context.Context, p Principal, w domain.Write, existing *domain.Document, now time.Time, version int64) (*domain.Document, error) {
	if err := w.Pre.Check(existing); err != nil {
		return nil, err
	}
	if w.Op == domain.OpVerify {
		// Verifying reveals whether the document exists and at which
		// version, so it needs the same permission as reading it.
		if !s.allowedAt(ctx, p, rules.MethodGet, w.Path, existing, nil, now) {
			return nil, domain.ErrPermissionDenied
		}
		return existing, nil
	}
	data, err := w.Apply(existing, now)
	if err != nil {
		return nil, err
	}

	var updated *domain.Document
	method := rules.MethodDelete
	if data != nil {
		updated = &domain.Document{Path: w.Path, Data: data, CreateTime: now, UpdateTime: now, Version: version}
		method = rules.MethodCreate
		if existing != nil {
			updated.CreateTime = existing.CreateTime
			method = rules.MethodUpdate
		}
	}
	if !s.allowedAt(ctx, p, method, w.Path, existing, updated, now) {
		return nil, domain.ErrPermissionDenied
	}
	return updated, nil
}
