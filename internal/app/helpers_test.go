package app_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/bshadmehr98/candledb/internal/adapters/memory"
	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/domain"
	"github.com/bshadmehr98/candledb/internal/rules"
)

var ctx = context.Background()

// fakeClock is a Clock the test moves by hand. Because app only knows the
// Clock interface, tests can make an hour pass in a nanosecond instead of
// sleeping.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// sequentialIDs hands out id-1, id-2, ... so tests can predict IDs.
type sequentialIDs struct {
	mu sync.Mutex
	n  int
}

func (g *sequentialIDs) NewID() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return fmt.Sprintf("id-%d", g.n)
}

var (
	admin = app.Principal{UID: "admin", Admin: true}
	anon  = app.Principal{}
)

func user(uid string) app.Principal { return app.Principal{UID: uid} }

// newService builds a Service over an empty in-memory store. An empty
// rulesSrc means "allow everything".
func newService(t *testing.T, rulesSrc string, opts ...app.Option) (*app.Service, *fakeClock) {
	t.Helper()
	ruleset := rules.AllowAll()
	if rulesSrc != "" {
		var err error
		if ruleset, err = rules.Parse(rulesSrc); err != nil {
			t.Fatalf("test rules do not parse: %v", err)
		}
	}
	clock := newClock()
	base := []app.Option{app.WithClock(clock), app.WithRules(ruleset), app.WithIDGenerator(&sequentialIDs{})}
	svc, err := app.New(ctx, memory.NewStore(), append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	return svc, clock
}

func obj(t *testing.T, s string) domain.Map {
	t.Helper()
	m, err := domain.ParseMapJSON([]byte(s))
	if err != nil {
		t.Fatalf("bad test data %s: %v", s, err)
	}
	return m
}

func setWrite(t *testing.T, path, data string) domain.Write {
	t.Helper()
	return domain.Write{Op: domain.OpSet, Path: domain.DocPath(path), Data: obj(t, data)}
}

func deleteWrite(path string) domain.Write {
	return domain.Write{Op: domain.OpDelete, Path: domain.DocPath(path)}
}

func incrementWrite(path, field string, by int64) domain.Write {
	return domain.Write{Op: domain.OpMerge, Path: domain.DocPath(path), Fields: []domain.FieldOp{
		{Path: domain.FieldPath{field}, Transform: domain.Increment{By: domain.Int(by)}},
	}}
}

// commit applies writes as the given principal and fails the test on error.
func commit(t *testing.T, svc *app.Service, p app.Principal, writes ...domain.Write) app.CommitResult {
	t.Helper()
	res, err := svc.Commit(ctx, p, writes)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return res
}
