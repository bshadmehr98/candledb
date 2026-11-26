# 03 · Testing

## What you will learn

- How Go tests are written and run, with nothing but the standard library
- Table-driven tests and subtests
- Test helpers: `t.Helper`, `t.Cleanup`, `t.TempDir`
- The two kinds of test package, external and internal
- Replacing a dependency with a fake through an interface
- Property tests, a differential test, and a contract test suite
- Fuzzing and benchmarks
- The race detector and coverage
- Integration tests over real HTTP, and a test of the built program
- How to wait for something concurrent without sleeping

## The feature

This chapter's "feature" is the test suite itself. CandleDB has tests at
every level, from a single function that compares two values up to a test
that starts the real `serve` command, sends it signals and reads its log.
They are also the most reliable documentation in the repository: a test
states precisely what the code is supposed to do, and it cannot go out of
date without failing.

When you want to understand a piece of this codebase, read its tests next to
it. When you do the exercises, the tests tell you whether you broke anything.

## Where the code is

Tests live beside the code they test, in files whose names end in
`_test.go`. The compiler ignores those files except when you run `go test`.

| Level | Files | What they show |
|---|---|---|
| Unit | [`internal/domain/*_test.go`](../../internal/domain/) | table tests, properties |
| Unit | [`internal/rules/rules_test.go`](../../internal/rules/rules_test.go) | a table as a language specification, fuzzing |
| Contract | [`internal/app/storetest/storetest.go`](../../internal/app/storetest/storetest.go) | one suite for every storage adapter |
| Adapter | [`internal/adapters/sqlite/*_test.go`](../../internal/adapters/sqlite/) | differential test, benchmarks |
| Use case | [`internal/app/*_test.go`](../../internal/app/) | fakes, concurrency |
| Integration | [`test/integration/`](../../test/integration/) | the assembled server over HTTP |
| End to end | [`cmd/candledb/main_test.go`](../../cmd/candledb/main_test.go) | the real command |

## The testing package

A test is a function named `TestSomething` that takes one parameter of type
`*testing.T`. There is no assertion library and no annotations: a test fails
by calling a method on `t`.

- `t.Errorf(...)` records a failure and **continues**.
- `t.Fatalf(...)` records a failure and **stops this test**.

Use `Errorf` by default, so that one run reports every problem, and `Fatalf`
when continuing makes no sense, for example when the setup failed. Failure
messages are written by hand, and the convention is to state what you got and
what you wanted: `Parent = "x", ID = "y"`.

`go test ./internal/domain` compiles the package together with its test
files and runs every `Test` function. Useful flags:

| Flag | Effect |
|---|---|
| `-run REGEX` | only tests whose name matches |
| `-v` | print every test, not only failures |
| `-count=1` | do not reuse a cached result |
| `-short` | sets `testing.Short()`, which long tests check to do less |
| `-race` | enable the race detector |

Go caches test results. If nothing a package depends on has changed, `go
test` prints `(cached)` and does not run it again.

## Table-driven tests

Most tests check the same thing for many inputs. The Go idiom is to put the
inputs and expected outputs in a slice of structs, the *table*, and loop over
it. From [`internal/domain/value_test.go`](../../internal/domain/value_test.go):

```go
func TestJSONKeepsNumberTypes(t *testing.T) {
	tests := []struct {
		json string
		want domain.Value
	}{
		{`1`, domain.Int(1)},
		{`1.0`, domain.Double(1)},
		{`-3`, domain.Int(-3)},
		{`1e3`, domain.Double(1000)},
		{`9007199254740993`, domain.Int(9007199254740993)},
		{`9223372036854775808`, domain.Double(9223372036854775808)}, // one past int64
	}
	for _, tt := range tests {
		got, err := domain.ParseJSON([]byte(tt.json))
		if err != nil {
			t.Errorf("ParseJSON(%s): %v", tt.json, err)
			continue
		}
		// Comparing with == checks the Go type as well as the number.
		if got != tt.want {
			t.Errorf("ParseJSON(%s) = %#v, want %#v", tt.json, got, tt.want)
		}
	}
```

The table is a slice of an anonymous struct type with two fields, `json` and
`want`. Each `{...}` line is one case. Adding a case is adding a line, which
is what makes the form attractive: you can see at a glance which cases are
covered, and reviewers can ask "what about zero?" by pointing at a gap.

`tt` is the conventional name for the current row. `continue` moves to the
next row after a failed parse, because there is no value to compare.

The comparison `got != tt.want` deserves a second look. Comparing two
interface values with `!=` compares their dynamic types as well as their
contents, so it distinguishes `Int(1)` from `Double(1)`, which
`domain.Equal` on purpose does not.

Some tables are the specification. `TestExpressions` in
`internal/rules/rules_test.go` has more than eighty rows of the form
`{"1 + 2 * 3 == 7", true}`; reading it is the quickest way to learn the rules
language exactly.

## Subtests with t.Run

`t.Run(name, func)` runs a function as a named **subtest**. From the same
file:

```go
for name, input := range bad {
	// t.Run gives each case its own name in the output and lets
	// "go test -run TestParseJSONRejects/too_deep" run one alone.
	t.Run(name, func(t *testing.T) {
		if _, err := domain.ParseJSON([]byte(input)); err == nil {
			t.Errorf("ParseJSON accepted %s", input)
		}
	})
}
```

Here the table is a map from a case name to an input. Each case becomes a
subtest with its own line in the output and its own pass or fail. A subtest
can be run alone:

```sh
go test ./internal/domain -run 'TestParseJSONRejects/too_deep' -v
```

Spaces in subtest names become underscores on the command line. Inside a
subtest, `t.Fatal` ends only that subtest, so one failing case does not hide
the others.

The function passed to `t.Run` is a closure that uses the loop variables
`name` and `input`. Since Go 1.22 every iteration of a `for` loop has its own
copy of those variables, so this is safe. In older code you will see
`tt := tt` at the top of such loops; that was the workaround for the earlier
rule, where all iterations shared one variable.

## Helpers: t.Helper, t.Cleanup, t.TempDir

Repeated setup goes into ordinary functions. Two things make them good test
helpers. From [`internal/domain/write_test.go`](../../internal/domain/write_test.go):

```go
// mustMap parses a JSON object into a Map, failing the test on bad input.
// t.Helper() makes a failure point at the caller's line instead of here.
func mustMap(t *testing.T, s string) domain.Map {
	t.Helper()
	m, err := domain.ParseMapJSON([]byte(s))
	if err != nil {
		t.Fatalf("bad test data %s: %v", s, err)
	}
	return m
}
```

`t.Helper()` marks the function as a helper. When it fails, the reported
file and line are those of the **caller**, the test that passed bad data,
not the line inside `mustMap`. Forgetting `t.Helper()` is the usual mistake:
every failure then points at the same unhelpful line.

By convention a helper whose name starts with `must` fails the test instead
of returning an error.

The second tool is automatic cleanup. From
[`internal/adapters/sqlite/store_test.go`](../../internal/adapters/sqlite/store_test.go):

```go
// It accepts testing.TB, the interface shared by *testing.T and
// *testing.B, so tests and benchmarks can use the same helper.
func openDB(tb testing.TB) *sqlite.DB {
	tb.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(tb.TempDir(), "test.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { db.Close() })
	return db
}
```

- `tb.TempDir()` creates a fresh directory and deletes it when the test
  ends. Every test gets its own database file.
- `tb.Cleanup(f)` registers `f` to run when the test ends. The helper opens
  the database **and** arranges for it to be closed, so the tests that call
  it need no teardown code and cannot forget it.
- `testing.TB` is an interface that both `*testing.T` and `*testing.B`
  (benchmarks) satisfy, so one helper serves both.

## External and internal test packages

A test file can declare one of two packages.

`package domain_test`, with the `_test` suffix, is an **external** test
package. It is a separate package that imports `github.com/bshadmehr98/candledb/internal/domain`
like any other client, so it can only use exported names. The comment at the
top of `value_test.go` gives the reason: the tests then exercise the package
exactly as other packages will. This is the default choice in this codebase.

`package sqlite`, the same name as the package under test, is an **internal**
test. It is compiled as part of the package and can reach unexported names.
From [`internal/adapters/sqlite/keyenc_test.go`](../../internal/adapters/sqlite/keyenc_test.go):

```go
package sqlite

// This file is in package sqlite, not sqlite_test: it tests encodeKey and
// planQuery, which are unexported. An "internal" test like this can reach
// everything in the package.
```

`encodeKey` and `planQuery` are implementation details that deserve direct
tests but should not be exported just to be testable. Both kinds of file can
sit in the same directory; `internal/adapters/sqlite` has `store_test.go`
(external) next to `keyenc_test.go` (internal).

## Fakes through interfaces

The account service needs the time: tokens expire. A test that waited an
hour for a token to expire would be useless. Because the service asks for
the time through an interface (`app.Clock`, chapter 04), a test can hand it
a clock that it controls. From
[`internal/app/helpers_test.go`](../../internal/app/helpers_test.go):

```go
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
```

A **fake** is a small working implementation written for tests. `fakeClock`
has the `Now` method that `app.Clock` requires, plus `Advance` for the test.
It needs no mocking framework and no code generation. It has a mutex because
the service may read the clock from several goroutines.

The test of token refresh then makes two hours pass in no time. From
[`internal/app/auth_test.go`](../../internal/app/auth_test.go):

```go
func TestRefreshRotatesTokens(t *testing.T) {
	auth, clock, _ := newAuth(t, app.AuthConfig{AccessTTL: time.Hour, RefreshTTL: 24 * time.Hour})
	first, err := auth.SignUp(ctx, "ada@example.com", "correct horse")
	if err != nil {
		t.Fatal(err)
	}

	clock.Advance(2 * time.Hour)
	if _, err := auth.Authenticate(first.AccessToken); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("expired access token: %v", err)
	}
```

The same file has `plainHasher` and `fakeTokens`, which replace bcrypt and
JWT. `plainHasher` also counts how often it is asked to verify a password,
and `TestSignInFailuresLookAlike` uses the count to prove that a sign-in
with an unknown email still does the hashing work. A fake can record how it
was used, which is all a "mock" is.

The design consequence matters more than the technique: code is testable
this way only if it receives its dependencies instead of reaching for them.
A function that calls `time.Now()` directly cannot be given a different
clock.

## Property tests

A table checks the cases you thought of. A **property test** states a rule
that must hold for all inputs and checks it against many generated ones.

The index of the SQLite store depends on one property: encoding two values
to bytes and comparing the bytes gives the same answer as comparing the
values. From `keyenc_test.go`:

```go
// TestKeyEncodingPreservesOrder is the property the whole index design
// rests on: comparing two encoded keys byte by byte gives the same answer
// as comparing the two values.
func TestKeyEncodingPreservesOrder(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	values := make([]domain.Value, 400)
	for i := range values {
		values[i] = domaintest.Value(rng, 3)
	}
	for _, a := range values {
		for _, b := range values {
			want := sign(domain.Compare(a, b))
			got := sign(bytes.Compare(encodeKey(a), encodeKey(b)))
			if got != want {
				t.Fatalf("order mismatch\n a = %#v -> % x\n b = %#v -> % x\n values compare %d, keys compare %d",
					a, encodeKey(a), b, encodeKey(b), want, got)
			}
		}
	}
}
```

Four hundred random values give 160,000 pairs. The values come from
[`internal/domain/domaintest`](../../internal/domain/domaintest/random.go), a
small package of generators that favour awkward inputs: zero, negative zero,
2^53, the int64 limits, empty strings, strings containing a zero byte.

Two details make random tests usable:

- **A fixed seed.** `rand.New(rand.NewPCG(7, 11))` produces the same sequence
  on every run. A failure can be reproduced, and the test never fails "only
  sometimes".
- **A failure message with the inputs.** When the property is violated the
  message prints both values and both encodings. Without that, a failing
  random test tells you nothing.

`TestCompareIsATotalOrder` in `internal/domain/value_test.go` uses the same
method to check that `Compare` is reflexive, antisymmetric and transitive,
the properties sorting depends on.

## A differential test

When you have two implementations of the same behaviour, you can test one
against the other. The memory store answers a query by brute force, using
`domain.Query.Apply`, which is the definition of the right answer. The SQLite
store uses indexes, range scans and paging. `TestQueriesAgreeWithReference`
loads the same random documents into both and runs 3,000 random queries. Its
core is this:

```go
want, err := reference.Query(ctx, q, keep)
if err != nil {
	t.Fatal(err)
}
got, err := db.Query(ctx, q, keep)
if err != nil {
	t.Fatalf("query %+v: %v", q, err)
}
if w, g := pathsOf(want), pathsOf(got); w != g {
	t.Fatalf("results differ for query %+v\n plan: %+v\n want: %s\n got:  %s", q, planQuery(q), w, g)
}
```

Nobody wrote down the expected result of any of those queries; the reference
implementation computes it. On a mismatch the message includes the plan the
SQLite store chose, which is what you need to debug it.

The test also guards against a trap of generated tests: if nearly every
random query returned nothing, both stores would agree trivially. It counts
the non-empty results and fails if there are too few.

The loop count is reduced under `-short`: the test checks `testing.Short()`
and runs 300 queries instead of 3,000. That is what `make test-short` is for.

## A contract test suite

`app.Store` is an interface with two implementations (three, counting the
instrumented wrapper). An interface fixes method signatures only. What the
methods must **do** is written in comments, which no compiler checks.

[`internal/app/storetest`](../../internal/app/storetest/storetest.go) turns
those comments into tests that take the store as a parameter:

```go
// Run executes the document store contract. open must return a new, empty
// store each time it is called.
func Run(t *testing.T, open func(t *testing.T) app.Store) {
	tests := map[string]func(*testing.T, app.Store){
		"get missing document":          testGetMissing,
		"apply then get":                testApplyThenGet,
		"overwrite and delete":          testOverwriteAndDelete,
		"batch and version":             testBatchAndVersion,
		"all value types survive":       testValueRoundTrip,
		"collections are isolated":      testCollectionIsolation,
		"query filters orders limits":   testQuery,
		"query keep runs before limit":  testQueryKeep,
		"query sees rewritten indexes":  testQueryAfterRewrite,
		"query pages through many rows": testQueryManyRows,
		"concurrent use":                testConcurrentUse,
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			test(t, store)
		})
	}
}
```

`Run` is an ordinary exported function in an ordinary (non-test) package, so
other packages' tests can import it. `open` is a function that creates a new
empty store; each subtest calls it to get its own. An adapter proves itself
in a few lines. This is all of
[`internal/adapters/memory/store_test.go`](../../internal/adapters/memory/store_test.go)
that concerns documents:

```go
func TestStoreContract(t *testing.T) {
	storetest.Run(t, func(*testing.T) app.Store { return memory.NewStore() })
}
```

The SQLite adapter's test is the same with a different `open`. If you ever
write a third adapter, this suite is its acceptance test, and exercise 04
asks you to do roughly that.

## Fuzzing

Fuzzing is property testing where the tool generates the inputs, guided by
code coverage: it mutates inputs and keeps those that reach new code. It is
suited to anything that parses untrusted text. From
[`internal/rules/rules_test.go`](../../internal/rules/rules_test.go):

```go
func FuzzParse(f *testing.F) {
	f.Add(`match /users/{uid} { allow read: if request.auth.uid == uid; }`)
	f.Add(`function f(a, b) { return a + b * 2 in [1, 2] || !a.size() > 3; } match /a/{b=**} { allow write: if f(1, 2); }`)
	f.Add(`match /a/{b} { allow get, list: if resource.data.x[0].y is string && "x".matches('a\\.b'); }`)
	f.Add(`match`)
	f.Add(`match /{`)
	f.Add("match /a/{b} { allow read: if \"")
	f.Fuzz(func(t *testing.T, src string) {
		rs, err := rules.Parse(src)
		if err != nil {
			return
		}
		// Whatever parses must also evaluate without panicking.
		for _, method := range []rules.Method{rules.MethodGet, rules.MethodCreate} {
			rs.Allow(context.Background(), rules.Request{Method: method, Path: "a/b", Auth: &rules.Auth{UID: "u"}}, nil)
		}
	})
}
```

`f.Add` supplies seed inputs. `f.Fuzz` takes the function to run on each
input. The property here is modest and valuable: for **any** string, `Parse`
returns normally (no panic, no endless loop), and whatever parses can also
be evaluated without a panic.

Run without `-fuzz`, a fuzz function is a normal test that runs the seeds,
so it is part of `go test ./...`. Run with `-fuzz`, it generates inputs until
you stop it or the time is up. If it finds a failing input, it saves it in a
`testdata/fuzz` directory inside the package, and from then on the plain
test replays it.

## Benchmarks

A benchmark is a function named `BenchmarkSomething` taking `*testing.B`. It
measures time and allocations instead of checking results. From
[`internal/adapters/sqlite/bench_test.go`](../../internal/adapters/sqlite/bench_test.go):

```go
func BenchmarkGet(b *testing.B) {
	db := seed(b, 50_000)
	ctx := context.Background()
	for b.Loop() {
		if _, err := db.Get(ctx, "items/i0025000"); err != nil {
			b.Fatal(err)
		}
	}
}
```

`for b.Loop()` runs the body as many times as the framework needs for a
stable measurement, and the setup before the loop (seeding 50,000 documents)
is not counted. Older Go code writes `for i := 0; i < b.N; i++` for the same
purpose.

Benchmarks do not run with plain `go test`; you ask for them with `-bench`.
`-run '^$'` matches no test name, so only benchmarks run:

```sh
go test ./internal/adapters/sqlite -run '^$' -bench BenchmarkGet -benchmem
```

The output line reports nanoseconds, bytes allocated and allocations per
operation. `BenchmarkQueryTopTen` in the same file is worth running: it
shows that fetching the ten largest values takes about as long from 50,000
documents as from 1,000, which is the promise of an ordered index scan.

## The race detector

A **data race** is two goroutines accessing the same variable at the same
time, at least one of them writing, with no synchronisation. Races produce
bugs that appear rarely and cannot be reproduced. Go has a tool that finds
them: with `-race`, the compiler instruments every memory access and the
program reports a race when one actually happens during the run.

```sh
go test -race ./internal/app -run TestConcurrentCommits
```

The detector only sees code that runs, so the suite contains tests whose
purpose is to exercise shared state from several goroutines:
`TestConcurrentCommits`, `TestSnapshotAndChangesAreConsistent`, and
`testConcurrentUse` in the storage contract. They are slower with `-race`
(several times), which is why it is a separate make target, `make
test-race`, and what CI runs. Chapter 09 covers the concurrency itself.

## Waiting without sleeping

Tests of concurrent code have to wait for something that happens in another
goroutine. Writing `time.Sleep(100 * time.Millisecond)` and hoping is the
classic mistake: too short and the test fails at random on a busy machine,
too long and the suite crawls.

Wait for the event itself, with a timeout only as a safety net. When the
event arrives on a channel, `select` does it. From
[`internal/app/listen_test.go`](../../internal/app/listen_test.go):

```go
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
```

The function returns the moment an event is available. The five seconds are
never spent unless something is broken, in which case the test fails with a
clear message instead of hanging. (`select` is explained in chapter 09; read
it here as "whichever happens first".)

When there is no channel to wait on, poll a condition with a deadline. From
[`cmd/candledb/main_test.go`](../../cmd/candledb/main_test.go):

```go
// waitFor polls until cond is true. Polling with a deadline is how a test
// waits for something that happens in another goroutine at an unknown time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
```

## Integration tests

Unit tests with fakes cannot find mistakes in how the parts are connected.
[`test/integration`](../../test/integration/) builds the server the way
`cmd/candledb` does, with a real SQLite file, real bcrypt and real tokens,
and talks to it over HTTP. Nothing is faked.

`start` in [`harness_test.go`](../../test/integration/harness_test.go)
assembles the server and serves it with `httptest.NewServer`, which listens
on a free local port. Small helpers keep the tests readable:

```go
// must performs a request and fails the test unless it returns wantStatus.
func (s *server) must(wantStatus int, method, path, token string, body any) response {
	s.t.Helper()
	res := s.call(method, path, token, body)
	if res.Status != wantStatus {
		s.t.Fatalf("%s %s = %d, want %d\n%s", method, path, res.Status, wantStatus, res.Raw)
	}
	return res
}
```

A test then reads as a sequence of requests and expected status codes. From
[`documents_test.go`](../../test/integration/documents_test.go):

```go
func TestDocumentLifecycle(t *testing.T) {
	s := start(t, options{})

	s.must(404, "GET", "/v1/documents/users/ada", "", nil)

	put := s.must(200, "PUT", "/v1/documents/users/ada", "", `{"name": "Ada", "address": {"city": "London", "zip": "N1"}}`)
	if put.Body["version"] != 1.0 {
		t.Errorf("first commit version = %v", put.Body["version"])
	}
```

Read `TestRulesEndToEnd` in `auth_test.go` for a longer example: three users,
a rules file, and a list of who may and may not do what.

## Testing the program itself

One level remains: `main`. The trick is in how `main` is written. It does
almost nothing and delegates to
`run(ctx, args, stdout, stderr) error`, a function that takes everything
`main` would otherwise get from globals. A test can call `run` with its own
arguments and capture the output. `TestServeEndToEnd` in
[`cmd/candledb/main_test.go`](../../cmd/candledb/main_test.go) starts the
`serve` command on a free port, finds the port in the log output, makes
requests, changes the rules file and sends the process a `SIGHUP`, takes a
backup, cancels the context and checks that the server shuts down cleanly.

Keeping `main` this thin is a pattern to copy in every Go program you write.

## Coverage

Coverage measures which statements ran during the tests:

```sh
make cover
```

This runs the tests with `-coverprofile=coverage.out` and prints the total.
`go tool cover -html=coverage.out` opens a view of the source with covered
lines in green and uncovered lines in red.

Read coverage as a way to find untested code, not as a score. The useful
question is "which lines are red, and does it matter?". In this codebase the
red lines are mostly the marker methods of chapter 01 (`sealed()`), which
are never called, and error returns for failures that are hard to provoke.

## Try it

```sh
go test ./internal/domain -v -run TestJSONKeepsNumberTypes
go test ./internal/domain -run 'TestParseJSONRejects/too_deep' -v
go test ./internal/adapters/memory -v -run TestStoreContract | head -30
go test ./internal/adapters/sqlite -run TestQueriesAgreeWithReference -v
go test -race ./internal/app -run TestConcurrentCommits -v
go test ./internal/rules -run '^$' -fuzz FuzzParse -fuzztime 5s
go test ./internal/adapters/sqlite -run '^$' -bench BenchmarkGet -benchmem
make cover
```

Then break something on purpose and watch the right test catch it. In
[`internal/adapters/sqlite/keyenc.go`](../../internal/adapters/sqlite/keyenc.go),
find `sortableInt` and remove the `^ (1 << 63)` so that it returns
`uint64(i)` unchanged. Run:

```sh
go test ./internal/adapters/sqlite -run 'TestKeyEncodingPreservesOrder|TestQueriesAgreeWithReference'
```

Both fail. The first prints two timestamps, one before 1970 and one after,
whose keys compare the wrong way round: the function also encodes
timestamps, as microseconds since 1970, and without the flipped sign bit a
negative number sorts above every positive one. The second shows a query
whose results come back in a different order from the reference. Read both
messages, and undo the change.

## Exercise: two generated tests for the domain

**Part 1: a property of With.** In `internal/domain`, add a test (a new file
`property_test.go` in package `domain_test` is a good place) that checks,
for many random maps, field paths and values:

- after `changed := m.With(fp, v)`, `changed.Get(fp)` returns `v` and `true`;
- `m` itself is unchanged.

**Hints.**

- `domaintest.Map(rng, 2)` and `domaintest.Value(rng, 2)` generate inputs.
  Build a random field path from one to three names out of `"a"`, `"b"`,
  `"c"`, `"d"`; those are the names `domaintest.Map` uses, so paths will
  often run into existing fields, including fields that are not maps.
- To check that `m` is unchanged you need its state from before the call.
  `m` is shared, not copied, by assignment. One way is to marshal it to JSON
  before and after and compare the strings.
- Use a fixed seed, and put the inputs into the failure message.

**Part 2: a fuzz test for the JSON parser.** In the same package add
`FuzzParseJSON`. The property: if `domain.ParseJSON(input)` succeeds, then
marshalling the result with `json.Marshal` and parsing that again gives a
value that is `domain.Equal` to the first.

**Hints.**

- The fuzz function receives `[]byte` or `string`; both are supported
  argument types.
- Add seeds with `f.Add` that cover every value type, including
  `{"$timestamp": "..."}` and a large integer.
- Return early when the first parse fails; invalid input is not a failure.

**How you know it works.** `go test ./internal/domain` passes, and
`go test ./internal/domain -run '^$' -fuzz FuzzParseJSON -fuzztime 30s`
runs without reporting a failure. To convince yourself the property test
can fail, apply the `out := m` change from chapter 01's "Try it" and run it.

## Further reading

- [Package testing](https://pkg.go.dev/testing)
- [Go Wiki: TableDrivenTests](https://go.dev/wiki/TableDrivenTests)
- [Using Subtests and Sub-benchmarks](https://go.dev/blog/subtests)
- [Go Fuzzing](https://go.dev/doc/security/fuzz/)
- [Data Race Detector](https://go.dev/doc/articles/race_detector)
- [The cover story](https://go.dev/blog/cover): how coverage works.
- [Package net/http/httptest](https://pkg.go.dev/net/http/httptest)

**Next:** [04 · Interfaces and ports](04-interfaces-and-ports.md)
