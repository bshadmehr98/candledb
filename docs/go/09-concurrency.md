# 9. Concurrency

Realtime listeners are the part of candledb where many things happen at
once: clients write, other clients receive those writes, connections come
and go. This chapter teaches Go's concurrency tools on that code.

## What you will learn

- Goroutines, and how to make sure each one ends.
- Channels: buffered and unbuffered, closing, and who is allowed to close.
- `select`, including non-blocking sends and channels that are `nil`.
- `sync.Mutex`, `sync.RWMutex`, `sync.Once`, `sync.WaitGroup`, and `sync/atomic`.
- Cancelling work with `context`.
- The design choices that keep this code correct: one writer at a time, a fixed lock order, and a policy for clients that cannot keep up.
- What the race detector finds and what it cannot.

## The feature

A client opens a listen stream and registers *targets*: a document or a
query it wants to follow ([tour: realtime](../tour/05-realtime.md)). For
each target it first receives the current state, then an event for every
commit that affects it.

Three requirements shape the code. A commit must not wait for listeners,
however many or however slow. A listener must not miss a commit or see
commits out of order. And a client that stops reading must not make the
server hold an ever-growing backlog for it.

## Where the code is

- [`internal/app/write.go`](../../internal/app/write.go): `Commit`, the single writer.
- [`internal/app/service.go`](../../internal/app/service.go): the `Service` struct and its lock.
- [`internal/app/listen.go`](../../internal/app/listen.go): the hub (who listens to what) and `Session` (one client's stream).
- [`internal/adapters/httpapi/listen.go`](../../internal/adapters/httpapi/listen.go): the HTTP handler that writes events to the socket.
- [`cmd/candledb/serve.go`](../../cmd/candledb/serve.go): the server's own goroutine and shutdown.

## Goroutines

A *goroutine* is a function running concurrently with the rest of the
program. It is started by writing `go` in front of a call. Goroutines are
cheap: a server can have many thousands. The Go runtime schedules them onto
operating-system threads.

Nobody waits for a goroutine automatically, and when `main` returns the
program ends whatever else is running. So the question to ask of every `go`
statement is: what makes this goroutine end, and who knows that it has?

Every listen session has one goroutine of its own:

From `internal/app/listen.go`:

```go
	s.hub.sessions[sess.id] = sess
	s.hub.mu.Unlock()

	go sess.run()
	return sess, nil
```

The HTTP server contributes the others. `net/http` runs every request
handler in a goroutine it starts. A handler such as `Commit`'s is therefore
already running concurrently with every other request, and all state shared
between requests needs protection. That is where the rest of this chapter
comes from.

## Mutexes: one writer at a time

A `sync.Mutex` is a lock. Between `Lock` and `Unlock`, a goroutine has
exclusive access; any other goroutine calling `Lock` waits. A
`sync.RWMutex` has two modes. `Lock` is exclusive as before. `RLock` is
shared: any number of goroutines can hold it together, but not while one
holds the exclusive lock.

The service has one lock that matters more than all the others:

From `internal/app/service.go`:

```go
	// commitMu makes commits strictly sequential. A commit holds it
	// exclusively; attaching a listener holds it shared, so the listener's
	// first snapshot and its position in the change stream agree exactly.
	commitMu sync.RWMutex
	version  int64 // version of the last commit; guarded by commitMu
```

`Commit` takes it exclusively for its whole duration:

From `internal/app/write.go`:

```go
	s.commitMu.Lock()
	defer s.commitMu.Unlock()

	now := s.clock.Now().UTC().Truncate(time.Microsecond)
	version := s.version + 1
```

`defer s.commitMu.Unlock()` releases the lock when `Commit` returns, on
every path. `Commit` has six `return` statements, and none of them has to
remember the unlock.

Inside the lock, `Commit` loads the documents, checks preconditions,
computes the new documents, asks the security rules, stores the result and
notifies listeners. Because only one commit runs at a time, none of those
steps can be invalidated by another commit in between. A precondition
"version is still 7" that holds when it is checked still holds when the
write is stored. There is no need for any finer locking, and SQLite accepts
only one writer anyway.

The comment `guarded by commitMu` on the `version` field is a convention
worth copying: it records which lock protects which data.

**Typical mistake:** `Lock()` at the top and `Unlock()` before each
`return`. The first `return` added later forgets it, and the next caller
waits for ever.

## Channels

A *channel* carries values from one goroutine to another. `ch <- v` sends,
`v := <-ch` receives. The rules that matter:

- An **unbuffered** channel (`make(chan T)`) has no storage. A send waits
  until another goroutine receives, and the reverse.
- A **buffered** channel (`make(chan T, n)`) stores up to `n` values. A send
  waits only when the buffer is full; a receive waits only when it is empty.
- `close(ch)` says that no more values will come. Receiving from a closed
  channel first returns any values still in the buffer, and after that
  returns the zero value immediately. The two-result form
  `v, open := <-ch` reports `open == false` in that state.
- Sending on a closed channel panics. So does closing a channel twice.

A session has three channels, each used differently:

From `internal/app/listen.go`:

```go
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
```

`queue` and `events` are buffered channels that carry data. `done` carries
nothing: its element type is the empty struct, and nothing is ever sent on
it. It is only ever closed. Closing a channel wakes *every* goroutine
receiving from it, now and in the future, which makes a closed channel a
broadcast signal: "this session is over".

The rule about who closes follows from "sending on a closed channel
panics": only a sender can know that no more sends will happen. `events`
has exactly one sender, the session's goroutine, and that goroutine closes
it. `queue` has many senders (every commit), so it is never closed at all;
the garbage collector reclaims it when the session is no longer referenced.

**Typical mistake:** closing a channel from the receiving side to "tell the
sender to stop". The sender's next send panics. Use a separate `done`
channel, as here, or a context.

## `select`

`select` waits on several channel operations and runs the case of
whichever can proceed. If several can, one is chosen at random. The
session's goroutine is a loop around a `select`:

From `internal/app/listen.go`:

```go
func (s *Session) run() {
	defer close(s.events)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-s.done
		cancel()
	}()
```

From `internal/app/listen.go`:

```go
			for _, ev := range out {
				select {
				case s.events <- ev:
				case <-s.done:
					return
				}
			}
```

(The middle of the function, omitted here, receives an item from `queue`
and turns it into the events in `out`.)

The inner `select` is the important one. A plain `s.events <- ev` would
wait for ever if the client stopped reading, and the goroutine would never
end. Offering `<-s.done` as an alternative gives the send a way out. Every
blocking channel operation in a long-lived goroutine should be in a
`select` with such an exit.

`defer close(s.events)` at the top is how the reader of `events` learns
that the session has ended.

### A non-blocking send

A `select` with a `default` case does not wait: if no channel operation
can proceed, `default` runs at once.

From `internal/app/listen.go`:

```go
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
```

This is called for every interested session, from inside `Commit`, with
the commit lock held. If it could wait, one stalled client would stop all
writes for everyone. With `default` it either puts the item in the buffer
or finds the buffer full, and then acts on that.

### A `nil` channel in a `select`

A channel variable that was never assigned is `nil`. Sending to or
receiving from a `nil` channel waits for ever, and in a `select` such a
case is simply never chosen. The HTTP handler uses that to make one case
optional:

From `internal/adapters/httpapi/listen.go`:

```go
	// A nil channel blocks forever in a select, so when the credentials do
	// not expire this case is simply never chosen.
	var expired <-chan time.Time
	if !p.Expires.IsZero() {
		timer := time.NewTimer(time.Until(p.Expires))
		defer timer.Stop()
		expired = timer.C
	}
```

Only when the caller's credentials have an expiry time is `expired` set to a timer's channel.
Otherwise it stays `nil` and the `case <-expired:` further down is dead,
without an `if` around the `select`.

`time.NewTimer` delivers one value on its channel `C` after the given
duration. A `time.Ticker`, used for the keep-alive a few lines earlier,
delivers one repeatedly. Both are stopped with `defer ...Stop()` so their
resources are released when the handler returns.

## Back-pressure: the slow consumer

When a producer is faster than a consumer, something has to give. The
options are: make the producer wait, buffer without limit, drop data, or
disconnect the consumer. Waiting would let a client stall commits.
Unlimited buffering lets a client exhaust the server's memory. Dropping
events would leave the client with a wrong picture and no way to know.

candledb disconnects. Each session can be a bounded number of commits
behind (`SessionBuffer`, 256 by default). `enqueue`, shown above, closes the
session when that buffer is full. The client notices the stream ending,
reconnects, and receives fresh snapshots, which is always a correct state.
`TestSlowListenerIsDisconnected` in
[`listen_test.go`](../../internal/app/listen_test.go) demonstrates it: one
session never reads, a hundred commits go through without delay, and the
session ends up closed.

## `sync.Once`: closing exactly once

A session can end for several reasons that may coincide: the client
disconnects, it falls behind, the server shuts down. `Close` can be called
by each of them, from different goroutines.

From `internal/app/listen.go`:

```go
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
```

`sync.Once` runs the function passed to `Do` the first time, and never
again, even under concurrent calls. Later callers wait until the first run
has finished and then return. Without it, the second `close(s.done)` would
panic.

`closed` shows how to ask a `done` channel a question without waiting:

From `internal/app/listen.go`:

```go
func (s *Session) closed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}
```

## The observer registry

Between `Commit` and the sessions sits the hub, which knows which sessions
have a target on which document or collection:

From `internal/app/listen.go`:

```go
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
```

The function is in two halves on purpose. Under `h.mu` it only collects
the interested sessions into a local set. The lock is released *before*
`enqueue` is called. `enqueue` may close a session, and `Close` needs
`h.mu` to remove the session from the tables. A `sync.Mutex` cannot be
locked twice by the same goroutine: the second `Lock` would wait for the
first to be released, which would never happen.

`map[*Session]struct{}` is the usual way to write a set: the keys are the
members and the values take no space.

## Attaching a listener without missing a commit

When a target is added, the client must get the current state and then
every later commit, with no gap and no overlap. Reading the state and
registering for changes are two steps, and a commit could land between them.

From `internal/app/listen.go`:

```go
	// Holding the commit lock (shared) freezes the database version while
	// the snapshot is taken and the target is registered. Every commit is
	// then either fully inside the snapshot or delivered afterwards as a
	// change; none is missed and none is seen twice.
	svc := s.svc
	svc.commitMu.RLock()
	defer svc.commitMu.RUnlock()
	s.mu.Lock()
	defer s.mu.Unlock()
```

`AddTarget` takes the commit lock in shared mode. While it holds it, no
commit can run, because commits need the lock exclusively. The database
version is frozen while the snapshot is read, the target is registered, and
the snapshot is put in the session's queue. Any commit after that finds the
target registered and queues behind the snapshot.

Shared mode is enough because adding a target changes no documents. Many
clients can attach listeners at the same moment; they only exclude writers,
and only briefly. Plain reads (`Get`, `Query`) do not take the lock at all.

`TestSnapshotAndChangesAreConsistent` checks the claim: it attaches
listeners while four goroutines are writing, replays each listener's
snapshot and changes, and requires the result to equal the store's final
content.

## Lock order, and a deadlock that was avoided

A *deadlock* is two goroutines each waiting for a lock the other holds.
The standard prevention is a fixed order: if every goroutine that needs
several locks takes them in the same order, a cycle cannot form.

In `internal/app` the order is:

1. `Service.commitMu`
2. `Session.mu`
3. `hub.mu`

`AddTarget` takes all three in that order (the third inside
`hub.interest`). `Close` takes the second and then the third. `Commit`
holds the first and, through `publish`, takes the third alone and releases
it; if it then has to close a session that fell behind, `Close` takes the
second and the third, still in order. No code path takes them in the
opposite direction.

One place needed care. At the end of `AddTarget`, the snapshot goes into
the queue, and the queue may be full:

From `internal/app/listen.go`:

```go
	select {
	case s.queue <- queueItem{event: &first}:
	default:
		// The queue is full, so the client is not keeping up. Close cannot
		// be called directly here because it needs s.mu, which we hold.
		go s.Close()
		return ErrSessionClosed
```

The obvious reaction to a full queue is `s.Close()`. But `AddTarget` holds
`s.mu` at this point, and `Close` begins by locking `s.mu`. The goroutine
would wait for itself. Starting `Close` in a new goroutine avoids that: the
new goroutine waits until `AddTarget` returns and releases the lock, then
closes the session. This is one of the few places where a goroutine is
started only to defer an action, and the comment says why.

**Typical mistake:** calling a method that takes a lock from inside a
section that already holds it. Nothing warns you; the program just stops
making progress on that path.

## A read lock is not re-entrant

The memory store shows a subtler version of the same problem:

From `internal/adapters/memory/store.go`:

```go
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
```

`keep` is the security-rules filter, and a rule may call `get(...)`, which
calls the store's `Get`, which takes `RLock`. Two read locks held by one
goroutine look harmless, since read locks are shared. They are not: when a
writer is waiting for the lock, `RWMutex` makes new readers wait behind it,
so that writers are not starved. The goroutine's second `RLock` then waits
for the writer, and the writer waits for the goroutine's first `RLock` to
be released.

The fix is the one in the excerpt: copy what is needed under the lock,
release it, and only then run code that might come back.

The SQLite adapter has the same shape of problem with a different resource:

From `internal/adapters/sqlite/plan.go`:

```go
// Rows are read a page at a time and the database connection is released
// between pages. keep may itself read from the database (a security rule
// calling get), and holding one connection while waiting for another is how
// a connection pool deadlocks under load.
func (db *DB) Query(ctx context.Context, q domain.Query, keep func(domain.Document) bool) ([]domain.Document, error) {
```

A connection pool is a counted resource. If every connection is held by a
query that is waiting for one more connection to run a rule's `get`, none
of them can finish. Reading a page, releasing the connection, and then
filtering means a query never holds one connection while asking for
another.

## `sync/atomic`

For a single number or pointer shared between goroutines, a mutex is more
than is needed. The types in `sync/atomic` offer operations that are
indivisible by themselves:

From `internal/adapters/instrumented/store.go`:

```go
type OpStats struct {
	Count  atomic.Int64
	Errors atomic.Int64
	Nanos  atomic.Int64 // total time spent
}
```

`Count.Add(1)` is safe from any number of goroutines, and `Count.Load()`
reads a consistent value. Atomics protect one operation on one variable.
The three counters above are not updated together, and a reader may see
`Count` already incremented and `Nanos` not yet. For statistics that is
acceptable. Where several values must change together, use a mutex.

`atomic.Pointer`, which holds the security rules, is covered in
[chapter 8](08-interpreter.md).

## Context cancellation

A `context.Context` carries a cancellation signal down a chain of calls.
Every store method takes one. For an HTTP request the context comes from
`r.Context()` and is cancelled when the client disconnects.

A session's work is not tied to one request, so its goroutine makes a
context of its own. Look again at the start of `run`, quoted above:
`context.WithCancel` returns a context and a function that cancels it. A
small helper goroutine waits for `done` and calls `cancel`. A query the
session is running for a limited listener is thereby abandoned as soon as
the session closes. The helper ends when `done` is closed, which every
path out of `run` guarantees.

`defer cancel()` is also there, although the helper would call it. Calling
a cancel function more than once is allowed, and the rule "always call the
cancel function you were given" is simpler to follow than to reason around.

## Waiting for goroutines: `sync.WaitGroup`

The server never needs to wait for a group of goroutines, but the tests do:

From `internal/app/write_test.go`:

```go
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range each {
				res, err := svc.Commit(ctx, anon, []domain.Write{incrementWrite("counters/c", "n", 1)})
				if err != nil {
					t.Errorf("Commit: %v", err)
					return
				}
				versions <- res.Version
			}
		})
	}
	wg.Wait()
	close(versions)
```

`wg.Go(f)` starts `f` in a new goroutine and counts it; `wg.Wait()` returns
when all of them have finished. Then, and only then, is it safe to close
`versions`: all senders are done.

`versions` is buffered with room for every result, so no sender ever waits
and the main goroutine can collect after the fact.

## Every goroutine has an owner

This is the complete list of goroutines the server's own code starts, and
what ends each one.

| Started in | Does | Ends when |
|---|---|---|
| `OpenSession` (`go sess.run()`) | turns queued commits into events | the session's `done` channel is closed |
| `Session.run` (the helper) | cancels the session's context | `done` is closed |
| `AddTarget` (`go s.Close()`) | closes a session that fell behind | `Close` returns |
| `serve` | runs `server.Serve` | the server is shut down or fails |

From `cmd/candledb/serve.go`:

```go
	serveErr := make(chan error, 1)
	go func() {
		if cfg.tlsCert != "" {
			serveErr <- server.ServeTLS(listener, cfg.tlsCert, cfg.tlsKey)
		} else {
			serveErr <- server.Serve(listener)
		}
	}()
```

The last one reports its result on a channel. The buffer of one matters:
if `serve` has already returned for another reason, nobody receives, and
an unbuffered send would keep this goroutine waiting for ever.

Goroutines started by libraries (`net/http` per connection, `database/sql`
for its pool) end when the server and the database are closed, which
`serve` does on shutdown.

## One commit, from write to socket

Follow one write that a listener is interested in.

1. **The writer's request goroutine** (started by `net/http`) runs
   `Service.Commit`. It takes `commitMu` exclusively, computes and stores
   the change, and calls `hub.publish`.
2. `publish` finds the sessions with a matching target and calls `enqueue`
   on each: a non-blocking send of a pointer to the commit's batch on that
   session's **`queue`** channel. `Commit` releases the lock and the writer
   gets its HTTP response. It never learns how many listeners there were.
3. **The session's goroutine** (`Session.run`) receives the batch from
   `queue`. In `process` it decides, per target, what the commit means for
   that target, consulting the security rules, and builds events.
4. It sends each event on the session's **`events`** channel, waiting if
   the buffer is full, unless the session is closed meanwhile.
5. **The listener's request goroutine**, inside `handleListen`, is waiting
   in its `select`. It receives the event, encodes it as JSON, writes
   `data: ...` to the response and flushes. The bytes are on the socket.

From `internal/adapters/httpapi/listen.go`:

```go
	// select waits on several channels at once and runs the case of
	// whichever is ready first.
	for {
		select {
		case <-r.Context().Done(): // the client disconnected
			return
		case ev, open := <-sess.Events():
			if !open { // the session ended: server shutdown, or the client fell behind
				_ = send(eventJSON{Type: "error", Code: "session_closed", Message: "reconnect to resume"})
				return
			}
			if err := send(toEventJSON(ev)); err != nil {
				return
			}
		case <-heartbeat.C:
```

Three goroutines and two channels. The buffers between them are what
decouple their speeds: the writer is never slowed by step 3 or 5, and the
session's goroutine is slowed by a slow socket only until its queue fills,
at which point the slow-consumer policy applies.

## The race detector

A *data race* is two goroutines accessing the same variable at the same
time, at least one of them writing, with nothing to order the accesses.
The result is undefined: values can be torn, updates lost, maps corrupted.

Go has a detector built in. `go test -race` (or `go build -race`) compiles
the program with instrumentation that watches memory accesses and reports
any race that occurs, with the stack of both accesses.

What it proves and what it does not:

- A report is a real race. It does not produce false alarms.
- It only sees what happens during the run. A race on a path the tests did
  not exercise, or exercised with only one goroutine, is not found.
- It finds data races, not logic errors. A program can be free of data
  races and still do things in the wrong order, lose an update between two
  correctly locked steps, or deadlock.

So the detector is only as good as the tests that run under it. That is
why this codebase has tests that are deliberately concurrent
(`TestConcurrentCommits`, the "concurrent use" case of the storage
contract, the consistency test above), and why CI runs everything with
`-race`.

## Try it

Run the concurrent tests with the race detector:

```sh
go test -race ./internal/app -run 'TestConcurrentCommits|TestSlowListenerIsDisconnected|TestSnapshotAndChangesAreConsistent' -v
```

Watch a commit travel. Start a server:

```sh
./bin/candledb serve -addr 127.0.0.1:18122 -data-dir /tmp/candledb-ch09 -dev-open-rules
```

In a second terminal, open the stream. `-N` stops curl from buffering:

```sh
curl -sN http://127.0.0.1:18122/v1/listen
```

It prints a `ready` message with a session ID and then waits. In a third
terminal, add a target to that session (paste your ID) and write a document:

```sh
curl -s -X POST http://127.0.0.1:18122/v1/listen/YOUR_SESSION_ID/targets \
  -d '{"id": 1, "query": {"collection": "notes"}}'
curl -s -X PUT http://127.0.0.1:18122/v1/documents/notes/n1 -d '{"text": "hello"}'
```

The second terminal shows a `snapshot` for target 1 and then a `change`
with the new document. Now press Ctrl-C in the server's terminal. The
stream prints `{"type":"error","code":"session_closed",...}` and ends:
shutdown closed the session, `run` closed `events`, and the handler saw
`open == false`.

Finally, see the race detector at work. In
`internal/adapters/memory/store.go`, delete these two lines at the top of
`Apply`:

```go
	s.mu.Lock()
	defer s.mu.Unlock()
```

and run the contract's concurrency test, first with the detector:

```sh
go test -race ./internal/adapters/memory -run 'TestStoreContract/concurrent_use'
```

The output contains `WARNING: DATA RACE` followed by the two conflicting
accesses, with file and line for each. Without `-race` the same test may
crash with `fatal error: concurrent map writes`, a cheaper check built
into maps, or may pass, depending on timing. Put the two lines back.

## Exercise

Make the slow-consumer policy observable.

**Goal.** The server counts how many listen sessions it has closed because
they fell behind, and `/metrics` reports the number as
`candledb_listen_sessions_dropped_total`.

**Files.**

- `internal/app/listen.go`: the counter, incremented in the right place.
- `internal/app/service.go`: add the number to `Stats`.
- `internal/adapters/httpapi/metrics.go`: one more line of output.
- `internal/app/listen_test.go`: a test.

**Hints.**

- Where does the counter live? There is one per server, not one per
  session.
- A session can be closed for falling behind in two places. Find both.
- Several commits can find the same session's queue full before it is
  closed. The session must be counted once. Which existing mechanism
  already makes something happen exactly once per session, and how can the
  count ride on it?
- Use an atomic type, and say in a comment why a mutex is not needed.
- For the test, start from `TestSlowListenerIsDisconnected`. After the slow
  session's `Done` channel is closed, the count must be exactly 1, and the
  healthy session must not have been counted.

**How to know it works.** The new test passes repeatedly under the race
detector (`go test -race -count=20 ./internal/app -run YourTestName`), and
the metric appears in `/metrics` when you request it with the admin key.

## Further reading

- [A Tour of Go: Concurrency](https://go.dev/tour/concurrency/1)
- [Effective Go: Concurrency](https://go.dev/doc/effective_go#concurrency)
- [Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines)
- [Go Concurrency Patterns: Context](https://go.dev/blog/context)
- [Introducing the Go Race Detector](https://go.dev/blog/race-detector) and the [race detector manual](https://go.dev/doc/articles/race_detector)
- [The Go Memory Model](https://go.dev/ref/mem)
- [sync package documentation](https://pkg.go.dev/sync), in particular the notes on `RWMutex`

Next: [10. Running in production](10-production.md)
