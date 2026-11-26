# 04 · Interfaces and ports

## What you will learn

- How a type satisfies an interface without declaring that it does
- Why Go interfaces are small, and where they are declared
- How that placement produces the dependency direction of a hexagonal
  architecture
- The compile-time assertion `var _ I = (*T)(nil)`
- Struct embedding, and using it to wrap or partly override a type
- `sync.Mutex` and `sync.RWMutex`
- Passing a function as an argument
- Why the clock is a dependency

## The feature

CandleDB's use cases (get a document, run a query, commit writes) need
somewhere to keep documents. They do not care where. In production that is a
SQLite file; in most tests it is a few Go maps in memory; in between sits a
wrapper that counts calls for the `/metrics` endpoint. The tour's
[Operations](../tour/08-operations.md) page shows the production side.

The use cases reach storage through an interface called `Store`. The same
holds for everything else the core needs from outside: the time, random IDs,
password hashing and token signing. In the vocabulary of hexagonal
architecture these interfaces are **ports** and the types that implement
them are **adapters**. This chapter covers the Go mechanics. The
architectural reasoning is in
[Hexagonal architecture](../architecture/01-hexagonal.md).

## Where the code is

1. [`internal/app/ports.go`](../../internal/app/ports.go): every port, in one
   file. Read all of it; it is the list of what the core asks of the world.
2. [`internal/adapters/memory/store.go`](../../internal/adapters/memory/store.go):
   the shortest implementation of `Store`, about a hundred lines.
3. [`internal/adapters/memory/users.go`](../../internal/adapters/memory/users.go):
   the account store, with a plain mutex.
4. [`internal/adapters/instrumented/store.go`](../../internal/adapters/instrumented/store.go):
   a `Store` that wraps another `Store`.
5. [`internal/adapters/bcrypthash/bcrypt.go`](../../internal/adapters/bcrypthash/bcrypt.go)
   and [`internal/adapters/jwtauth/jwt.go`](../../internal/adapters/jwtauth/jwt.go):
   two small adapters around third-party libraries.
6. [`cmd/candledb/serve.go`](../../cmd/candledb/serve.go): where the pieces
   are connected.

## An interface is a set of methods

Here is the storage port:

```go
type Store interface {
	// Get returns the document at path, or an error wrapping
	// domain.ErrNotFound.
	Get(ctx context.Context, path domain.DocPath) (domain.Document, error)

	// Query returns the documents selected by q, in query order. keep, when
	// not nil, is an extra filter that is applied before the limit, so a
	// limited query still returns as many documents as it can.
	Query(ctx context.Context, q domain.Query, keep func(domain.Document) bool) ([]domain.Document, error)

	// Apply atomically stores a batch of mutations and records version as
	// the new version of the database. Either all of it happens or none.
	Apply(ctx context.Context, version int64, batch []Mutation) error

	// Version returns the version recorded by the last Apply, or 0.
	Version(ctx context.Context) (int64, error)

	Close() error
}
```

An interface type lists method signatures. A variable of type `Store` can
hold any value whose type has all five methods. Calling `store.Get(...)` on
it runs the method of whatever concrete type is inside.

The comments are part of the contract. The signature says `Get` returns a
document and an error; only the comment says that a missing document is
reported as an error wrapping `domain.ErrNotFound`. Chapter 03 showed how
`storetest` turns those comments into tests.

## Satisfaction is implicit

In Java or C# a class names the interfaces it implements. In Go it does not.
A type satisfies an interface simply by having the methods. Here is the
start of the in-memory store:

```go
// Store is an app.Store that lives in memory.
type Store struct {
	// mu guards every field below. Readers take it shared, writers
	// exclusively, so any number of reads can run at the same time.
	mu          sync.RWMutex
	collections map[domain.CollectionPath]map[string]domain.Document
	version     int64
}
```

Nothing in this declaration mentions `app.Store`. The type `*Store` happens
to have methods named `Get`, `Query`, `Apply`, `Version` and `Close` with the
right signatures, so a `*Store` can be passed wherever an `app.Store` is
wanted.

This has a consequence that takes a while to appreciate: **the interface can
be written after, and far away from, the types that satisfy it**. The
standard library's `*sql.Row` and `*sql.Rows` both have a `Scan` method. The
SQLite adapter declares its own one-method interface, `scanner`, and both
library types satisfy it, although their authors never heard of it:

```go
// scanner is satisfied by both *sql.Row and *sql.Rows, so one function can
// decode a document from either.
type scanner interface {
	Scan(dest ...any) error
}
```

### The compile-time assertion

Implicit satisfaction has one drawback. If `memory.Store` were missing a
method, the compiler would say so only at the place where a `*Store` is
first used as an `app.Store`, possibly in another package, with an error
that points away from the real problem. One line fixes that:

```go
// This line makes the compiler verify that *Store implements app.Store.
// Without it a missing method would only be reported where a Store is first
// passed to something expecting the interface.
var _ app.Store = (*Store)(nil)
```

Read it from the right. `(*Store)(nil)` is a nil pointer converted to the
type `*Store`: a value of the right type that costs nothing. It is assigned
to a variable of type `app.Store`, which only compiles if `*Store` satisfies
the interface. The variable is named `_`, the blank identifier, so nothing is
kept. The line exists purely so that the compiler checks it. Every adapter
in this codebase has one.

Note that it is `*Store`, the pointer type, that satisfies the interface,
because the methods have pointer receivers (`func (s *Store) Get`). A plain
`Store` value does not. The bcrypt adapter is the opposite case: its methods
have value receivers, so the assertion uses a value:

```go
// Hasher hashes passwords with bcrypt at a configurable cost. Each step up
// in cost doubles the time one hash takes, for the server and for an
// attacker trying guesses against a stolen database alike.
type Hasher struct{ cost int }

var _ app.PasswordHasher = Hasher{}
```

Mixing these up gives the compile error "method has pointer receiver", which
puzzles everyone the first time. The rule: methods with a pointer receiver
belong only to the pointer type; methods with a value receiver belong to
both.

## Small interfaces

Go interfaces are small, often one method. The smaller an interface, the
more types satisfy it and the easier it is to fake. Two of candledb's ports,
and the production implementation of the first:

```go
// Clock tells the time. Production code uses the system clock; tests pass a
// clock they control so that timestamps and token expiry are predictable.
type Clock interface {
	Now() time.Time
}

// IDGenerator produces identifiers for new documents, users and sessions.
type IDGenerator interface {
	NewID() string
}

// SystemClock is the Clock backed by the operating system.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }
```

`Clock` and `IDGenerator` have one method each. `SystemClock` is the real
implementation of `Clock`: an empty struct (`struct{}` has no fields and
takes no memory) with one method. `fakeClock` in the tests is the other.

`Store` has five methods, which is large by Go standards, and
`UserStore` has five as well. They are cohesive (each is one storage
concern), so they were not split further. If you find yourself writing an
interface with fifteen methods, it is probably several interfaces.

## Where interfaces are declared

In many languages the interface lives next to its implementation, or in a
shared "interfaces" package. The Go convention is different: **declare the
interface in the package that uses it**. `Store` is declared in package
`app`, which calls it. The packages that implement it, `memory` and
`sqlite`, import `app` to refer to it.

Look at what that does to the imports. The in-memory adapter:

```go
import (
	"context"
	"fmt"
	"sync"

	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/domain"
)
```

The adapter imports `app` and `domain`. Now list what `app` imports:

```sh
go list -f '{{join .Imports "\n"}}' ./internal/app
```

The only project packages in the output are `github.com/bshadmehr98/candledb/internal/domain` and
`github.com/bshadmehr98/candledb/internal/rules`. Package `app` does not import `memory`, `sqlite`,
`httpapi` or any other adapter, and because Go forbids import cycles, it
**cannot**: the adapters import `app`, so an import in the other direction
would not compile.

That is the whole mechanism behind the hexagonal layout. The core defines
what it needs as interfaces; the adapters depend on the core to implement
them; the compiler rejects any attempt by the core to depend on an adapter.
The architecture is not a guideline that erodes over time. It is enforced on
every build.

A saying summarises the two halves of the convention: **accept interfaces,
return structs**. The constructor of the core service takes the interface:

```go
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
```

`store Store` accepts anything that satisfies the port. (`opts ...Option` is
a variadic parameter: the caller may pass any number of `Option` values. It
is the functional options pattern, described in the architecture track.) The
function returns
`*Service`, a concrete type, not an interface: the caller gets the full type
with all its methods and documentation, and is free to define its own
interface over the part it needs. Likewise `memory.NewStore()` returns
`*memory.Store`, not `app.Store`.

Something has to know the concrete types and connect them. That is `main`.
From `serve` in [`cmd/candledb/serve.go`](../../cmd/candledb/serve.go):

```go
// Assemble the hexagon: driven adapters, then the core, then the
// driving adapter.
store := instrumented.Wrap(db)
docs, err := app.New(ctx, store, app.WithLogger(logger), app.WithRules(ruleset))
if err != nil {
	return err
}
```

`db` is a `*sqlite.DB`. `instrumented.Wrap(db)` returns a store that wraps
it. `app.New` receives that as an `app.Store` and never learns what is
inside. Replacing SQLite would change these lines and no line in `app`.

## A wrapper that is also a Store

The instrumented store is a `Store` that contains another `Store`:

```go
// Store is an app.Store that records statistics about another one.
type Store struct {
	next app.Store

	Gets    OpStats
	Queries OpStats
	Applies OpStats
}

var _ app.Store = (*Store)(nil)

// Wrap returns a Store that forwards to next.
func Wrap(next app.Store) *Store { return &Store{next: next} }
```

Each of its methods measures, then forwards:

```go
func (s *Store) Query(ctx context.Context, q domain.Query, keep func(domain.Document) bool) (docs []domain.Document, err error) {
	defer func(start time.Time) { s.Queries.record(start, err) }(time.Now())
	return s.next.Query(ctx, q, keep)
}
```

`s.next.Query(...)` calls the wrapped store. The line before it uses
`defer` with a function literal that is called immediately with `time.Now()`
as its argument. The argument is evaluated when the `defer` statement runs,
so `start` is the time before the query; the body runs when `Query`
returns, so it records the time after. The results are *named*
(`docs`, `err`) so that the deferred function can read the final `err`.

Because the wrapper has the same interface as what it wraps, the service
cannot tell the difference, and wrappers can be stacked. This arrangement is
the decorator pattern, which the architecture track describes among the
design patterns used in the code.

## Embedding

Writing a forwarding method for every method of an interface is tedious when
you only want to change one or two. **Embedding** removes the tedium. A
struct can include another type without giving the field a name, and the
methods of that type are *promoted*: they can be called on the outer struct
as if they were its own.

The HTTP adapter uses it to record the status code of a response. From
[`internal/adapters/httpapi/middleware.go`](../../internal/adapters/httpapi/middleware.go):

```go
// statusRecorder remembers the status code a handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the real writer, which is how
// the listen handler can still flush and set deadlines through this wrapper.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
```

`http.ResponseWriter` is an interface with three methods: `Header`,
`Write` and `WriteHeader`. By embedding it, `statusRecorder` gets all three
and therefore satisfies `http.ResponseWriter` itself. It then declares its
own `WriteHeader` and `Write`, which take precedence over the promoted ones,
note the status, and call the embedded writer explicitly as
`r.ResponseWriter.WriteHeader(status)`. `Header` is not redeclared, so calls
to it go straight to the embedded writer.

The embedded field can be referred to by its type name, here
`r.ResponseWriter`. That is also how it is set in a literal:
`&statusRecorder{ResponseWriter: w}`.

A test uses the same technique to build a broken store in four lines. From
[`internal/adapters/instrumented/store_test.go`](../../internal/adapters/instrumented/store_test.go):

```go
// failing is a store whose Apply and Query always fail. Embedding the
// app.Store interface gives it every method; only the ones under test are
// overridden.
type failing struct{ app.Store }

func (failing) Apply(context.Context, int64, []app.Mutation) error { return errors.New("disk full") }
```

`failing{memory.NewStore()}` has every method of `app.Store` through the
embedded interface; only `Apply` and `Query` are replaced by versions that
return an error.

Embedding is not inheritance, and treating it as such is the usual mistake.
There is no "is-a" relation and no virtual dispatch: when a promoted method
of the inner type calls another of its own methods, it calls the inner
type's version, never an "override" on the outer struct. Embedding is
composition with shorter syntax.

## Mutexes

The in-memory store is used by many goroutines at once: every HTTP request
runs in its own (chapter 09). A Go map must not be read while another
goroutine writes to it. The store protects its maps with a lock. Look again
at the struct at the top of this chapter: the field `mu sync.RWMutex` comes
first, and the comment says what it guards. Putting the mutex directly above
the fields it protects, with such a comment, is the convention.

A `sync.Mutex` has two methods, `Lock` and `Unlock`; only one goroutine can
hold it at a time. A `sync.RWMutex` distinguishes readers from writers:
any number of goroutines can hold it for reading (`RLock`/`RUnlock`), or
exactly one for writing (`Lock`/`Unlock`).

Writing takes the exclusive lock:

```go
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
```

`defer s.mu.Unlock()` on the line after `Lock` guarantees the lock is
released on every path out of the function.

The zero value of a mutex is an unlocked mutex, so `NewStore` does not
initialise it. A struct that contains a mutex must not be copied after first
use, since the copy would have its own separate lock. That is one more
reason the store's methods have pointer receivers. `go vet` reports
accidental copies.

### Why Query copies before it filters

Reading takes the shared lock, but `Query` does something `Get` does not:

```go
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
```

It takes the read lock, copies the candidate documents into a slice,
**releases the lock**, and only then filters. It does not use `defer` here,
on purpose, because the lock must be released before the function ends.

The reason is the `keep` argument, a function supplied by the caller. When
security rules are active, `keep` evaluates a rule for each document, and a
rule can call `get(...)`, which calls `Store.Get`, which takes the read lock
again. A goroutine that takes an `RWMutex` read lock while it already holds
one can deadlock: if a writer started waiting in between, the second `RLock`
waits behind the writer, and the writer waits for the first read lock to be
released. Neither can proceed. The `sync` documentation prohibits recursive
read locking for exactly this reason.

The general rule is worth remembering beyond this case: **do not call code
you do not control while holding a lock**. Copy what you need, unlock, then
call out. The storage contract has a test for it (`testQueryKeep` calls
`Get` from inside `keep`).

Copying the slice is cheap because a `domain.Document` holds a reference to
its data map, not the data, and the documents are immutable (chapter 01).
The lock protects the store's maps; immutability protects the documents.

## Functions as arguments

`keep` is a parameter whose type is a function type:
`keep func(domain.Document) bool`. Functions are values in Go. They can be
stored in variables, passed and returned, and `nil` is the zero value of a
function type.

The caller builds `keep` as a closure. From `runQuery` in
[`internal/app/read.go`](../../internal/app/read.go):

```go
var keep func(domain.Document) bool
if !p.Admin {
	keep = func(doc domain.Document) bool {
		return s.allowed(ctx, p, rules.MethodList, doc.Path, &doc, nil)
	}
}
docs, err := s.store.Query(ctx, q, keep)
```

For an admin `keep` stays `nil` and the store skips the check
(`keep == nil || keep(doc)` in `domain.Query.Apply`). For everyone else it is
a closure that captures the service `s`, the context and the principal `p`,
and asks the rules about one document.

Notice what this buys. The store needs to apply a filter in the middle of
its scan, before the limit, but it must not know about security rules; the
service knows the rules but not how the store scans. A one-method interface
would work too. A function value is the lighter tool when the behaviour is a
single operation, and you will see it used that way throughout Go: sort
comparators, HTTP handlers, the `fn` of `inTx` in chapter 02.

## Why the time is a port

It may look excessive to hide `time.Now()` behind an interface. The service
uses it like this, from `Refresh` in
[`internal/app/auth.go`](../../internal/app/auth.go):

```go
if !a.clock.Now().Before(stored.ExpiresAt) {
	return AuthSession{}, invalid
}
```

`a.clock` is an `app.Clock`. In production it is `SystemClock`. In
`TestRefreshRotatesTokens` it is a `fakeClock`, and the test advances it by
25 hours in one statement to check that a refresh token expires.

Time, randomness, the network and the file system are all inputs your code
does not control. Code that reads them directly can only be tested slowly or
not at all. Code that receives them through a port can be tested exactly.
`IDGenerator` exists for the same reason: tests use `sequentialIDs`, which
returns `id-1`, `id-2` and so on, so that assertions can name IDs.

The balance to strike: introduce an interface when you have a second
implementation, and a test fake counts as one. Do not wrap every dependency
in an interface "in case". The `Service` uses `log/slog` and `sync`
directly, because nothing would be gained by abstracting them.

## Try it

```sh
go doc ./internal/app Store
go list -f '{{join .Imports "\n"}}' ./internal/app
go test ./internal/adapters/memory -run TestStoreContract -v | grep -c PASS
go test -race ./internal/adapters/instrumented -v
go vet ./...
```

Then see the compile-time assertion work. In
[`internal/adapters/memory/store.go`](../../internal/adapters/memory/store.go),
rename the method `Version` to `Versions` and run `go build ./...`. The
error is on the `var _ app.Store = (*Store)(nil)` line, in the file you
changed, and names the missing method. Now delete the assertion line as well
and build again: the build **succeeds**, because no non-test code uses the
memory store as an `app.Store`. The mistake only shows up when you run
`go vet ./...` or the tests, as errors in the test files of three other
packages. Undo both changes.

Finally, see the import rule work. Add
`import _ "github.com/bshadmehr98/candledb/internal/adapters/memory"` to
`internal/app/service.go` and run `go build ./...`. The build fails with
"import cycle not allowed". Remove the line.

## Exercise: a read-only store

**Goal.** Write a new adapter, `internal/adapters/readonly`, that wraps any
`app.Store`, forwards reads, and refuses every write. It could back a
maintenance mode in which the server answers queries but changes nothing.

**What to do.**

1. Create `internal/adapters/readonly/store.go` with a type that holds
   (or embeds) an `app.Store` and a constructor `Wrap(next app.Store)`.
2. `Apply` returns an error that wraps `domain.ErrUnavailable`, with a
   message that tells an API user what is going on. Everything else
   forwards.
3. Add the compile-time assertion.
4. Write `store_test.go`. Check that reads return what the wrapped store
   holds, that `Apply` fails with `ErrUnavailable` (use `errors.Is`), and
   that the wrapped store is unchanged afterwards.

**Hints.**

- Embedding makes the adapter very short. Decide consciously whether you
  want that: with embedding, a method added to `app.Store` in the future is
  forwarded automatically, including a method that writes. With explicit
  forwarding, the compiler makes you look at each new method. For a wrapper
  whose purpose is safety, which is better?
- You cannot run the whole `storetest.Run` suite against a store that
  refuses writes, because the suite writes. Seed a `memory.Store` first,
  wrap it, and test the wrapper's reads against it.
- `ErrUnavailable` already maps to HTTP 503 in `httpapi/errors.go`.

**Stretch.** Add a `-read-only` flag to `cmd/candledb/serve.go` that wraps
the store when set, and an end-to-end check that a `PUT` then answers 503
while a `GET` still works. One line changes in the composition root; nothing
changes in `app`. That is the hexagon paying off.

**How you know it works.** `go test ./internal/adapters/readonly` and
`go vet ./...` pass.

## Further reading

- [A Tour of Go: Methods and interfaces](https://go.dev/tour/methods/1)
- [Effective Go: Interfaces and other types](https://go.dev/doc/effective_go#interfaces_and_types)
- [Effective Go: Embedding](https://go.dev/doc/effective_go#embedding)
- [Go Wiki: Go Code Review Comments, "Interfaces"](https://go.dev/wiki/CodeReviewComments#interfaces):
  why interfaces belong in the package that uses them.
- [Package sync](https://pkg.go.dev/sync): read the documentation of
  `RWMutex` in particular.

**Next:** [05 · HTTP](05-http.md)
