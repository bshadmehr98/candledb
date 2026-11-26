package app_test

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/domain"
)

// nextEvent waits for the session's next event. Tests of concurrent code
// should never sleep and hope; they wait for the thing itself, with a
// timeout only as a safety net so a bug shows up as a failure, not a hang.
func nextEvent(t *testing.T, sess *app.Session) app.Event {
	t.Helper()
	select {
	case ev, open := <-sess.Events():
		if !open {
			t.Fatal("session closed while waiting for an event")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an event")
	}
	panic("unreachable")
}

// describe renders an event compactly: "t1 v3 [+a ~b -c]", with "!" marking
// a reset. Comparing strings keeps the assertions readable.
func describe(ev app.Event) string {
	parts := make([]string, len(ev.Changes))
	for i, c := range ev.Changes {
		symbol := map[app.ChangeKind]string{app.ChangeAdded: "+", app.ChangeModified: "~", app.ChangeRemoved: "-"}[c.Kind]
		parts[i] = symbol + c.Doc.Path.ID()
	}
	sort.Strings(parts)
	reset := ""
	if ev.Reset {
		reset = "!"
	}
	return fmt.Sprintf("t%d v%d%s [%s]", ev.Target, ev.Version, reset, strings.Join(parts, " "))
}

func expectEvent(t *testing.T, sess *app.Session, want string) app.Event {
	t.Helper()
	ev := nextEvent(t, sess)
	if got := describe(ev); got != want {
		t.Fatalf("event = %s, want %s", got, want)
	}
	return ev
}

func openSession(t *testing.T, svc *app.Service, p app.Principal) *app.Session {
	t.Helper()
	sess, err := svc.OpenSession(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sess.Close)
	return sess
}

func queryTarget(q domain.Query) app.Target { return app.Target{Query: &q} }

func TestListenToDocument(t *testing.T) {
	svc, _ := newService(t, "")
	commit(t, svc, anon, setWrite(t, "users/ada", `{"n": 1}`))
	sess := openSession(t, svc, anon)

	if err := sess.AddTarget(ctx, 1, app.Target{Doc: "users/ada"}); err != nil {
		t.Fatal(err)
	}
	first := expectEvent(t, sess, "t1 v1! [+ada]")
	if first.Changes[0].Doc.Data["n"] != domain.Int(1) {
		t.Errorf("snapshot data = %v", first.Changes[0].Doc.Data)
	}

	commit(t, svc, anon, setWrite(t, "users/ada", `{"n": 2}`))
	changed := expectEvent(t, sess, "t1 v2 [~ada]")
	if changed.Changes[0].Doc.Data["n"] != domain.Int(2) {
		t.Errorf("change data = %v", changed.Changes[0].Doc.Data)
	}

	commit(t, svc, anon, setWrite(t, "users/bob", `{}`)) // another document: no event
	commit(t, svc, anon, deleteWrite("users/ada"))
	expectEvent(t, sess, "t1 v4 [-ada]")

	commit(t, svc, anon, setWrite(t, "users/ada", `{"n": 3}`))
	expectEvent(t, sess, "t1 v5 [+ada]")
}

func TestListenToMissingDocument(t *testing.T) {
	svc, _ := newService(t, "")
	sess := openSession(t, svc, anon)
	if err := sess.AddTarget(ctx, 1, app.Target{Doc: "users/nobody"}); err != nil {
		t.Fatal(err)
	}
	expectEvent(t, sess, "t1 v0! []")
	commit(t, svc, anon, setWrite(t, "users/nobody", `{}`))
	expectEvent(t, sess, "t1 v1 [+nobody]")
}

func TestListenToQuery(t *testing.T) {
	svc, _ := newService(t, "")
	commit(t, svc, anon,
		setWrite(t, "tasks/a", `{"done": false}`),
		setWrite(t, "tasks/b", `{"done": true}`),
	)
	sess := openSession(t, svc, anon)
	open := domain.Query{Collection: "tasks", Filters: []domain.Filter{
		{Field: domain.FieldPath{"done"}, Op: domain.OpEq, Value: domain.Bool(false)}}}
	if err := sess.AddTarget(ctx, 1, queryTarget(open)); err != nil {
		t.Fatal(err)
	}
	expectEvent(t, sess, "t1 v1! [+a]")

	commit(t, svc, anon, setWrite(t, "tasks/c", `{"done": false}`))
	expectEvent(t, sess, "t1 v2 [+c]")

	commit(t, svc, anon, setWrite(t, "tasks/a", `{"done": false, "title": "x"}`))
	expectEvent(t, sess, "t1 v3 [~a]")

	// Leaving the filter is a removal; entering it is an addition.
	commit(t, svc, anon, setWrite(t, "tasks/a", `{"done": true}`))
	expectEvent(t, sess, "t1 v4 [-a]")
	commit(t, svc, anon, setWrite(t, "tasks/b", `{"done": false}`))
	expectEvent(t, sess, "t1 v5 [+b]")

	commit(t, svc, anon, deleteWrite("tasks/c"))
	expectEvent(t, sess, "t1 v6 [-c]")

	// A commit in the collection that changes nothing visible still
	// produces an (empty) event carrying the new version.
	commit(t, svc, anon, setWrite(t, "tasks/z", `{"done": true}`))
	expectEvent(t, sess, "t1 v7 []")

	// One batch, one event.
	commit(t, svc, anon, setWrite(t, "tasks/d", `{"done": false}`), setWrite(t, "tasks/e", `{"done": false}`), deleteWrite("tasks/b"))
	expectEvent(t, sess, "t1 v8 [+d +e -b]")
}

func TestListenToLimitedQuery(t *testing.T) {
	svc, _ := newService(t, "")
	commit(t, svc, anon,
		setWrite(t, "scores/a", `{"points": 10}`),
		setWrite(t, "scores/b", `{"points": 20}`),
		setWrite(t, "scores/c", `{"points": 30}`),
	)
	sess := openSession(t, svc, anon)
	top2 := domain.Query{Collection: "scores", Orders: []domain.Order{{Field: domain.FieldPath{"points"}, Desc: true}}, Limit: 2}
	if err := sess.AddTarget(ctx, 1, queryTarget(top2)); err != nil {
		t.Fatal(err)
	}
	expectEvent(t, sess, "t1 v1! [+b +c]")

	// Removing a document from the top two pulls the next one in.
	commit(t, svc, anon, deleteWrite("scores/c"))
	expectEvent(t, sess, "t1 v2 [+a -c]")

	// A new leader pushes the last one out.
	commit(t, svc, anon, setWrite(t, "scores/d", `{"points": 99}`))
	expectEvent(t, sess, "t1 v3 [+d -a]")

	// A change below the cut-off is not visible.
	commit(t, svc, anon, setWrite(t, "scores/a", `{"points": 11}`))
	expectEvent(t, sess, "t1 v4 []")

	commit(t, svc, anon, setWrite(t, "scores/b", `{"points": 21}`))
	expectEvent(t, sess, "t1 v5 [~b]")
}

func TestListenRespectsRules(t *testing.T) {
	svc, _ := newService(t, notesRules)
	commit(t, svc, admin,
		setWrite(t, "notes/mine", `{"owner": "ann"}`),
		setWrite(t, "notes/theirs", `{"owner": "bob"}`),
		setWrite(t, "secrets/s", `{}`),
	)
	sess := openSession(t, svc, user("ann"))

	if err := sess.AddTarget(ctx, 1, queryTarget(domain.Query{Collection: "notes"})); err != nil {
		t.Fatal(err)
	}
	expectEvent(t, sess, "t1 v1! [+mine]")

	// Bob's new note never reaches ann; publishing it does.
	commit(t, svc, admin, setWrite(t, "notes/bob2", `{"owner": "bob"}`))
	expectEvent(t, sess, "t1 v2 []")
	commit(t, svc, admin, setWrite(t, "notes/bob2", `{"owner": "bob", "public": true}`))
	// "modified", not "added": the server does not remember what each
	// client has seen, and clients treat a change to an unknown document
	// as an addition.
	expectEvent(t, sess, "t1 v3 [~bob2]")
	// Making it private again takes it away.
	commit(t, svc, admin, setWrite(t, "notes/bob2", `{"owner": "bob"}`))
	expectEvent(t, sess, "t1 v4 [-bob2]")

	// Listening to a document the caller may not read fails up front.
	if err := sess.AddTarget(ctx, 2, app.Target{Doc: "secrets/s"}); !errors.Is(err, domain.ErrPermissionDenied) {
		t.Errorf("listening to a forbidden document: %v", err)
	}
}

func TestSeveralTargetsOnOneSession(t *testing.T) {
	svc, _ := newService(t, "")
	sess := openSession(t, svc, anon)
	if err := sess.AddTarget(ctx, 1, queryTarget(domain.Query{Collection: "a"})); err != nil {
		t.Fatal(err)
	}
	if err := sess.AddTarget(ctx, 2, app.Target{Doc: "a/x"}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AddTarget(ctx, 3, queryTarget(domain.Query{Collection: "b"})); err != nil {
		t.Fatal(err)
	}
	expectEvent(t, sess, "t1 v0! []")
	expectEvent(t, sess, "t2 v0! []")
	expectEvent(t, sess, "t3 v0! []")

	if err := sess.AddTarget(ctx, 1, app.Target{Doc: "a/x"}); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("reusing a target ID: %v", err)
	}
	if err := sess.AddTarget(ctx, 9, app.Target{}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("empty target: %v", err)
	}

	commit(t, svc, anon, setWrite(t, "a/x", `{}`), setWrite(t, "b/y", `{}`))
	expectEvent(t, sess, "t1 v1 [+x]")
	expectEvent(t, sess, "t2 v1 [+x]")
	expectEvent(t, sess, "t3 v1 [+y]")

	sess.RemoveTarget(1)
	commit(t, svc, anon, setWrite(t, "a/x", `{"n": 1}`))
	expectEvent(t, sess, "t2 v2 [~x]") // and nothing for target 1

	if stats := svc.Stats(); stats.Sessions != 1 || stats.Targets != 2 {
		t.Errorf("stats = %+v, want 1 session and 2 targets", stats)
	}
	sess.Close()
	sess.Close() // closing twice is harmless
	if stats := svc.Stats(); stats.Sessions != 0 || stats.Targets != 0 {
		t.Errorf("stats after close = %+v, want zero", stats)
	}
	// Ranging over a channel ends when the channel is closed, so this loop
	// finishing is itself the check that Close closed the event stream.
	for range sess.Events() {
	}
	if err := sess.AddTarget(ctx, 5, app.Target{Doc: "a/x"}); !errors.Is(err, app.ErrSessionClosed) {
		t.Errorf("AddTarget on a closed session: %v", err)
	}
}

// TestSlowListenerIsDisconnected checks the back-pressure policy: a client
// that does not read its events must not slow down writers or make the
// server buffer without bound.
func TestSlowListenerIsDisconnected(t *testing.T) {
	svc, _ := newService(t, "", app.WithLimits(app.Limits{
		MaxBatchWrites: 10, MaxQueryResults: 100, MaxSessions: 10, MaxTargetsPerSession: 10, SessionBuffer: 4,
	}))
	slow := openSession(t, svc, anon)
	if err := slow.AddTarget(ctx, 1, app.Target{Doc: "c/doc"}); err != nil {
		t.Fatal(err)
	}
	healthy := openSession(t, svc, anon)
	if err := healthy.AddTarget(ctx, 1, app.Target{Doc: "c/doc"}); err != nil {
		t.Fatal(err)
	}
	expectEvent(t, healthy, "t1 v0! []")

	// Nobody reads from slow. Its buffers fill up and it gets dropped,
	// while every commit still succeeds promptly.
	for i := range 100 {
		commit(t, svc, anon, incrementWrite("c/doc", "n", 1))
		ev := nextEvent(t, healthy)
		if ev.Version != int64(i+1) {
			t.Fatalf("healthy listener got version %d, want %d", ev.Version, i+1)
		}
	}
	select {
	case <-slow.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the slow session was never closed")
	}
}

func TestSessionLimits(t *testing.T) {
	svc, _ := newService(t, "", app.WithLimits(app.Limits{
		MaxBatchWrites: 10, MaxQueryResults: 100, MaxSessions: 1, MaxTargetsPerSession: 1, SessionBuffer: 4,
	}))
	sess := openSession(t, svc, anon)
	if _, err := svc.OpenSession(anon); !errors.Is(err, domain.ErrResourceExhausted) {
		t.Errorf("second session: %v, want ErrResourceExhausted", err)
	}
	if err := sess.AddTarget(ctx, 1, app.Target{Doc: "a/b"}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AddTarget(ctx, 2, app.Target{Doc: "a/c"}); !errors.Is(err, domain.ErrResourceExhausted) {
		t.Errorf("second target: %v, want ErrResourceExhausted", err)
	}
	if got, ok := svc.Session(sess.ID()); !ok || got != sess {
		t.Error("Session lookup by ID failed")
	}
}

// TestSnapshotAndChangesAreConsistent starts listeners while writers are
// busy and checks that snapshot plus changes always add up to the real
// final state. If attaching a listener could miss a commit, or deliver one
// twice out of order, the replayed state would differ.
func TestSnapshotAndChangesAreConsistent(t *testing.T) {
	svc, _ := newService(t, "")
	const writers, writesEach, listeners = 4, 60, 6

	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for i := range writesEach {
				path := fmt.Sprintf("items/w%d-%d", w, i%10)
				var write domain.Write
				if i%7 == 6 {
					write = deleteWrite(path)
				} else {
					write = setWrite(t, path, fmt.Sprintf(`{"i": %d}`, i))
				}
				if _, err := svc.Commit(ctx, anon, []domain.Write{write}); err != nil {
					t.Errorf("Commit: %v", err)
					return
				}
			}
		})
	}

	sessions := make([]*app.Session, listeners)
	for i := range sessions {
		sessions[i] = openSession(t, svc, anon)
		if err := sessions[i].AddTarget(ctx, 1, queryTarget(domain.Query{Collection: "items"})); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond) // spread the listeners across the writes
	}
	wg.Wait()

	final, err := svc.Query(ctx, anon, domain.Query{Collection: "items"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[domain.DocPath]int64{}
	for _, d := range final {
		want[d.Path] = d.Version
	}
	lastVersion := svc.Stats().Version

	for i, sess := range sessions {
		state := map[domain.DocPath]int64{}
		previous := int64(-1)
		for {
			ev := nextEvent(t, sess)
			if ev.Version <= previous {
				t.Fatalf("listener %d: version went from %d to %d", i, previous, ev.Version)
			}
			previous = ev.Version
			for _, c := range ev.Changes {
				if c.Kind == app.ChangeRemoved {
					delete(state, c.Doc.Path)
				} else {
					state[c.Doc.Path] = c.Doc.Version
				}
			}
			if ev.Version == lastVersion {
				break
			}
		}
		if len(state) != len(want) {
			t.Fatalf("listener %d ended with %d documents, the store has %d", i, len(state), len(want))
		}
		for path, version := range want {
			if state[path] != version {
				t.Fatalf("listener %d has %s at version %d, the store has %d", i, path, state[path], version)
			}
		}
	}
}

// A commit that names a document without changing it (a delete of nothing,
// or a verify) still tells listeners the new version.
func TestListenersHearAboutCommitsThatChangeNothing(t *testing.T) {
	svc, _ := newService(t, "")
	commit(t, svc, anon, setWrite(t, "c/doc", `{}`))
	sess := openSession(t, svc, anon)
	if err := sess.AddTarget(ctx, 1, queryTarget(domain.Query{Collection: "c"})); err != nil {
		t.Fatal(err)
	}
	expectEvent(t, sess, "t1 v1! [+doc]")

	commit(t, svc, anon, deleteWrite("c/never-existed"))
	expectEvent(t, sess, "t1 v2 []")

	commit(t, svc, anon, domain.Write{Op: domain.OpVerify, Path: "c/doc"})
	expectEvent(t, sess, "t1 v3 []")
}
