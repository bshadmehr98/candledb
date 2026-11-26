# Design decisions

An architecture is a set of decisions, and each decision gives something up.
This page records the important ones in the same form every time:

- **Context**: the situation that forced a choice.
- **Decision**: what the code does.
- **Consequences**: what follows, good and bad, including limits accepted.
- **Alternatives rejected**: what else was possible and why it lost.

Engineers call such a note an architecture decision record. The value is in
the last two parts. Anyone can see what the code does; the record says what
it costs and what was considered.

The decisions are grouped: writes, storage, queries and rules, realtime,
values, accounts.

---

## Writes

### 1. One writer at a time

**Context.** A commit must load documents, check preconditions, compute new
documents, evaluate rules against the old and new state, store the result
and notify listeners. If two commits interleave, a precondition can be
checked against a document that changes a moment later, or two increments
can both start from the same old value.

**Decision.** `Service.Commit` in
[`app/write.go`](../../internal/app/write.go) holds an exclusive lock,
`commitMu`, for the whole pipeline. There is exactly one commit in progress
at any time.

```go
// commitMu makes commits strictly sequential. A commit holds it
// exclusively; attaching a listener holds it shared, so the listener's
// first snapshot and its position in the change stream agree exactly.
commitMu sync.RWMutex
version  int64 // version of the last commit; guarded by commitMu
```

**Consequences.**

- Correctness becomes easy to argue. Inside `Commit`, nothing else changes
  the data, so every check holds until the write is stored. The test
  `TestConcurrentCommits` increments one counter from twenty goroutines and
  demands the exact total.
- Write throughput is that of one core and one disk. SQLite allows only one
  writer anyway, so the lock adds little on top of the storage engine's own
  limit. The benchmark `BenchmarkApply` measures the storage part at roughly
  twelve thousand single-document commits per second on a laptop.
- A rule that calls `get()` on a slow store delays every other writer.
- The lock lives in one process. Two candledb processes on one database
  would each believe they are the only writer. The design is single-node.

**Alternatives rejected.** Per-document locks allow parallel commits to
unrelated documents, but batches touch several documents (deadlock ordering
needed), rules can read arbitrary other documents, and listeners need
commits in one global order. Optimistic concurrency inside the server
(run, detect conflict, retry) moves the same complexity elsewhere. Both buy
throughput that a lightweight single-node database does not need.

### 2. One version number for the whole database

**Context.** Clients need to say "write only if this document is unchanged",
and listeners need to know how far through the history of changes they are.

**Decision.** Every successful commit increases a single counter by one.
Each document written by the commit is stamped with that number
(`Document.Version`), and the store saves the counter in the same
transaction as the documents.

**Consequences.**

- Versions totally order all writes. An event with version 41 was caused by
  an earlier commit than one with version 42, whichever documents were
  involved.
- A document's version doubles as its optimistic-concurrency token.
- The counter is the client SDK's signal for "the server has caught up with
  my write" (decision 10).
- Commits that change nothing visible (a delete of a missing document, a
  verify) still consume a version.
- Because the counter is in memory and guarded by the commit lock, it is
  another reason the design is single-node.

**Alternatives rejected.** Timestamps as versions: clocks go backwards and
two commits can share a microsecond. Per-document counters: fine for
preconditions, but they give listeners no global order.

### 3. Transactions are preconditions, checked at commit

**Context.** Applications need read-modify-write: read a balance, subtract,
write it back, with no lost updates. Firestore offers transactions for this.

**Decision.** The server has no transaction sessions and holds no locks
between requests. A write may carry a `Precondition` (the document must
exist, must not exist, or must be at an exact version). A special write
operation, `verify`, changes nothing and only checks its precondition. The
client SDK builds transactions from these: it records the version of every
document it reads, then sends one commit in which each written document
carries its version as precondition and each document that was only read
gets a `verify`. On a 412 response the SDK runs the function again.

```go
// Precondition makes a write conditional. The zero value imposes nothing.
type Precondition struct {
	// Exists, when set, requires the document to exist (true) or not (false).
	Exists *bool
	// Version, when set, requires the stored document to be at exactly this
	// version. It is how clients build optimistic transactions: read, then
	// write only if nothing changed in between.
	Version *int64
}
```

**Consequences.**

- The server stays stateless between requests; a client that disappears
  mid-transaction leaves nothing to clean up.
- Under heavy contention on one document, clients retry a lot. The SDK's
  contended-counter test raises the retry limit from the default 5 to 200
  for four clients racing on one counter.
- A transaction's reads are not a consistent snapshot: two `get` calls may
  see different versions of the world. The commit-time check makes the
  *outcome* correct, but the function may briefly compute with mixed data
  before being retried.
- `verify` requires `get` permission on the document, since it reveals the
  document's version.

**Alternatives rejected.** Server-side transactions with locks: need
sessions, timeouts and deadlock handling. Sending the function to the server
to execute: a different and much larger product.

---

## Storage

### 4. SQLite, with one write handle and a pool of readers

**Context.** The product is meant to be one binary with no services to
install. It needs durable storage, atomic multi-row writes and ordered range
scans.

**Decision.** Store everything in one SQLite file, through a SQLite driver
written in Go (no C compiler, static binary). `sqlite.Open` in
[`sqlite/db.go`](../../internal/adapters/sqlite/db.go) creates two
`*sql.DB` handles on the same file: `write`, limited to one connection, and
`read`, with a small pool. The file uses write-ahead logging.

```go
type DB struct {
	// SQLite allows many readers but only one writer at a time. Giving
	// writes their own handle limited to one connection turns that rule
	// into ordinary queueing inside database/sql, instead of "database is
	// locked" errors under load.
	write *sql.DB
	read  *sql.DB
}
```

**Consequences.**

- Deployment is a file. Backup is one command (`VACUUM INTO`).
- Readers never wait for the writer, and the writer never fails with
  "database is locked", because competing writes queue inside `database/sql`
  for the single connection.
- `synchronous=NORMAL` with WAL means a power failure can lose the last few
  commits (never corrupt the file). A crash of the process alone loses
  nothing. Operators who need every acknowledged commit to survive power
  loss would have to change that setting.
- One machine's disk is the capacity limit.

**Alternatives rejected.** An embedded key-value store (Pebble, bbolt):
closer to how Firestore works internally, but then transactions over several
keys, a query layer and tooling to inspect data would all have to be built.
PostgreSQL: a server to install, which defeats "lightweight". The storage
port leaves that door open ([hexagonal](01-hexagonal.md), "A PostgreSQL
store").

### 5. One index row per field value, with order-preserving keys

**Context.** Queries filter and order by arbitrary fields of schemaless
documents whose values may be of any type. Firestore answers every query
from an index and requires a composite index, declared in advance, for most
combinations of fields.

**Decision.** For every document, the store writes one row to an `idx`
table for each field at every nesting level, and one more per array element.
The row's key is the value encoded by `encodeKey` in
[`sqlite/keyenc.go`](../../internal/adapters/sqlite/keyenc.go) so that
comparing the encoded bytes gives the same order as `domain.Compare` gives
for the values. There are no composite indexes and nothing to declare.

```go
// This file turns a domain.Value into a byte string whose plain byte order
// (the order memcmp and SQLite's BLOB comparison use) is the same as
// domain.Compare's order of the values:
//
//	Compare(a, b) < 0   exactly when   bytes.Compare(encodeKey(a), encodeKey(b)) < 0
//
// With that property an ordinary B-tree index over the encoded bytes can
// answer "all documents where age >= 18, in order" with a single range scan,
// for any mix of value types.
```

**Consequences.**

- Any single-field filter or ordering is one range scan of the table's
  primary key.
- Every query works without configuration. No "this query needs an index"
  error exists.
- A query combining conditions on several fields uses the index for one of
  them and checks the rest row by row (decision 6). It is correct, but its
  cost depends on how selective the chosen field is, not on the result size.
  Firestore's promise that cost follows result size holds here only for
  queries the single index covers.
- Writes cost one index row per field, nested fields included. A document
  with many fields is expensive to write.

**Alternatives rejected.** Composite indexes: the right tool for large data,
but they bring declaration files, background index builds and query
rejection, which is most of the complexity a lightweight tool wants to
avoid. Querying with SQLite's JSON functions: cannot preserve the
cross-type ordering that the domain defines, and cannot use an index for
arbitrary fields.

### 6. The plan finds candidates; the domain decides

**Context.** A query planner is where subtle bugs live: an off-by-one in a
range bound returns a wrong document, silently.

**Decision.** `planQuery` in
[`sqlite/plan.go`](../../internal/adapters/sqlite/plan.go) only has to
choose a scan that returns a *superset* of the answer in a known order.
Every row read is then tested with `domain.Query.Matches` and `InWindow`,
the same functions the memory store uses. If the plan is not in final order,
the results are sorted with `domain.Query.Compare`.

```go
// That second step is what keeps the planner simple and safe. The plan only
// has to produce a superset of the answer, in a known order; domain.Query
// decides what really matches. A planning mistake can make a query slower
// but cannot make it return a wrong document.
```

**Consequences.**

- The definition of "what a query returns" exists once, in
  [`domain/query.go`](../../internal/domain/query.go), and both adapters
  inherit it.
- A planner mistake makes a query slower, never wrong. The planner's range
  bounds are deliberately loose for that reason: `age > 18` scans from the
  key of 18 inclusive and lets `Matches` drop the 18s.
- The differential test (`TestQueriesAgreeWithReference`) can compare the
  two adapters on thousands of random queries.
- Each row is decoded from JSON before it is tested, even when the index
  alone could have rejected it.

**Alternatives rejected.** Compiling the whole query to SQL: faster for
multi-condition queries, but the cross-type comparison rules would have to
be reproduced exactly in SQL, a second definition to keep in step.

### 7. Read in pages, hold no connection in between

**Context.** While scanning, the store calls `keep` for each row, and `keep`
may evaluate a rule that calls `get()`, which needs a database connection.
If the scan holds a connection while `keep` waits for another, then enough
concurrent queries exhaust the pool with every query waiting for a
connection that only another waiting query could release.

**Decision.** `DB.Query` reads up to 500 rows into memory, closes the rows
(releasing the connection), then filters them, and fetches the next page
starting after the last row's `(key, id)` position.

**Consequences.**

- No query ever holds one connection while waiting for another, so the pool
  cannot deadlock on itself. The contract suite's
  "query keep runs before limit" case calls `Get` inside `keep` to exercise
  this.
- A query that spans several pages is not one snapshot. A document moved by
  a concurrent write between two pages could be seen twice or missed.
  Listeners are unaffected for their first snapshot (it runs under the
  commit lock) and self-correct afterwards; one-shot queries over more than
  500 candidate rows under concurrent writes accept the skew.

**Alternatives rejected.** One read transaction per query: a consistent
snapshot, but exactly the hold-and-wait that deadlocks. A separate pool for
rule lookups: works, at the price of a second pool to size.

---

## Queries and rules

### 8. Rules filter query results

**Context.** In Firestore, "rules are not filters". A query is rejected
unless the rules can be shown to allow *every* document it could return.
Firestore proves this by analysing the query against the rules without
reading data. CandleDB has no such analysis.

Without it, the obvious imitation is: run the query, check each result, and
reject the whole query if any result is forbidden. That leaks. Suppose a
caller may not read other users' profiles and sends "profiles where
`email == "ann@example.com"`". If the answer is "permission denied", a
matching document exists; if it is an empty list, none does. The caller has
learned whether the address is registered, one probe at a time.

**Decision.** For queries and query listeners, rules act as a filter.
Documents the caller may not `list` are left out as if they did not exist.
The filter is the `keep` function passed into the store, so it runs before
the limit.

**Consequences.**

- No existence leak: an unreadable document and a missing one are
  indistinguishable in query results.
- Limits and pagination stay exact, because filtering happens inside the
  scan.
- A mistake in the rules shows up as missing results, not as an error. This
  is harder to debug than Firestore's refusal, and it is the main
  behavioural difference from Firestore
  ([tour](../tour/10-differences-from-firestore.md)).
- Each candidate document costs one rule evaluation. A rule with `get()`
  costs a document read per candidate.
- Single-document reads are different on purpose: `Service.Get` returns
  "permission denied", and it checks the rules *before* checking existence,
  so the error does not reveal existence either.

**Alternatives rejected.** Static analysis as in Firestore: the correct
design and a research-grade project. Reject-if-any-forbidden: leaks, as
shown.

---

## Realtime

### 9. Server-Sent Events, one stream, many targets

**Context.** Listeners need the server to push data. Browsers offer
WebSockets and Server-Sent Events (SSE). Browsers also limit HTTP/1.1
connections to about six per host, so one connection per listener is not
viable.

**Decision.** A client opens one SSE stream (`GET /v1/listen`). The first
message carries a session ID. Listeners ("targets") are added and removed
with ordinary HTTP requests naming the session, and all their events arrive
on the one stream, each tagged with the client-chosen target number.

**Consequences.**

- The server side is the standard library only: an HTTP response that is
  never finished.
- Proxies, load balancers and HTTP/2 treat it as plain HTTP.
- Communication from client to server costs a request per target change,
  which is rare.
- The browser's `EventSource` cannot send an `Authorization` header, so the
  SDK reads the stream with `fetch` and parses the SSE framing itself.
- The session ID appears in URLs and therefore in access logs. It is not
  sufficient on its own: `Server.session` also requires the caller to be the
  principal that opened the session.

**Alternatives rejected.** WebSockets: bidirectional and a little more
efficient, but they need a third-party library or a hand-written protocol
implementation, and more care with proxies. Long polling: simple and
wasteful.

### 10. Query listeners keep no state; limited ones re-run the query

**Context.** When a document changes, the server must tell each query
listener whether the document entered, changed within or left its result.

**Decision.** For a query without a limit, membership depends only on the
document itself. `queryChanges` in
[`app/listen.go`](../../internal/app/listen.go) evaluates the query on the
document's state before and after the commit and derives added, modified or
removed. The server remembers nothing per listener. For a query *with* a
limit, a document leaving the result must be replaced by the next one, which
cannot be known from the change alone, so `limitedQueryChanges` runs the
query again and compares with the set of `(path, version)` it last sent.

Every target with a stake in a commit receives an event for it, even an
empty one, so the client learns the version it is now up to date with.

**Consequences.**

- Memory per unlimited listener is constant, however large its result.
- The server may send "removed" for a document the client never had, or
  "modified" for one it did not have (when a rule's answer changed).
  Clients must treat events as idempotent: a change to an unknown document
  is an addition, a removal of an unknown document is ignored. The SDK does.
- The "before" state is not checked against the rules, only the "after"
  state. This is what makes the two cases above possible, and harmless.
- Rules that depend on *other* documents (`get()` in a read rule) are
  evaluated only when the listened document itself changes. If ann is
  removed from a room, her listener on the room's messages is not told until
  a message changes or she reconnects.
- **A limited listener's event may carry state newer than its version.** The
  re-run happens on the session's goroutine, after the commit lock is
  released, so it may already include commit 43 while the event is stamped
  42. The event for 43 then finds nothing new and is empty. Clients
  converge, but the SDK's "has my write been confirmed" logic can, for a
  moment, apply a client's own increment on top of server state that
  already contains it.
- A commit touching a collection costs one query per limited listener on
  that collection.

**Alternatives rejected.** Keeping every listener's full ordered result on
the server: exact indexes for the client, memory proportional to all results
of all listeners. Sending the full result on every change: trivial and
quadratic.

### 11. A slow listener is disconnected

**Context.** Commits produce events faster than a stalled client reads them.
Something must give: the writer waits, the server buffers without limit, or
the listener is dropped.

**Decision.** Each session has a bounded queue (256 commits by default).
`hub.publish` adds to it without ever blocking. If the queue is full the
session is closed; the client reconnects and receives fresh snapshots.

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

**Consequences.**

- A writer's latency does not depend on any reader. `TestSlowListenerIsDisconnected`
  runs a hundred commits past a listener that never reads.
- Memory per session is bounded.
- A client on a poor connection during a burst of writes is cut off and
  pays for a full resync, which is the expensive moment to pay.

**Alternatives rejected.** Blocking the commit: one dead phone stops the
database. Unbounded buffering: one dead phone eventually takes the server's
memory. Dropping individual events: the client's state would be silently
wrong.

### 12. Reconnecting means starting over

**Context.** After a network drop, a client's listeners are behind by an
unknown number of commits.

**Decision.** The server keeps no log of past changes. A reconnecting client
opens a new session and adds its targets again; each gets a complete
snapshot marked as a reset. The SDK compares the snapshot with what it had
and reports only real differences to the application.

**Consequences.**

- No change log to store, trim or replay; a server restart is the same as a
  network drop.
- Reconnecting costs a full query per listener, however little changed.
  Firestore avoids this with resume tokens.
- The application sees no flicker, because the SDK diffs.

**Alternatives rejected.** A change log with resume tokens: the natural
next step, and the version counter is already the right token. It was left
out of the first version because it needs retention rules and an answer for
"the log no longer reaches back that far", which is the same full resync.

---

## Values

### 13. A closed set of value types, with exact number ordering

**Context.** Documents hold JSON-like values. Queries must order values of
different types consistently, and a JavaScript client will send numbers that
are sometimes integers and sometimes not.

**Decision.** `domain.Value` is an interface with an unexported method, so
only the eight types in
[`domain/value.go`](../../internal/domain/value.go) implement it: `Null`,
`Bool`, `Int`, `Double`, `Timestamp`, `String`, `Array`, `Map`. Integers and
doubles are distinct types that compare with each other numerically.
`NumberKey` defines the order by a pair, the value as `float64` and then as
a saturated `int64`, and the index encoder writes the same pair.

**Consequences.**

- A type switch over the eight types is complete; the compiler cannot check
  that, but no other package can add a ninth.
- `1` and `1.0` are equal in queries; `9007199254740993` (2^53 + 1) keeps
  its exact value and sorts after `9007199254740992`, where a plain
  `float64` comparison would call them equal.
- The index order and the in-memory order agree by construction, and
  `TestKeyEncodingPreservesOrder` checks it on random values.
- There are no bytes, references or geographic points yet. Adding a type
  means touching `Compare`, the JSON codec, the key encoder, the rules
  engine's `is` operator and the SDK's comparison, in step.
- JSON cannot express "this whole number is a double", so doubles are
  written with a decimal point (`2.0`). A JavaScript client reads that as
  `2` and writes back `2`, which arrives as an integer. Equality and order
  are unaffected; only the rules test `is float` would notice.

**Alternatives rejected.** One number type (`float64`): simpler, loses
integers above 2^53, which matter for IDs and counters. `any` with
`map[string]any`: no compile-time help at all, and nowhere to hang
`Compare`.

---

## Accounts

### 14. Built-in accounts with HS256 tokens

**Context.** A backend for a frontend needs to know who is calling. Firebase
has a separate authentication product.

**Decision.** The server has its own email and password accounts. Passwords
are hashed with bcrypt. A sign-in returns a JSON Web Token signed with
HMAC-SHA256 (valid one hour by default) and an opaque refresh token (thirty
days), stored only as a SHA-256 hash and replaced on every use. An optional
admin key, compared in constant time, bypasses the rules for server-side
callers.

**Consequences.**

- The server works alone: no identity provider to configure.
- Verifying a token needs no database read.
- Any system that knows the secret can mint tokens the server accepts, with
  extra claims that rules can read as `request.auth.token.<name>`. This is
  the integration point for an existing login system.
- An access token cannot be revoked before it expires. Sign-out revokes the
  refresh token; the access token lives out its hour.
- Symmetric signing means every verifier holds the power to sign. There is
  no JWKS endpoint and no support for tokens signed by third parties with
  RS256 or ES256.
- No email verification, password reset or second factor. These need an
  email sender, which is a driven port waiting to be added.

**Alternatives rejected.** Verify external tokens only: forces every user of
the tool to set up an identity provider before writing a first document.
Server-side sessions instead of JWTs: revocable, at the price of a lookup
per request.

---

## Reading the decisions together

Three assumptions run through the list:

1. **One process owns the database.** Decisions 1, 2 and 4 rely on it. It is
   the main thing to revisit before running more than one server.
2. **Correct and simple first, fast where it is cheap.** Decisions 5, 6, 7
   and 10 each pick the version that is easy to reason about and accept a
   known cost.
3. **The client can be asked to cooperate.** Decisions 3, 10 and 12 keep the
   server stateless by having the SDK retry, deduplicate and diff.

When a requirement changes, find the decision whose context it invalidates.
That tells you which part of the code to reopen, and the "alternatives
rejected" paragraph tells you where to start.

Next: [Testing strategy](04-testing-strategy.md)
