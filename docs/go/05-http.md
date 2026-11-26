# 05 · HTTP

## What you will learn

- The `http.Handler` interface, on which all of `net/http` is built
- Routing with the standard library's `ServeMux`: methods, wildcards,
  `r.PathValue`
- Reading and writing JSON with `encoding/json`: struct tags,
  `json.RawMessage`, custom marshalling, strict decoding
- `context.Context`, and carrying request-scoped values in it safely
- Middleware: functions that wrap handlers
- Limiting request size, and server timeouts
- Streaming a response with `http.ResponseController`
- Testing handlers with `httptest`

## The feature

Clients talk to candledb over HTTP with JSON bodies: `GET` and `PUT` on
`/v1/documents/...`, `POST /v1/query`, `POST /v1/commit`, the account
endpoints, and one long-lived stream for realtime updates. The tour covers
the API from the user's side in
[Reading and writing](../tour/03-reading-and-writing.md) and lists every
endpoint in [HTTP API](../tour/09-http-api.md).

All of it is implemented in one package, `internal/adapters/httpapi`, with no
web framework and no router library: only the standard library. The package
is an adapter in the sense of chapter 04. Its handlers do three things:
translate the request into domain types, call one method of the `app`
package, and translate the result back. If you look for business logic in
this package you should find none.

## Where the code is

1. [`server.go`](../../internal/adapters/httpapi/server.go): the `Server`
   type, its options and the routing table.
2. [`documents.go`](../../internal/adapters/httpapi/documents.go): the
   handlers for documents, queries, commits and accounts.
3. [`wire.go`](../../internal/adapters/httpapi/wire.go): the JSON shapes and
   their conversion to and from domain types.
4. [`errors.go`](../../internal/adapters/httpapi/errors.go): errors to
   status codes (chapter 02).
5. [`middleware.go`](../../internal/adapters/httpapi/middleware.go):
   logging, CORS, authentication, rate limiting, panic recovery.
6. [`listen.go`](../../internal/adapters/httpapi/listen.go): the realtime
   stream.
7. [`internal/domain/valuejson.go`](../../internal/domain/valuejson.go): how
   values become JSON and back.

## Handlers

Everything in `net/http` rests on one interface:

```go
type Handler interface {
	ServeHTTP(ResponseWriter, *Request)
}
```

A handler receives the request and writes the response through the
`ResponseWriter`. It returns nothing; whatever it wrote is the answer.

Most handlers are plain functions. The type `http.HandlerFunc` is a function
type with a `ServeHTTP` method that calls the function, so any function with
the signature `func(http.ResponseWriter, *http.Request)` can be used as a
`Handler`. (A function type with a method: named types can have methods
whatever they are built on, as chapter 01 said.)

Here is a complete handler:

```go
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	path, err := domain.ParseDocPath(r.PathValue("path"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	doc, err := s.docs.Get(r.Context(), principal(r.Context()), path)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toDocumentJSON(doc))
}
```

It is a method on `*Server`, so it can reach the use cases through `s.docs`.
Read it as the three steps: parse the path into a `domain.DocPath`; call
`s.docs.Get`; convert the document with `toDocumentJSON` and write it. Both
failure cases go through `s.writeError` from chapter 02, followed by
`return`.

That `return` is the classic HTTP mistake when it is forgotten. Writing an
error response does not end the handler; without the `return` the code would
carry on and write a second response on top of the first.

`net/http` runs every request in its own goroutine. Handlers therefore run
concurrently, and anything they share must be safe for that, which is why
the memory store of chapter 04 has a lock. Chapter 09 explains goroutines.

## Routing

A `ServeMux` ("multiplexer") maps request patterns to handlers. Since Go
1.22 a pattern can include the HTTP method and wildcards, which removed the
main reason to use a third-party router. From `routes` in
[`server.go`](../../internal/adapters/httpapi/server.go):

```go
mux := http.NewServeMux()

mux.HandleFunc("GET /healthz", s.handleHealth)
mux.HandleFunc("GET /readyz", s.handleReady)
mux.HandleFunc("GET /metrics", s.handleMetrics)

limited := s.rateLimit(s.authLimiter)
mux.Handle("POST /v1/auth/signup", limited(http.HandlerFunc(s.handleSignUp)))
mux.Handle("POST /v1/auth/signin", limited(http.HandlerFunc(s.handleSignIn)))
mux.Handle("POST /v1/auth/refresh", limited(http.HandlerFunc(s.handleRefresh)))
mux.HandleFunc("POST /v1/auth/signout", s.handleSignOut)
mux.HandleFunc("GET /v1/auth/me", s.handleMe)

mux.HandleFunc("GET /v1/documents/{path...}", s.handleGet)
mux.HandleFunc("PUT /v1/documents/{path...}", s.handleSet)
mux.HandleFunc("PATCH /v1/documents/{path...}", s.handleUpdate)
mux.HandleFunc("DELETE /v1/documents/{path...}", s.handleDelete)
mux.HandleFunc("POST /v1/documents/{path...}", s.handleAdd)
```

- `"GET /healthz"` matches only `GET` requests for exactly that path. A
  request with another method gets `405 Method Not Allowed` automatically.
- `{path...}` is a wildcard that matches the rest of the URL, slashes
  included. A document path such as `users/ada/posts/p1` has slashes, so the
  document routes need it. A wildcard without the dots, as in `{session}`
  further down in the same function, matches a single segment.
- The handler reads the matched text with `r.PathValue("path")`, as
  `handleGet` does above.
- `s.handleGet` without parentheses is a *method value*: the method bound to
  this particular `s`, as a function that can be passed around.
- `mux.Handle` takes a `Handler`; `mux.HandleFunc` takes a function. The
  three account routes use `Handle` because they are wrapped in a rate
  limiter first, which is the topic of the middleware section.

Five routes share the pattern `/v1/documents/{path...}` and differ only in
method. The same path means a document for `GET`, `PUT`, `PATCH` and
`DELETE`, and a collection for `POST`; each handler validates which one it
got.

## JSON out: struct tags

`encoding/json` converts between Go values and JSON using the shape of your
types. Each JSON shape of the API has a struct. From
[`wire.go`](../../internal/adapters/httpapi/wire.go):

```go
type documentJSON struct {
	Path       string     `json:"path"`
	ID         string     `json:"id"`
	Data       domain.Map `json:"data"`
	CreateTime string     `json:"createTime"`
	UpdateTime string     `json:"updateTime"`
	Version    int64      `json:"version"`
}
```

The text in backquotes after each field is a **struct tag**: metadata that
libraries can read. `json:"createTime"` tells `encoding/json` to use that
name in JSON instead of the Go field name. Elsewhere in the file you will
see `json:"path,omitempty"`, which leaves the field out when it holds its
zero value.

Only exported fields are encoded. A lower-case field is silently skipped,
which is a mistake everyone makes once: the JSON comes out as `{}` and
nothing complains.

Notice that this struct is **not** `domain.Document`. The domain type has no
JSON tags and knows nothing about the wire format; `toDocumentJSON` converts.
Keeping a separate type per boundary means the API's JSON can stay stable
while the internal type changes, and the reverse.

Writing a response is one helper, used by every handler:

```go
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// The status line is already sent, so an encoding error here (in
	// practice: the client went away) has nowhere to be reported.
	_ = json.NewEncoder(w).Encode(body)
}
```

The order of the three steps is fixed by HTTP: headers first, then the
status line with `WriteHeader`, then the body. Once `WriteHeader` has been
called, changes to the headers have no effect. `json.NewEncoder(w).Encode`
writes straight to the response without building the whole text in memory
first. `_ =` discards the error explicitly, and the comment says why: that
is the accepted way to ignore an error on purpose.

## JSON out: custom marshalling

A type can take over its own encoding by implementing the `json.Marshaler`
interface, a single method `MarshalJSON() ([]byte, error)`. The domain value
types do, because the defaults are wrong for them. From
[`internal/domain/valuejson.go`](../../internal/domain/valuejson.go):

```go
func (d Double) MarshalJSON() ([]byte, error) {
	f := float64(d)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, fmt.Errorf("%w: number is not finite", ErrInvalidArgument)
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	// Keep a trace of "this was a double" so 1.0 does not come back as the
	// integer 1 when the document is read again.
	if !strings.ContainsAny(s, ".e") {
		s += ".0"
	}
	return []byte(s), nil
}
```

By default a `float64` holding 2 is written as `2`, and would be read back
as an integer. This method appends `.0` so the value stays a double across a
round trip, and refuses NaN and infinity, which JSON cannot represent.

```go
func (a Array) MarshalJSON() ([]byte, error) {
	if a == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]Value(a))
}
```

A nil slice normally encodes as `null`. A document field that is an empty
list should be `[]`, so `Array` handles the nil case itself. The conversion
`[]Value(a)` in the last line matters: calling `json.Marshal(a)` directly
would invoke this same method again, forever. Converting to the underlying
type, which has no `MarshalJSON` method, gets the default behaviour for the
container while each element still uses its own method.

`encoding/json` finds these methods through nested values, so a `Map` inside
a `documentJSON` inside a slice is encoded correctly without any extra code.

## JSON in: strict decoding

Reading a request body is also one helper:

```go
// decodeBody reads the request body as one JSON value into dst. Unknown
// fields are rejected: a misspelt field name becomes an error message
// instead of a silently ignored setting.
func decodeBody(r *http.Request, dst any) error {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return err // includes *http.MaxBytesError when the body is too large
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: malformed request body: %v", domain.ErrInvalidArgument, err)
	}
	if dec.More() {
		return fmt.Errorf("%w: unexpected data after request body", domain.ErrInvalidArgument)
	}
	return nil
}
```

`dst any` is a parameter of the empty interface type: `any` is satisfied by
every type. Callers pass a pointer to the struct they want filled, such as
`&req`.

Two settings make the decoding strict. `DisallowUnknownFields` turns a
misspelt field (`"limt": 5`) into an error; the default is to ignore it, and
the client would get an unlimited query with no hint why. `dec.More()`
rejects a body with anything after the first JSON value.

The errors are wrapped as `ErrInvalidArgument`, so they become a 400. The
one exception is the read error, which is returned as it is: when the body
exceeds the size limit it is an `*http.MaxBytesError`, and `classify` maps
that to 413.

### json.RawMessage: decoding in two steps

Some parts of a request cannot be described by a fixed struct. The value in
a query filter may be any JSON value:

```go
type filterJSON struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	// RawMessage postpones decoding, so the value can be parsed by the
	// domain's own JSON reader, which keeps integers exact.
	Value json.RawMessage `json:"value"`
}
```

A `json.RawMessage` field receives the raw bytes of that part of the JSON,
undecoded. The struct decoder handles the fixed part (`field`, `op`), and
`toDomain` then passes the raw bytes to the domain's own parser,
`domain.ParseJSON`. The same technique carries the `data` of a write.

### UseNumber: keeping integers exact

Why does the domain need its own parser? Because of how `encoding/json`
treats numbers when it has no struct to guide it. Decoding into `any`, it
turns every JSON number into a `float64`, and a `float64` cannot represent
every integer above 2^53: `9007199254740993` would come back as
`9007199254740992`. For a database that is silent data corruption.

```go
// DecodeJSONTree reads exactly one JSON value from r into the generic shapes
// of encoding/json (map[string]any, []any, json.Number, string, bool, nil).
func DecodeJSONTree(r io.Reader) (any, error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("%w: malformed JSON: %v", ErrInvalidArgument, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: unexpected data after JSON value", ErrInvalidArgument)
	}
	return tree, nil
}
```

`dec.UseNumber()` makes the decoder keep each number as a `json.Number`,
which is a string holding the digits as they were written. `parseNumber` in
the same file then decides: digits only and within range become an `Int`,
anything else a `Double`. The integration test `TestTransformsAndValueTypes`
checks the result on the raw response text for exactly this number.

## context.Context

Every request carries a **context**, obtained with `r.Context()`. A
`context.Context` does two jobs.

The first is **cancellation**. When the client disconnects, the request's
context is cancelled, and anything that received that context can stop
working. This is why nearly every function in the `app` package and every
store method takes `ctx context.Context` as its first parameter and passes
it down: `s.docs.Get(r.Context(), ...)` hands it to the use case, which
hands it to the store, which hands it to `database/sql`, which aborts the
query if the client has gone. The convention is strict: the context is the
first parameter, it is named `ctx`, and it is passed explicitly, never
stored in a struct.

A context can also carry a deadline. The readiness check gives the store two
seconds to answer:

```go
// handleReady reports whether the server can reach its storage.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
```

`context.WithTimeout` returns a derived context and a `cancel` function.
Always call `cancel`, normally with `defer`, to release the timer; `go vet`
reports it if you forget.

The second job is carrying **request-scoped values**. The authentication
middleware works out who is calling, and the handlers need to know. The
value travels in the context:

```go
// contextKey is a private type for context keys. Because no other package
// can create a value of this type, no other package can collide with, read
// or overwrite what is stored under it.
type contextKey int

const (
	keyPrincipal contextKey = iota
	keyRequestID
)

func principal(ctx context.Context) app.Principal {
	p, _ := ctx.Value(keyPrincipal).(app.Principal)
	return p
}
```

`context.WithValue(ctx, key, value)` returns a new context that holds the
pair, and `ctx.Value(key)` looks it up. The key here is of type
`contextKey`, an unexported type. Since no other package can create a value
of that type, no other package can read or overwrite what this package
stored, even by accident. Using a plain string as the key is the common
mistake; two packages that both choose `"user"` then collide.

`ctx.Value` returns `any`, so `principal` uses a two-result type assertion.
If nothing was stored, the assertion fails quietly and the function returns
the zero `app.Principal`, which is the anonymous caller: a sensible default
by construction.

Keep context values for data that belongs to the request and crosses API
boundaries (who is calling, the request ID). Do not use them to pass
ordinary parameters; those belong in function signatures, where the compiler
can see them.

## Middleware

Logging, authentication, CORS headers and panic recovery apply to every
request. Writing them into every handler would be repetitive and easy to
forget. Instead, each is a **middleware**: a function that takes a handler
and returns a new handler that does something before or after calling the
original.

```go
// middleware is a function that wraps a handler to add behaviour around it.
// This is the decorator pattern applied to http.Handler: each middleware
// returns something with the same interface as what it was given.
type middleware func(http.Handler) http.Handler

// chain applies middleware so that the first one listed is closest to the
// handler and the last one is outermost.
func chain(h http.Handler, mws ...middleware) http.Handler {
	for _, mw := range mws {
		h = mw(h)
	}
	return h
}
```

The simplest example wraps the request body in a size limit:

```go
// limitBody caps the size of request bodies. Reading past the cap fails
// with *http.MaxBytesError, which classify maps to 413.
func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}
```

Read it from the inside out. The inner `func(w, r)` is the new handler's
body: it replaces `r.Body` and then calls `next.ServeHTTP(w, r)`, the
handler it wraps. `http.HandlerFunc(...)` converts that function into a
`Handler`. The outer function returns it. `next` is captured by the closure.

`http.MaxBytesReader` returns a reader that fails once more than
`s.maxBodyBytes` bytes have been read. Without a limit, a client could post
a body of any size and exhaust the server's memory.

A middleware can also stop the request. The authentication middleware does:

```go
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var bearer string
		if h := r.Header.Get("Authorization"); h != "" {
			scheme, token, ok := strings.Cut(h, " ")
			if !ok || !strings.EqualFold(scheme, "Bearer") {
				s.writeError(w, r, fmt.Errorf("%w: Authorization header must be \"Bearer <token>\"", domain.ErrUnauthenticated))
				return
			}
			bearer = strings.TrimSpace(token)
		}
		p, err := s.auth.Authenticate(bearer)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), keyPrincipal, p)))
	})
}
```

If the `Authorization` header is malformed or the token is invalid, it
writes an error and returns **without calling `next`**; the handler never
runs. Otherwise it stores the principal in the context and calls `next` with
a request that carries the new context. `r.WithContext` returns a copy of
the request, since requests are not modified in place.
`strings.Cut(h, " ")` splits a string at the first separator and reports
whether it was found.

### Order

The middlewares are stacked at the end of `routes`:

```go
// Middleware wraps the mux from the inside out: the last one listed is
// the first to see a request.
return chain(mux,
	s.authenticate,
	s.limitBody,
	s.cors,
	s.observe,
	s.recoverPanics,
)
```

`chain` wraps in list order, so the first one listed ends up closest to the
mux and the last one is outermost. A request passes through them from the
bottom of the list to the top: `recoverPanics`, `observe`, `cors`,
`limitBody`, `authenticate`, then the mux and the handler.

The order is a set of decisions:

- `recoverPanics` is outermost so that it also catches a panic in another
  middleware.
- `observe` (request ID, logging, metrics) is outside everything else, so
  rejected requests are logged and counted too.
- `cors` is outside `authenticate`. A 401 produced by `authenticate` then
  still carries the CORS headers, and without those a browser hides the
  error from the page's JavaScript. `TestCORS` in the integration tests
  checks an error response for this reason.

The rate limiter is applied differently: only to three routes, by wrapping
their handlers individually (`limited(http.HandlerFunc(s.handleSignUp))` in
`routes`). `s.rateLimit(l)` is a function that returns a middleware, which
is how a middleware gets a parameter.

`statusRecorder`, which `observe` uses to learn the status code, was covered
in chapter 04 under embedding.

## Server timeouts

A handler is not the only place to defend the server. The `http.Server`
itself is configured where the program is assembled. From
[`cmd/candledb/serve.go`](../../cmd/candledb/serve.go):

```go
server := &http.Server{
	Handler: api.Handler(),
	// Timeouts protect against clients that open a connection and then
	// send or read very slowly, tying up resources. The listen stream
	// extends its own write deadline message by message.
	ReadHeaderTimeout: 10 * time.Second,
	ReadTimeout:       60 * time.Second,
	WriteTimeout:      60 * time.Second,
	IdleTimeout:       120 * time.Second,
	ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
}
```

The zero value of each timeout means "no limit", and that default is unsafe
on the open internet: a client that sends its headers one byte a minute
would hold a connection, and a goroutine, for as long as it liked. A
production server sets all four. `http.ListenAndServe(addr, handler)`, which
most tutorials use, creates a server with no timeouts; this is the main
reason real programs construct `http.Server` themselves.

## Streaming: the listen endpoint at the HTTP level

`GET /v1/listen` is different from every other route: its response never
ends. It uses Server-Sent Events, a simple format in which the server keeps
the response open and writes messages of the form `data: <json>` followed by
a blank line. [Realtime](../tour/05-realtime.md) describes it from the
client's side.

Two things stand in the way of streaming from a normal handler. The response
is buffered, so a small write may sit in a buffer instead of reaching the
client. And the server's `WriteTimeout`, set to 60 seconds above, would cut
the stream off after a minute. `http.ResponseController` solves both. From
`handleListen` in [`listen.go`](../../internal/adapters/httpapi/listen.go):

```go
// ResponseController reaches features of the underlying connection
// (flushing, deadlines) through any wrapping middleware.
rc := http.NewResponseController(w)
send := func(ev eventJSON) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	// A per-message deadline replaces the server-wide write timeout,
	// which would otherwise cut the stream off after a fixed time.
	if err := rc.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return err
	}
	return rc.Flush()
}
```

`send` is a closure stored in a variable, used for every message of the
stream. For each message it sets a fresh write deadline 30 seconds ahead,
writes, and calls `rc.Flush()` to push the bytes to the client immediately.
The stream as a whole has no time limit, but any single write that takes
longer than 30 seconds fails, which is how the server notices a client that
has silently gone away.

`fmt.Fprintf(w, ...)` works because `http.ResponseWriter` has a `Write`
method and therefore satisfies `io.Writer`, the standard library's interface
for "something bytes can be written to". Files, network connections, buffers
and HTTP responses all satisfy it, so the same formatting functions work on
all of them.

The response starts like this:

```go
w.Header().Set("Content-Type", "text/event-stream")
w.Header().Set("Cache-Control", "no-store")
w.Header().Set("X-Accel-Buffering", "no") // ask proxies such as nginx not to buffer
w.WriteHeader(http.StatusOK)
if err := send(eventJSON{Type: "ready", Session: sess.ID()}); err != nil {
	return
}
```

After that the handler enters a loop that waits for events and sends them
until the client disconnects (`r.Context().Done()`). That loop is built from
channels and `select`, which belong to chapter 09; the HTTP part ends here.

There is one more connection to chapter 04. The `w` this handler receives is
not the server's own writer but the `statusRecorder` that `observe` wrapped
around it, which has no `Flush` method. `ResponseController` finds the real
writer by calling the wrapper's `Unwrap` method. A wrapper that forgets
`Unwrap` breaks streaming for every handler behind it.

## Testing HTTP code

The package `net/http/httptest` provides the two tools you need.

`httptest.NewRecorder()` is a `ResponseWriter` that records what a handler
writes, and `httptest.NewRequest` builds a request without a network. With
them, a handler or middleware is tested as an ordinary function call. From
[`internal_test.go`](../../internal/adapters/httpapi/internal_test.go):

```go
func TestPanicBecomesInternalError(t *testing.T) {
	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := s.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Code != 500 {
		t.Fatalf("status = %d", rec.Code)
	}
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "internal" {
		t.Errorf("body = %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Error("the panic value leaked to the client")
	}
}
```

The test wraps a handler that panics in the real `recoverPanics` middleware,
calls `ServeHTTP` directly, and inspects the recorded status and body.

`httptest.NewServer(handler)` starts a real HTTP server on a free local
port and gives you its URL. The integration tests of chapter 03 use it to
exercise the complete stack, middleware and streaming included.

## Try it

Start a server and talk to it with `curl`. `-i` shows the status line and
headers.

```sh
./bin/candledb serve -addr 127.0.0.1:18112 -data-dir /tmp/candledb-ch05 -dev-open-rules &

curl -i -X PUT http://127.0.0.1:18112/v1/documents/users/ada -d '{"name": "Ada", "big": 9007199254740993, "ratio": 2.0}'
curl -i http://127.0.0.1:18112/v1/documents/users/ada
curl -i -X POST http://127.0.0.1:18112/v1/query -d '{"collection": "users", "limt": 5}'
curl -i -X DELETE http://127.0.0.1:18112/v1/query
curl -i -X OPTIONS http://127.0.0.1:18112/v1/documents/users/ada \
     -H 'Origin: https://example.com' -H 'Access-Control-Request-Method: PUT'
curl -i -H 'Authorization: Bearer nonsense' http://127.0.0.1:18112/v1/documents/users/ada
```

What to look for, in order: the `X-Request-Id` header that `observe` adds;
`"big":9007199254740993` and `"ratio":2.0` coming back exact; a 400 that
names the unknown field `limt`; a 405 produced by the mux; a 204 with
`Access-Control-Allow-*` headers produced by `cors` before any handler ran;
and a 401 produced by `authenticate`.

Now the stream. `-N` turns off curl's own buffering:

```sh
curl -N http://127.0.0.1:18112/v1/listen
```

It prints a `ready` message with a session ID and stays open. After 20
seconds a line `: ping` appears, the keep-alive. Stop it with Ctrl-C, then
stop the server with `kill %1`.

The tests for this chapter:

```sh
go test ./internal/adapters/httpapi -v
go test ./test/integration -run 'TestCORS|TestRequestID|TestInvalidRequests' -v
```

## Exercise: put the "maximum" transform on the wire

In chapter 01 you added `domain.Maximum`. It exists in the domain but no
client can reach it. Make `{"$max": n}` in the data of a write request it,
the same way `{"$increment": n}` requests an increment.

**What to do.**

1. In [`wire.go`](../../internal/adapters/httpapi/wire.go), add a constant
   next to `keyIncrement` and a case in `parseTransform`.
2. Reject an operand that is not a number with a clear `ErrInvalidArgument`
   message, as `$increment` does.
3. Add cases to `TestWriteTranslation` and `TestWriteTranslationRejects` in
   `internal_test.go`.
4. Add an integration test in `test/integration/documents_test.go`: `PUT` a
   document with a score, `PATCH` it with `{"score": {"$max": ...}}` once
   with a lower and once with a higher operand, and `GET` it after each.

**Hints.**

- Read `parseTransform` fully before you change it. It is called for every
  field of every write, and its last lines decide what happens to an
  unknown key that starts with `$`.
- Nothing in `app` or in the stores needs to change. Ask yourself why: what
  does the `app` package know about a write's transforms?
- Check how the transform behaves inside `set` with and without `merge`, and
  inside `update`. You get those for free from `parseWriteData`, but a test
  should say so.

**How you know it works.** `go test ./internal/adapters/httpapi
./test/integration` passes, and this prints 15 and then 15 again:

```sh
curl -X PUT   http://127.0.0.1:18112/v1/documents/games/g1 -d '{"score": 10}'
curl -X PATCH http://127.0.0.1:18112/v1/documents/games/g1 -d '{"score": {"$max": 15}}'
curl          http://127.0.0.1:18112/v1/documents/games/g1
curl -X PATCH http://127.0.0.1:18112/v1/documents/games/g1 -d '{"score": {"$max": 12}}'
curl          http://127.0.0.1:18112/v1/documents/games/g1
```

Remember to rebuild the server (`make build`) before trying it by hand.

For a complete feature the TypeScript client in `sdk/js` would also get a
`maximum(n)` helper next to `increment(n)`. That is outside this track, but
`sdk/js/src/values.ts` (the helper) and `sdk/js/src/mutations.ts` (its local
estimate for optimistic writes) show where it would go.

## Further reading

- [Package net/http](https://pkg.go.dev/net/http): the documentation of
  `ServeMux` describes the pattern syntax in full.
- [Routing Enhancements for Go 1.22](https://go.dev/blog/routing-enhancements)
- [JSON and Go](https://go.dev/blog/json)
- [Package encoding/json](https://pkg.go.dev/encoding/json)
- [Go Concurrency Patterns: Context](https://go.dev/blog/context)
- [Package context](https://pkg.go.dev/context)
- [Package net/http/httptest](https://pkg.go.dev/net/http/httptest)

**Next:** [06 · SQL and encoding](06-sql-and-encoding.md)
