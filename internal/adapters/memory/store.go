// Package memory provides in-memory implementations of the storage ports.
//
// They keep everything in Go maps and lose it when the process exits. They
// exist for two reasons: tests run against them without touching the disk,
// and they are the shortest possible answer to "what does a storage adapter
// have to do?", which makes them the right place to start reading before
// the SQLite adapter.
package memory

import (
	"context"
	"fmt"
	"sync"

	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/domain"
)

// Store is an app.Store that lives in memory.
type Store struct {
	// mu guards every field below. Readers take it shared, writers
	// exclusively, so any number of reads can run at the same time.
	mu          sync.RWMutex
	collections map[domain.CollectionPath]map[string]domain.Document
	version     int64
}

// This line makes the compiler verify that *Store implements app.Store.
// Without it a missing method would only be reported where a Store is first
// passed to something expecting the interface.
var _ app.Store = (*Store)(nil)

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{collections: map[domain.CollectionPath]map[string]domain.Document{}}
}

func (s *Store) Get(_ context.Context, path domain.DocPath) (domain.Document, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	doc, ok := s.collections[path.Parent()][path.ID()]
	if !ok {
		return domain.Document{}, fmt.Errorf("%w: document %s", domain.ErrNotFound, path)
	}
	return doc, nil
}

func (s *Store) Query(_ context.Context, q domain.Query, keep func(domain.Document) bool) ([]domain.Document, error) {
	// Copy the candidates out and release the lock before filtering. keep
	// may call back into Get, and a goroutine must not take a read lock it
	// already holds: if a writer queued up in between, both would wait
	// forever.
	s.mu.RLock()
	collection := s.collections[q.Collection]
	candidates := make([]domain.Document, 0, len(collection))
	for _, doc := range collection {
		candidates = append(candidates, doc)
	}
	s.mu.RUnlock()

	return q.Apply(candidates, keep), nil
}

func (s *Store) Apply(_ context.Context, version int64, batch []app.Mutation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range batch {
		parent, id := m.Path.Parent(), m.Path.ID()
		if m.Doc == nil {
			delete(s.collections[parent], id)
			if len(s.collections[parent]) == 0 {
				delete(s.collections, parent)
			}
			continue
		}
		if s.collections[parent] == nil {
			s.collections[parent] = map[string]domain.Document{}
		}
		s.collections[parent][id] = *m.Doc
	}
	s.version = version
	return nil
}

func (s *Store) Version(context.Context) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.version, nil
}

func (s *Store) Close() error { return nil }
