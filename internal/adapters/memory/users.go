package memory

import (
	"context"
	"fmt"
	"sync"

	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/domain"
)

// UserStore is an app.UserStore that lives in memory.
type UserStore struct {
	mu      sync.Mutex
	byID    map[string]app.User
	byEmail map[string]string // email -> user ID
	tokens  map[string]app.RefreshToken
}

var _ app.UserStore = (*UserStore)(nil)

// NewUserStore returns an empty user store.
func NewUserStore() *UserStore {
	return &UserStore{
		byID:    map[string]app.User{},
		byEmail: map[string]string{},
		tokens:  map[string]app.RefreshToken{},
	}
}

func (s *UserStore) CreateUser(_ context.Context, u app.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, taken := s.byEmail[u.Email]; taken {
		return fmt.Errorf("%w: an account with this email", domain.ErrAlreadyExists)
	}
	s.byID[u.ID] = u
	s.byEmail[u.Email] = u.ID
	return nil
}

func (s *UserStore) UserByEmail(_ context.Context, email string) (app.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byEmail[email]
	if !ok {
		return app.User{}, fmt.Errorf("%w: user", domain.ErrNotFound)
	}
	return s.byID[id], nil
}

func (s *UserStore) UserByID(_ context.Context, id string) (app.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byID[id]
	if !ok {
		return app.User{}, fmt.Errorf("%w: user", domain.ErrNotFound)
	}
	return u, nil
}

func (s *UserStore) PutRefreshToken(_ context.Context, t app.RefreshToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[t.Hash] = t
	return nil
}

func (s *UserStore) TakeRefreshToken(_ context.Context, hash string) (app.RefreshToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[hash]
	if !ok {
		return app.RefreshToken{}, fmt.Errorf("%w: refresh token", domain.ErrNotFound)
	}
	delete(s.tokens, hash)
	return t, nil
}
