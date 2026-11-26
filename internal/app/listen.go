package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/bshadmehr98/candledb/internal/domain"
	"github.com/bshadmehr98/candledb/internal/rules"
)

// This file implements realtime listeners with the observer pattern: the
// commit path is the subject, sessions are the observers, and the hub is
// the registry that connects them.
//
//	Commit --publish--> hub --enqueue--> Session.queue --run--> Session.Events()
//
// The commit path must never wait for a slow client, so publishing only
// drops a pointer into each interested session's buffered queue. Each
// session has its own goroutine that turns queued commits into events at
// whatever pace its client can read them.

// ErrSessionClosed is returned by operations on a session that has ended.
var ErrSessionClosed = errors.New("listen session is closed")

// ChangeKind says what happened to a document from a listener's viewpoint.
type ChangeKind string

const (
	ChangeAdded    ChangeKind = "added"
	ChangeModified ChangeKind = "modified"
	ChangeRemoved  ChangeKind = "removed"
)

// DocChange is one entry of an Event. For ChangeRemoved only Doc.Path is set.
type DocChange struct {
	Kind ChangeKind
	Doc  domain.Document
}

// Event is one message to a listening client about one of its targets.
type Event struct {
	// Target is the client-chosen ID of the listener the event belongs to.
	Target int
	// Version is the database version the event brings the target up to.
	Version int64
	// Reset is true for the first event of a target: Changes then holds the
	// complete current result, replacing anything the client had.
	Reset   bool
	Changes []DocChange
}

// Target is what a client listens to: exactly one of a document or a query.
type Target struct {
	Doc   domain.DocPath
	Query *domain.Query
}

// docChange is the before and after of one document in a commit. Either
// pointer may be nil: before is nil for a create, after is nil for a delete.
type docChange struct {
	path          domain.DocPath
	before, after *domain.Document
}

// commitBatch is everything one commit changed. It is shared, read-only,
// between all sessions it is delivered to.
type commitBatch struct {
	version int64
	changes []docChange
}

// hub is the registry of who listens to what. It answers one question for
// the commit path: which sessions care about this document?
type hub struct {
	mu           sync.Mutex
	sessions     map[string]*Session
	byDoc        map[domain.DocPath]map[*Session]int
	byCollection map[domain.CollectionPath]map[*Session]int
	targets      int
	// closed is set when the service shuts down; no session may open after.
	closed bool
}

func newHub() *hub {
	return &hub{
		sessions:     map[string]*Session{},
		byDoc:        map[domain.DocPath]map[*Session]int{},
		byCollection: map[domain.CollectionPath]map[*Session]int{},
	}
}

func (h *hub) counts() (sessions, targets int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sessions), h.targets
}

// shutdown stops new sessions from opening and returns the open ones.
func (h *hub) shutdown() []*Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	out := make([]*Session, 0, len(h.sessions))
	for _, s := range h.sessions {
		out = append(out, s)
	}
	return out
}

// interest adds (delta = +1) or removes (delta = -1) one target of a session
// from the lookup tables. The tables count targets per session so that two
// listeners on the same collection are removed independently.
func (h *hub) interest(s *Session, t *target, delta int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.targets += delta
	if t.query != nil {
		adjust(h.byCollection, t.query.Collection, s, delta)
	} else {
		adjust(h.byDoc, t.doc, s, delta)
	}
}

// adjust is generic over the key type so that one function serves both the
// by-document and the by-collection table.
func adjust[K comparable](table map[K]map[*Session]int, key K, s *Session, delta int) {
	set := table[key]
	if set == nil {
		set = map[*Session]int{}
		table[key] = set
	}
	set[s] += delta
	if set[s] <= 0 {
		delete(set, s)
	}
	if len(set) == 0 {
		delete(table, key)
	}
}

// publish hands a commit to every session with a target that could be
// affected. It is called with the commit lock held, so batches reach each
// session in commit order.
func (h *hub) publish(b *commitBatch) {
	h.mu.Lock()
	interested := map[*Session]struct{}{}
	for _, c := range b.changes {
		for s := range h.byDoc[c.path] {
			interested[s] = struct{}{}
		}
		for s := range h.byCollection[c.path.Parent()] {
			interested[s] = struct{}{}
		}
	}
	h.mu.Unlock()

	for s := range interested {
		s.enqueue(queueItem{batch: b})
	}
}

// Session is one client's realtime connection. Any number of targets
// (listeners) are multiplexed on it. Read its events from Events until that
// channel is closed.
type Session struct {
	id        string
	svc       *Service
	principal Principal

	mu      sync.Mutex
	targets map[int]*target

	queue  chan queueItem // commits and first snapshots, in order
	events chan Event     // what the transport adapter reads
	done   chan struct{}  // closed when the session ends
	once   sync.Once
}

// queueItem is either a commit to process or a ready-made event to forward.
type queueItem struct {
	batch *commitBatch
	event *Event
}

type target struct {
	id    int
	doc   domain.DocPath
	query *domain.Query
	// known is kept only for queries with a limit: the documents currently
	// in the result and their versions. Such a query cannot be maintained
	// from changes alone, because when a document leaves the result
	// another one has to take its place.
	known map[domain.DocPath]int64
}

// OpenSession starts a listen session for the caller.
func (s *Service) OpenSession(p Principal) (*Session, error) {
	sess := &Session{
		id:        s.ids.NewID(),
		svc:       s,
		principal: p,
		targets:   map[int]*target{},
		queue:     make(chan queueItem, s.limits.SessionBuffer),
		events:    make(chan Event, 16),
		done:      make(chan struct{}),
	}
	s.hub.mu.Lock()
	if s.hub.closed {
		s.hub.mu.Unlock()
		return nil, fmt.Errorf("%w: the server is shutting down", domain.ErrUnavailable)
	}
	if len(s.hub.sessions) >= s.limits.MaxSessions {
		s.hub.mu.Unlock()
		return nil, fmt.Errorf("%w: too many listen sessions", domain.ErrResourceExhausted)
	}
	s.hub.sessions[sess.id] = sess
	s.hub.mu.Unlock()

	go sess.run()
	return sess, nil
}

// Session finds an open session by ID.
func (s *Service) Session(id string) (*Session, bool) {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	sess, ok := s.hub.sessions[id]
	return sess, ok
}

// ID returns the session's unguessable identifier.
func (s *Session) ID() string { return s.id }

// Principal returns the caller the session was opened by.
func (s *Session) Principal() Principal { return s.principal }

// Events returns the stream of events. The channel is closed when the
// session ends, whether by Close or because the client fell too far behind.
func (s *Session) Events() <-chan Event { return s.events }

// Done is closed when the session ends.
func (s *Session) Done() <-chan struct{} { return s.done }

// Close ends the session and releases everything it holds. It is safe to
// call more than once and from any goroutine.
func (s *Session) Close() {
	s.once.Do(func() {
		close(s.done)
		s.mu.Lock()
		for _, t := range s.targets {
			s.svc.hub.interest(s, t, -1)
		}
		s.targets = map[int]*target{}
		s.mu.Unlock()

		s.svc.hub.mu.Lock()
		delete(s.svc.hub.sessions, s.id)
		s.svc.hub.mu.Unlock()
	})
}

func (s *Session) closed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// enqueue adds an item without ever blocking. A session whose queue is full
// has a client that cannot keep up; it is closed, and the client recovers by
// reconnecting and receiving fresh snapshots.
func (s *Session) enqueue(item queueItem) {
	select {
	case s.queue <- item:
	default:
		s.svc.logger.Warn("closing listen session that fell behind", "session", s.id)
		s.Close()
	}
}

// run is the session's goroutine. It is the only sender on s.events, which
// is why it is also the one that closes it.
func (s *Session) run() {
	defer close(s.events)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-s.done
		cancel()
	}()

	for {
		select {
		case <-s.done:
			return
		case item := <-s.queue:
			var out []Event
			if item.event != nil {
				out = []Event{*item.event}
			} else {
				var err error
				if out, err = s.process(ctx, item.batch); err != nil {
					if !s.closed() {
						s.svc.logger.Error("listen session failed", "session", s.id, "error", err)
					}
					s.Close()
					return
				}
			}
			for _, ev := range out {
				select {
				case s.events <- ev:
				case <-s.done:
					return
				}
			}
		}
	}
}

// AddTarget starts a listener on the session. The first event for the target
// carries its complete current state; later events carry changes.
func (s *Session) AddTarget(ctx context.Context, id int, t Target) error {
	if (t.Doc == "") == (t.Query == nil) {
		return fmt.Errorf("%w: a target needs either a document or a query", domain.ErrInvalidArgument)
	}
	tg := &target{id: id, doc: t.Doc}
	if t.Query != nil {
		if err := t.Query.Validate(); err != nil {
			return err
		}
		q := *t.Query
		tg.query = &q
	}

	// Holding the commit lock (shared) freezes the database version while
	// the snapshot is taken and the target is registered. Every commit is
	// then either fully inside the snapshot or delivered afterwards as a
	// change; none is missed and none is seen twice.
	svc := s.svc
	svc.commitMu.RLock()
	defer svc.commitMu.RUnlock()
	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case s.closed():
		return ErrSessionClosed
	case s.targets[id] != nil:
		return fmt.Errorf("%w: target %d is already in use", domain.ErrAlreadyExists, id)
	case len(s.targets) >= svc.limits.MaxTargetsPerSession:
		return fmt.Errorf("%w: too many targets on one session", domain.ErrResourceExhausted)
	}

	first := Event{Target: id, Version: svc.version, Reset: true, Changes: []DocChange{}}
	if tg.query == nil {
		doc, err := svc.Get(ctx, s.principal, tg.doc)
		switch {
		case err == nil:
			first.Changes = append(first.Changes, DocChange{ChangeAdded, doc})
		case !errors.Is(err, domain.ErrNotFound):
			return err
		}
	} else {
		docs, err := svc.runQuery(ctx, s.principal, *tg.query)
		if err != nil {
			return err
		}
		if tg.query.Limit > 0 {
			tg.known = make(map[domain.DocPath]int64, len(docs))
		}
		for _, doc := range docs {
			first.Changes = append(first.Changes, DocChange{ChangeAdded, doc})
			if tg.known != nil {
				tg.known[doc.Path] = doc.Version
			}
		}
	}

	select {
	case s.queue <- queueItem{event: &first}:
	default:
		// The queue is full, so the client is not keeping up. Close cannot
		// be called directly here because it needs s.mu, which we hold.
		go s.Close()
		return ErrSessionClosed
	}
	s.targets[id] = tg
	svc.hub.interest(s, tg, +1)
	return nil
}

// RemoveTarget stops a listener. Removing an unknown target does nothing.
func (s *Session) RemoveTarget(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.targets[id]; ok {
		delete(s.targets, id)
		s.svc.hub.interest(s, t, -1)
	}
}

// process turns one commit into at most one event per target.
//
// A target that has any stake in the commit gets an event even when nothing
// visible changed for it. The event's version tells the client "you are now
// up to date with this commit", which the client SDK relies on to know when
// its optimistic local writes have been confirmed.
func (s *Session) process(ctx context.Context, b *commitBatch) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ids := make([]int, 0, len(s.targets))
	for id := range s.targets {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	var events []Event
	for _, id := range ids {
		t := s.targets[id]
		var (
			changes []DocChange
			touched bool
			err     error
		)
		switch {
		case t.query == nil:
			changes, touched = s.documentChanges(ctx, t, b)
		case t.known == nil:
			changes, touched = s.queryChanges(ctx, t, b)
		default:
			changes, touched, err = s.limitedQueryChanges(ctx, t, b)
		}
		if err != nil {
			return nil, err
		}
		if touched {
			if changes == nil {
				changes = []DocChange{}
			}
			events = append(events, Event{Target: id, Version: b.version, Changes: changes})
		}
	}
	return events, nil
}

func (s *Session) mayRead(ctx context.Context, method rules.Method, doc *domain.Document) bool {
	return s.svc.allowed(ctx, s.principal, method, doc.Path, doc, nil)
}

// documentChanges handles a listener on a single document.
func (s *Session) documentChanges(ctx context.Context, t *target, b *commitBatch) (changes []DocChange, touched bool) {
	for _, c := range b.changes {
		if c.path != t.doc {
			continue
		}
		touched = true
		if c.before == c.after {
			continue // named by the commit but not changed
		}
		switch {
		case c.after != nil && s.mayRead(ctx, rules.MethodGet, c.after):
			kind := ChangeModified
			if c.before == nil {
				kind = ChangeAdded
			}
			changes = append(changes, DocChange{kind, *c.after})
		case c.before != nil:
			changes = append(changes, DocChange{ChangeRemoved, domain.Document{Path: c.path}})
		}
	}
	return changes, touched
}

// queryChanges handles a query without a limit. Such a query needs no state
// on the server: whether a document is in the result depends only on the
// document itself, so comparing "was it in before" with "is it in now"
// yields the change directly.
func (s *Session) queryChanges(ctx context.Context, t *target, b *commitBatch) (changes []DocChange, touched bool) {
	q := *t.query
	for _, c := range b.changes {
		if c.path.Parent() != q.Collection {
			continue
		}
		touched = true
		if c.before == c.after {
			continue // named by the commit but not changed
		}
		was := c.before != nil && q.Matches(*c.before) && q.InWindow(*c.before)
		is := c.after != nil && q.Matches(*c.after) && q.InWindow(*c.after) &&
			s.mayRead(ctx, rules.MethodList, c.after)
		switch {
		case was && is:
			changes = append(changes, DocChange{ChangeModified, *c.after})
		case is:
			changes = append(changes, DocChange{ChangeAdded, *c.after})
		case was:
			changes = append(changes, DocChange{ChangeRemoved, domain.Document{Path: c.path}})
		}
	}
	return changes, touched
}

// limitedQueryChanges handles a query with a limit by running it again and
// comparing the new result with the one the client already has.
func (s *Session) limitedQueryChanges(ctx context.Context, t *target, b *commitBatch) (changes []DocChange, touched bool, err error) {
	q := *t.query
	relevant := false
	for _, c := range b.changes {
		if c.path.Parent() != q.Collection {
			continue
		}
		touched = true
		if c.before == c.after {
			continue // named by the commit but not changed
		}
		if (c.before != nil && q.Matches(*c.before)) || (c.after != nil && q.Matches(*c.after)) {
			relevant = true
		}
	}
	if !relevant {
		return nil, touched, nil
	}

	docs, err := s.svc.runQuery(ctx, s.principal, q)
	if err != nil {
		return nil, false, err
	}
	current := make(map[domain.DocPath]int64, len(docs))
	for _, doc := range docs {
		current[doc.Path] = doc.Version
		switch version, had := t.known[doc.Path]; {
		case !had:
			changes = append(changes, DocChange{ChangeAdded, doc})
		case version != doc.Version:
			changes = append(changes, DocChange{ChangeModified, doc})
		}
	}
	for path := range t.known {
		if _, still := current[path]; !still {
			changes = append(changes, DocChange{ChangeRemoved, domain.Document{Path: path}})
		}
	}
	t.known = current
	return changes, touched, nil
}
