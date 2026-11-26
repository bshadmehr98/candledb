package app

import (
	"context"
	"fmt"

	"github.com/bshadmehr98/candledb/internal/domain"
	"github.com/bshadmehr98/candledb/internal/rules"
)

// Get returns one document.
//
// The rules are consulted before the "not found" check, so a caller who may
// not read a path cannot use Get to learn whether a document exists there.
func (s *Service) Get(ctx context.Context, p Principal, path domain.DocPath) (domain.Document, error) {
	doc, err := s.load(ctx, path)
	if err != nil {
		return domain.Document{}, err
	}
	if !s.allowed(ctx, p, rules.MethodGet, path, doc, nil) {
		return domain.Document{}, fmt.Errorf("%w: get %s", domain.ErrPermissionDenied, path)
	}
	if doc == nil {
		return domain.Document{}, fmt.Errorf("%w: document %s", domain.ErrNotFound, path)
	}
	return *doc, nil
}

// Query returns the documents selected by q that the caller may read.
//
// Security rules act as a filter here: documents the caller may not list
// are left out, as if they did not exist. Because the filter runs inside the
// store's scan, before the limit, a limited query is still filled with
// readable documents.
func (s *Service) Query(ctx context.Context, p Principal, q domain.Query) ([]domain.Document, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	return s.runQuery(ctx, p, q)
}

func (s *Service) runQuery(ctx context.Context, p Principal, q domain.Query) ([]domain.Document, error) {
	ceiling := s.limits.MaxQueryResults
	if q.Limit > ceiling {
		return nil, fmt.Errorf("%w: limit %d is above the maximum of %d", domain.ErrInvalidArgument, q.Limit, ceiling)
	}
	unlimited := q.Limit == 0
	if unlimited {
		// Ask for one more than the maximum: getting it back proves the
		// result is too large without loading all of it.
		q.Limit = ceiling + 1
	}
	var keep func(domain.Document) bool
	if !p.Admin {
		keep = func(doc domain.Document) bool {
			return s.allowed(ctx, p, rules.MethodList, doc.Path, &doc, nil)
		}
	}
	docs, err := s.store.Query(ctx, q, keep)
	if err != nil {
		return nil, err
	}
	if unlimited && len(docs) > ceiling {
		return nil, fmt.Errorf("%w: the query matches more than %d documents; add a limit and paginate",
			domain.ErrResourceExhausted, ceiling)
	}
	return docs, nil
}
