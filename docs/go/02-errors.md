# 02 · Errors

## What you will learn

- That an error in Go is an ordinary value, returned like any other
- The `if err != nil` idiom and why Go uses it instead of exceptions
- Sentinel errors, and wrapping an error with context using `%w`
- `errors.Is` and `errors.As`
- How to define your own error type
- `defer`, for cleanup that must happen on every path
- When `panic` is appropriate, and the one place this program recovers
- How one error travels from the database to an HTTP status code

## The feature

Every request to candledb can fail in ways the client should be told about:
the document does not exist, the rules forbid the write, a precondition did
not hold, the JSON was malformed. Each of these has a code and an HTTP
status, listed in the tour's [HTTP API](../tour/09-http-api.md) page.

Other failures are the server's own: the disk is full, the database file is
corrupt. Those must be logged and must **not** be described to the client.

The code that detects a failure (deep in the storage adapter, say) is far
from the code that answers the client. This chapter is about how the failure
gets from one to the other with its meaning intact.

## Where the code is

1. [`internal/domain/errors.go`](../../internal/domain/errors.go): the eight
   categories of error, in one short file.
2. [`internal/adapters/memory/store.go`](../../internal/adapters/memory/store.go)
   and [`internal/adapters/sqlite/store.go`](../../internal/adapters/sqlite/store.go):
   `Get` in each, where "not found" is created.
3. [`internal/app/service.go`](../../internal/app/service.go) (`load`) and
   [`internal/app/read.go`](../../internal/app/read.go) (`Get`): where the
   use case interprets it.
4. [`internal/adapters/httpapi/errors.go`](../../internal/adapters/httpapi/errors.go):
   where categories become status codes.
5. [`internal/rules/lexer.go`](../../internal/rules/lexer.go): `SyntaxError`,
   an error type with fields.
6. [`internal/adapters/sqlite/db.go`](../../internal/adapters/sqlite/db.go):
   `inTx`, for `errors.Join` and transactions.

## Errors are values

Go has no exceptions for ordinary failures. A function that can fail returns
an extra result of type `error`, and the caller checks it.

`error` is a built-in interface with one method:

```go
type error interface {
	Error() string
}
```

Any value with an `Error() string` method is an error. `nil` means "no
error". You saw the shape in chapter 01: `ParseDocPath` returns
`(DocPath, error)`.

Here is a function that calls something that can fail. From
[`internal/app/read.go`](../../internal/app/read.go):

```go
func (s *Service) Get(ctx context.Context, p Principal, path domain.DocPath) (domain.Document, error) {
	doc, err := s.load(ctx, path)
	if err != nil {
		return domain.Document{}, err
	}
	if !s.allowed(ctx, p, rules.MethodGet, path, doc, nil) {
		return domain.Document{}, fmt.Errorf("%w: get %s", domain.ErrPermissionDenied, path)
	}
	if doc == nil {
		return domain.Document{}, fmt.Errorf("%w: document %s", domain.ErrNotFound, path)
	}
	return *doc, nil
}
```

The first three lines of the body are the idiom you will type thousands of
times: call, check `err != nil`, return early. On the error path the function
returns the zero value of its normal result (`domain.Document{}`) and the
error.

This is more typing than `try`/`catch`, and it is deliberate. Every place
where the function can exit is visible as a `return`. There is no invisible
control flow: when you read `doc, err := s.load(ctx, path)` you know that
the next line runs whether or not it failed, and you can see what the code
does about it. In a server that must keep its locks, transactions and
listeners consistent, knowing every exit point matters.

The usual shape, which this whole codebase follows: handle the error case
first and return, and keep the successful path at the left margin, not
nested inside `else` blocks.

The typical newcomer mistake is to ignore an error by assigning it to `_`,
or to check it and carry on anyway with a result that is not valid.

## Sentinel errors

A caller often needs to know *which kind* of failure happened. Comparing
message strings would be fragile. Instead, a package declares error values
that stand for a category, and callers compare against them. These are
called **sentinel errors**. CandleDB has eight, and this is the entire file:

```go
// Sentinel errors. Code that fails wraps one of these with extra context
// (fmt.Errorf("%w: ...", ErrNotFound, ...)) and callers test for the category
// with errors.Is. The HTTP adapter maps each one to a status code.
var (
	ErrInvalidArgument    = errors.New("invalid argument")
	ErrNotFound           = errors.New("not found")
	ErrAlreadyExists      = errors.New("already exists")
	ErrFailedPrecondition = errors.New("failed precondition")
	ErrPermissionDenied   = errors.New("permission denied")
	ErrUnauthenticated    = errors.New("unauthenticated")
	ErrResourceExhausted  = errors.New("resource exhausted")
	ErrUnavailable        = errors.New("unavailable")
)
```

`errors.New` creates an error with a fixed message. Each call creates a
distinct value, so `ErrNotFound` is equal only to itself. By convention the
names of sentinel errors start with `Err`.

These eight values are the vocabulary for failure in the whole program. The
domain defines them, the adapters produce them, the use cases interpret
them, and the HTTP adapter translates them.

## Wrapping: adding context with %w

Returning the bare sentinel would lose information: *which* document was not
found? The in-memory store adds it. From
[`internal/adapters/memory/store.go`](../../internal/adapters/memory/store.go):

```go
func (s *Store) Get(_ context.Context, path domain.DocPath) (domain.Document, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	doc, ok := s.collections[path.Parent()][path.ID()]
	if !ok {
		return domain.Document{}, fmt.Errorf("%w: document %s", domain.ErrNotFound, path)
	}
	return doc, nil
}
```

`fmt.Errorf` builds an error from a format string. The verb `%w` (for
"wrap") inserts the message of `domain.ErrNotFound` **and keeps a link to
it** inside the new error. The result prints as
`not found: document users/nobody`, and it still "is" `ErrNotFound` in the
sense the next section explains.

Compare `%w` with `%v`. Both produce the same text. `%v` only formats: the
link to the original error is gone, and a caller can no longer recognise the
category. Using `%v` where `%w` was meant is the most common mistake in Go
error handling, and nothing warns you; the status code just quietly becomes
500. The rule here: `%w` when the caller should be able to recognise the
cause, `%v` when the cause is an internal detail that should not become part
of the function's contract.

Wrapping can be layered. `Commit` adds which write of a batch failed. From
[`internal/app/write.go`](../../internal/app/write.go):

```go
updated, err := s.stage(ctx, p, w, existing, now, version)
if err != nil {
	if len(writes) == 1 {
		return CommitResult{}, err
	}
	// In a batch, say which write failed.
	return CommitResult{}, fmt.Errorf("write %d (%s %s): %w", i, w.Op, w.Path, err)
}
```

If the third write of a batch is an update of a missing document, the client sees
`write 2 (update accounts/missing): not found: document accounts/missing`,
and the error still counts as `ErrNotFound`, two layers down.

## errors.Is

`errors.Is(err, target)` reports whether `err`, or any error it wraps at any
depth, is `target`. It is how you test for a sentinel. Never use `==` for
this: `==` only matches the outermost error and breaks as soon as someone
adds context.

The use cases treat a missing document as an ordinary situation, not a
failure, so they convert the error into a nil pointer in one place. From
[`internal/app/service.go`](../../internal/app/service.go):

```go
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
```

Three outcomes, three returns: not found gives `(nil, nil)`, any other error
is passed up unchanged, and success gives a pointer to the document.

The SQLite adapter has the same translation one level lower. The standard
library's `database/sql` package has its own sentinel, `sql.ErrNoRows`, and
the adapter turns it into ours. From
[`internal/adapters/sqlite/store.go`](../../internal/adapters/sqlite/store.go):

```go
func (db *DB) Get(ctx context.Context, path domain.DocPath) (domain.Document, error) {
	parent := path.Parent()
	row := db.read.QueryRowContext(ctx,
		"SELECT "+docColumns+" FROM docs WHERE parent = ? AND id = ?", string(parent), path.ID())
	doc, err := scanDoc(row, parent)
	if errors.Is(err, sql.ErrNoRows) {
		return doc, fmt.Errorf("%w: document %s", domain.ErrNotFound, path)
	}
	return doc, err
}
```

This is a boundary rule worth copying: **an adapter translates the errors of
its technology into the errors of the domain**. Nothing above the adapter
knows that `sql.ErrNoRows` exists, so the store can be replaced without
touching the callers. Note also that the new error wraps `ErrNotFound` and
not `sql.ErrNoRows`: the SQL error is deliberately dropped.

## Custom error types and errors.As

A sentinel carries no data. When an error has to carry structured
information, define a type. The rules parser reports the position of a
syntax error. From [`internal/rules/lexer.go`](../../internal/rules/lexer.go):

```go
// SyntaxError is returned by Parse when the source is not valid.
type SyntaxError struct {
	Pos Pos
	Msg string
}

func (e *SyntaxError) Error() string { return fmt.Sprintf("rules:%s: %s", e.Pos, e.Msg) }
```

`SyntaxError` is a struct with two fields. The `Error` method makes
`*SyntaxError` (the pointer type) satisfy the `error` interface.

To get at the fields, a caller uses `errors.As`, the counterpart of
`errors.Is` for types. From
[`internal/rules/rules_test.go`](../../internal/rules/rules_test.go):

```go
func TestSyntaxErrorPosition(t *testing.T) {
	_, err := rules.Parse("match /a/{b} {\n  allow read: if oops;\n}")
	var syntaxErr *rules.SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Fatalf("got %v", err)
	}
	if syntaxErr.Pos.Line != 2 || syntaxErr.Pos.Col != 18 {
		t.Errorf("position = %v, want 2:18", syntaxErr.Pos)
	}
}
```

You declare a variable of the type you are looking for and pass its address.
If `err` or anything it wraps is a `*rules.SyntaxError`, `errors.As` stores
it in the variable and returns true. Like `errors.Is`, it looks through
wrapping: `candledb check-rules` wraps the error with the file name, and the
position is still reachable.

Use `errors.Is` for "is it this particular error value?" and `errors.As` for
"is it an error of this type, and if so give it to me".

There is a trap with custom error types. A function that returns `error`
must return a literal `nil` on success, never a nil pointer of a concrete
type such as `var e *SyntaxError; return e`. An interface value that holds a
nil pointer is itself **not** nil, so the caller's `err != nil` is true. The
parser avoids the problem by having its helper `errorf` return `error` and
by writing `return nil, err` or `return rs, nil` explicitly.

## From error to HTTP status

The last step is the HTTP adapter. It has a table. From
[`internal/adapters/httpapi/errors.go`](../../internal/adapters/httpapi/errors.go):

```go
var errorStatus = []struct {
	err    error
	status int
	code   string
}{
	{domain.ErrInvalidArgument, http.StatusBadRequest, "invalid_argument"},
	{domain.ErrUnauthenticated, http.StatusUnauthorized, "unauthenticated"},
	{domain.ErrPermissionDenied, http.StatusForbidden, "permission_denied"},
	{domain.ErrNotFound, http.StatusNotFound, "not_found"},
	{domain.ErrAlreadyExists, http.StatusConflict, "already_exists"},
	{domain.ErrFailedPrecondition, http.StatusPreconditionFailed, "failed_precondition"},
	{domain.ErrResourceExhausted, http.StatusTooManyRequests, "resource_exhausted"},
	{domain.ErrUnavailable, http.StatusServiceUnavailable, "unavailable"},
	{app.ErrSessionClosed, http.StatusGone, "session_closed"},
}
```

`errorStatus` is a slice of *anonymous structs*: a struct type declared in
place, used once, with no name. It is a compact way to write a small table.

```go
func classify(err error) (status int, code string) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return http.StatusRequestEntityTooLarge, "request_too_large"
	}
	for _, e := range errorStatus {
		if errors.Is(err, e.err) {
			return e.status, e.code
		}
	}
	return http.StatusInternalServerError, "internal"
}
```

`classify` uses both tools. `errors.As` recognises `*http.MaxBytesError`, a
type from the standard library that a size-limited request body returns when
the client sends too much. `errors.Is` checks the sentinels. Anything
unrecognised is a 500.

```go
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, code := classify(err)
	message := err.Error()
	if status == http.StatusInternalServerError {
		s.logger.Error("request failed", "error", err, "method", r.Method, "path", r.URL.Path,
			"request_id", requestID(r.Context()))
		message = "internal error"
	}
	writeJSON(w, status, errorBody{errorDetail{Code: code, Message: message}})
}
```

This function draws the line described at the start of the chapter. For an
error the client caused, the message is sent as it is; that is why messages
are written for the API user ("document is at version 3, expected 2"). For
anything else the real error goes to the log with a request ID, and the
client gets the two words "internal error". A SQL error message can contain
file paths and query text, and none of that leaves the server. The test
`TestInternalErrorsAreNotLeaked` in
[`internal/adapters/httpapi/internal_test.go`](../../internal/adapters/httpapi/internal_test.go)
pins this down.

### The whole journey of one "not found"

A client sends `GET /v1/documents/users/nobody`.

1. `handleGet` in `httpapi/documents.go` parses the path and calls
   `s.docs.Get`.
2. `Service.Get` in `app/read.go` calls `s.load`, which calls the store.
3. `DB.Get` in `sqlite/store.go` runs the query. No row comes back:
   `sql.ErrNoRows`. It returns
   `fmt.Errorf("%w: document %s", domain.ErrNotFound, path)`.
4. Back in `load`, `errors.Is(err, domain.ErrNotFound)` is true, so `load`
   returns `(nil, nil)`.
5. `Service.Get` first asks the rules whether the caller may read this path.
   Only then does it look at `doc == nil` and return a **new** error that
   wraps `ErrNotFound`. The order is a security decision: a caller who may
   not read a path gets "permission denied" whether or not a document is
   there.
6. `handleGet` receives the error and calls `s.writeError`.
7. `classify` finds `ErrNotFound` in the table: status 404, code `not_found`.
8. The client receives
   `{"error":{"code":"not_found","message":"not found: document users/nobody"}}`.

No layer skipped the layer above it, and no layer needed to know how the one
below represents a missing row.

## errors.Join and transactions

Sometimes two things fail at once. A database transaction that fails must be
rolled back, and the rollback can fail too. Losing either error would hide
information. From
[`internal/adapters/sqlite/db.go`](../../internal/adapters/sqlite/db.go):

```go
// inTx runs fn inside a write transaction, committing if fn returns nil and
// rolling back otherwise. Wrapping the begin/commit/rollback dance in one
// helper means no caller can forget a rollback on an error path.
func (db *DB) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		// Join keeps both errors if the rollback fails too.
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}
```

`errors.Join` combines errors into one. It ignores nil arguments, so if the
rollback succeeds the result is just `err`. `errors.Is` and `errors.As` look
into every joined error.

`inTx` also shows a Go habit: where a sequence must always be completed
(begin, then commit or roll back), write it once in a helper that takes the
variable part as a function. Callers pass a closure and cannot forget the
rollback. `Apply` in `sqlite/store.go` is written that way.

## defer

`defer` schedules a function call to run when the surrounding function
returns, whichever `return` it leaves through, and also if it panics. It is
Go's tool for cleanup, used where other languages use `finally` or
destructors. From `serve` in
[`cmd/candledb/serve.go`](../../cmd/candledb/serve.go):

```go
db, err := sqlite.Open(ctx, filepath.Join(cfg.dataDir, "candledb.db"))
if err != nil {
	return err
}
// defer runs when serve returns, by any path. Deferred calls run in
// reverse order, so the database closes after everything that uses it.
defer db.Close()
```

`serve` has about ten `return` statements after this point. The
database is closed on every one of them, because the `defer` is written
once, right after the resource was acquired. That placement is the
convention: acquire, check the error, defer the release.

Three rules about `defer`:

- Deferred calls run in reverse order: the last one deferred runs first. So
  the database, opened first, is closed last, after everything that uses it.
- The arguments of a deferred call are evaluated when the `defer` statement
  runs, not when the call happens.
- `defer` belongs to the **function**, not to the block. A `defer` inside a
  loop does not run at the end of each iteration; the calls pile up until
  the function returns. Move the loop body into its own function if you need
  per-iteration cleanup.

The most frequent use in this codebase is releasing a lock: the line that
takes it is followed immediately by a `defer` that gives it back. You can see
the pair `s.mu.RLock()` and `defer s.mu.RUnlock()` in the excerpt from
`memory/store.go` above. Chapter 04 explains the locks themselves.

## panic and recover

A **panic** stops the normal flow, runs the deferred calls of each function
on the way up, and, if nothing intervenes, ends the program with a stack
trace. Go code does not use panics for expected failures. A missing
document, malformed input, a full disk: all of those are errors.

A panic is for a situation that can only arise from a bug in the program
itself. The domain package has a few. From `Compare` in
[`internal/domain/value.go`](../../internal/domain/value.go):

```go
	panic("domain: unknown value type")
}
```

Reaching that line means someone added a ninth value type and did not update
`Compare`. No caller could handle that sensibly, and returning an error would
force every caller of `Compare` to check for a condition that cannot occur
in a correct program.

Some panics come from the runtime: indexing past the end of a slice, calling
a method through a nil pointer, writing to a nil map.

`recover` stops a panic. It is only useful inside a deferred function. This
program calls it in exactly one place, at the outer edge of every HTTP
request. From
[`internal/adapters/httpapi/middleware.go`](../../internal/adapters/httpapi/middleware.go):

```go
// recoverPanics turns a panic in a handler into a 500 response. Without it
// net/http would only close the connection, and the client would see a
// network error with no hint of what happened.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil || v == http.ErrAbortHandler {
				return
			}
			s.logger.Error("panic in handler", "panic", v, "stack", string(debug.Stack()),
				"method", r.Method, "path", r.URL.Path)
			writeJSON(w, http.StatusInternalServerError,
				errorBody{errorDetail{Code: "internal", Message: "internal error"}})
		}()
		next.ServeHTTP(w, r)
	})
}
```

If a bug makes one handler panic, this logs the panic value with a stack
trace and answers 500, and the server keeps serving other requests. Without
it the client would see the connection drop with no explanation. The check
for `http.ErrAbortHandler` lets through the one panic value that `net/http`
itself uses on purpose to abort a response.

The mistake to avoid is using `panic` and `recover` as a substitute for
exceptions inside your own logic. One recover at the boundary, as a safety
net, is the accepted use.

## A different policy: errors that mean "no"

Not every error is reported. In the security rules evaluator, an expression
that fails to evaluate (it reads a field of a missing document, or compares
a number with a string) makes the condition false. From
[`internal/rules/eval.go`](../../internal/rules/eval.go):

```go
// holds evaluates a condition to a yes or no. Errors and non-boolean
// results are a no.
func (ev *evaluator) holds(cond expr, sc *scope) bool {
	if cond == nil {
		return true
	}
	v, err := ev.eval(cond, sc)
	if err != nil {
		return false
	}
	b, ok := v.(domain.Bool)
	return ok && bool(b)
}
```

The error is deliberately dropped, and the function says so in its comment.
In an access check the safe answer to "something went wrong" is "deny". This
is still explicit error handling: the `if err != nil` is there, and what it
does was chosen.

## Try it

Run a server with open rules on a spare port and look at real error
responses:

```sh
./bin/candledb serve -addr 127.0.0.1:18111 -data-dir /tmp/candledb-ch02 -dev-open-rules &

curl -i http://127.0.0.1:18111/v1/documents/users/nobody
curl -i -X PATCH http://127.0.0.1:18111/v1/documents/users/nobody -d '{"a": 1}'
curl -i -X PUT http://127.0.0.1:18111/v1/documents/users -d '{}'
curl -i -X PUT http://127.0.0.1:18111/v1/documents/users/ada -d '{"a": '

kill %1
```

The four requests answer 404 `not_found`, 404 `not_found`, 400
`invalid_argument` and 400 `invalid_argument`.
For each one, find the line of code that created the error and the line that
wrapped it.

Then run the tests that fix this behaviour:

```sh
go test ./internal/adapters/httpapi -run TestInternalErrorsAreNotLeaked -v
go test ./internal/rules -run TestParseErrors -v
go test ./test/integration -run TestInvalidRequests -v
```

## Exercise: a sentinel error of its own for oversized documents

**Goal.** A document larger than 1 MiB is currently refused with
`ErrInvalidArgument`, which becomes HTTP 400. Make it a distinct category
that becomes HTTP 413 (content too large) with the code `too_large`.

**What to do.**

1. Add `ErrTooLarge` to `internal/domain/errors.go`.
2. In `ValidateData` in `internal/domain/document.go`, wrap the new sentinel
   for the size check. Leave the depth check as it is.
3. Add a row to `errorStatus` in `internal/adapters/httpapi/errors.go`.
4. Update the tests that expect the old behaviour, and add an integration
   test in `test/integration/documents_test.go` that sends an oversized
   document and expects 413 with the new code.

**Hints.**

- Search for the existing tests first: `grep -rn "oversized" internal test`.
  One of them asserts `ErrInvalidArgument` for an oversized document; decide
  what it should assert now.
- The integration harness starts the server with a request body limit of
  10 MiB unless the test says otherwise, so a 1.5 MiB JSON string reaches
  `ValidateData`. Build the body with `strings.Repeat`.
- A request body over the body limit is a different error
  (`*http.MaxBytesError`, code `request_too_large`). Your test should not
  trip that one; check the `code` field, not only the status.
- Think about whether `ErrTooLarge` should also "be" an `ErrInvalidArgument`.
  It can: `fmt.Errorf` accepts more than one `%w`. If you do that, the order
  of the rows in `errorStatus` starts to matter. Read the comment above the
  table.

**How you know it works.** `go test ./internal/... ./test/...` passes, and
the `curl` equivalent of your integration test prints `413`.

## Further reading

- [Error handling and Go](https://go.dev/blog/error-handling-and-go)
- [Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors): the
  introduction of `%w`, `errors.Is` and `errors.As`.
- [Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover)
- [Package errors](https://pkg.go.dev/errors)
- [Effective Go: Errors](https://go.dev/doc/effective_go#errors)

**Next:** [03 · Testing](03-testing.md)
