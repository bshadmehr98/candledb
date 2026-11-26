package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bshadmehr98/candledb/internal/domain"
	"github.com/bshadmehr98/candledb/internal/rules"
)

// Limits bound the resources one request or one client can consume.
type Limits struct {
	MaxBatchWrites       int // writes in one commit
	MaxQueryResults      int // documents returned by one query
	MaxSessions          int // concurrent listen sessions
	MaxTargetsPerSession int // listeners multiplexed on one session
	SessionBuffer        int // commits a slow session may fall behind
}

// DefaultLimits are the limits used unless WithLimits overrides them.
var DefaultLimits = Limits{
	MaxBatchWrites:       500,
	MaxQueryResults:      10_000,
	MaxSessions:          10_000,
	MaxTargetsPerSession: 100,
	SessionBuffer:        256,
}

// Principal is the caller of a use case, as established by the transport
// adapter. The zero value is an anonymous caller.
type Principal struct {
	UID    string
	Claims domain.Map
	// Admin callers bypass security rules. It is set for requests that
	// present the server's admin key.
	Admin bool
	// Expires is when the caller's credentials stop being valid; zero means
	// they do not expire.
	Expires time.Time
}

// Service implements the document use cases. Create one with New; it is
// safe for concurrent use.
type Service struct {
	store  Store
	clock  Clock
	ids    IDGenerator
	logger *slog.Logger
	limits Limits

	// rules is swapped atomically so that a reload never blocks or tears a
	// request that is being evaluated.
	rules atomic.Pointer[rules.Ruleset]

	// commitMu makes commits strictly sequential. A commit holds it
	// exclusively; attaching a listener holds it shared, so the listener's
	// first snapshot and its position in the change stream agree exactly.
	commitMu sync.RWMutex
	version  int64 // version of the last commit; guarded by commitMu

	hub *hub
}

// Option configures a Service. This is the functional options pattern: New
// takes any number of options, each a function that adjusts the Service, so
// new settings can be added without changing New's signature or breaking
// existing callers.
type Option func(*Service)

// WithClock replaces the system clock.
func WithClock(c Clock) Option { return func(s *Service) { s.clock = c } }

// WithIDGenerator replaces the random ID generator.
func WithIDGenerator(g IDGenerator) Option { return func(s *Service) { s.ids = g } }

// WithLogger sets the logger. The default discards everything.
func WithLogger(l *slog.Logger) Option { return func(s *Service) { s.logger = l } }

// WithRules sets the initial security rules. The default denies everything.
func WithRules(rs *rules.Ruleset) Option { return func(s *Service) { s.rules.Store(rs) } }

// WithLimits replaces the default limits.
func WithLimits(l Limits) Option { return func(s *Service) { s.limits = l } }

// New builds a Service on top of a store.
func New(ctx context.Context, store Store, opts ...Option) (*Service, error) {
	s := &Service{
		store:  store,
		clock:  SystemClock{},
		ids:    RandomIDs{},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		limits: DefaultLimits,
		hub:    newHub(),
	}
	s.rules.Store(rules.DenyAll())
	for _, opt := range opts {
		opt(s)
	}
	version, err := store.Version(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading store version: %w", err)
	}
	s.version = version
	return s, nil
}

// SetRules replaces the security rules. Requests already in progress finish
// under the rules they started with.
func (s *Service) SetRules(rs *rules.Ruleset) { s.rules.Store(rs) }

// NewID returns a fresh random document ID.
func (s *Service) NewID() string { return s.ids.NewID() }

// Close ends every listen session and refuses new ones, so that a server
// shutting down is not held open by clients reconnecting. Reads and commits
// keep working, and it does not close the store.
func (s *Service) Close() {
	for _, sess := range s.hub.shutdown() {
		sess.Close()
	}
}

// Stats is a snapshot of the service's counters, for monitoring.
type Stats struct {
	Version  int64
	Sessions int
	Targets  int
}

// Stats reports current counters.
func (s *Service) Stats() Stats {
	s.commitMu.RLock()
	version := s.version
	s.commitMu.RUnlock()
	sessions, targets := s.hub.counts()
	return Stats{Version: version, Sessions: sessions, Targets: targets}
}

// load fetches a document, turning "not found" into a nil pointer. Inside
// the use cases a missing document is an ordinary case, not a failure.
func (s *Service) load(ctx context.Context, path domain.DocPath) (*domain.Document, error) {
	doc, err := s.store.Get(ctx, path)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

// allowed asks the security rules whether the principal may perform method
// on the document at path, now.
func (s *Service) allowed(ctx context.Context, p Principal, method rules.Method, path domain.DocPath, existing, updated *domain.Document) bool {
	return s.allowedAt(ctx, p, method, path, existing, updated, s.clock.Now())
}

// allowedAt is allowed with an explicit request time. A commit passes its
// commit time, so that request.time in a rule is exactly the value a
// serverTimestamp transform writes: the rule
//
//	request.resource.data.created == request.time
//
// then holds precisely when the client asked for a server timestamp.
func (s *Service) allowedAt(ctx context.Context, p Principal, method rules.Method, path domain.DocPath, existing, updated *domain.Document, at time.Time) bool {
	if p.Admin {
		return true
	}
	req := rules.Request{
		Method:      method,
		Path:        path,
		Resource:    existing,
		NewResource: updated,
		Time:        at,
	}
	if p.UID != "" {
		req.Auth = &rules.Auth{UID: p.UID, Token: p.Claims}
	}
	return s.rules.Load().Allow(ctx, req, s.load)
}

// Ping checks that the store answers.
func (s *Service) Ping(ctx context.Context) error {
	_, err := s.store.Version(ctx)
	return err
}
