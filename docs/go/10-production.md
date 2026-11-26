# 10. Running in production

The previous chapters were about what the server does. This one is about
the program around it: how it starts, how it is configured, how it stops,
how it is built and shipped, and how you find out what it is doing.

## What you will learn

- How to structure `package main` so that the program can be tested.
- Command-line flags with the `flag` package, and environment variables as fallbacks.
- Structured logging with `log/slog`.
- Handling signals: shutting down gracefully and reloading configuration.
- The timeouts an HTTP server needs.
- Build flags, static binaries, cross-compiling, and a minimal container image.
- Small security habits: file permissions, the right random source, constant-time comparison.
- Benchmarks and CPU profiles.
- What "production ready" means for this project.

## The feature

`candledb` is one binary with four commands: `serve`, `check-rules`,
`backup` and `version`. `serve` reads its configuration from flags and
environment variables, opens the database, and runs until it is told to
stop. Operating it is described in
[tour: operations](../tour/08-operations.md).

`cmd/candledb` is also the *composition root*: the one place that creates
the concrete adapters and hands them to the core.
[Architecture: hexagonal](../architecture/01-hexagonal.md) explains that
role; this chapter covers the Go around it.

## Where the code is

- [`cmd/candledb/main.go`](../../cmd/candledb/main.go): `main` and `run`, the command dispatcher.
- [`cmd/candledb/serve.go`](../../cmd/candledb/serve.go): configuration, wiring, the server's life cycle.
- [`cmd/candledb/tools.go`](../../cmd/candledb/tools.go): `check-rules` and `backup`.
- [`cmd/candledb/main_test.go`](../../cmd/candledb/main_test.go): tests that run the real commands.
- [`internal/adapters/httpapi/middleware.go`](../../internal/adapters/httpapi/middleware.go) and [`metrics.go`](../../internal/adapters/httpapi/metrics.go): request logging and `/metrics`.
- [`internal/adapters/instrumented/store.go`](../../internal/adapters/instrumented/store.go): counters around the store.
- [`Makefile`](../../Makefile), [`Dockerfile`](../../Dockerfile), [`.github/workflows/ci.yml`](../../.github/workflows/ci.yml): building, packaging, checking ([chapter 11](11-continuous-integration.md)).

## `main` does almost nothing

A Go program starts in the function `main` of the package `main`. That
function takes no arguments and returns nothing: it reads `os.Args`, and
ends the process with `os.Exit`. Both make it awkward to call from a test.

From `cmd/candledb/main.go`:

```go
func main() {
	// NotifyContext returns a context that is cancelled when the process
	// receives one of the signals: the idiomatic way to turn Ctrl-C and
	// "docker stop" into an orderly shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "candledb:", err)
		}
		os.Exit(1)
	}
}
```

So `main` is kept to what cannot be tested anyway, and everything else is
in `run`:

From `cmd/candledb/main.go`:

```go
func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return flag.ErrHelp
	}
	switch args[0] {
	case "serve":
		return serve(ctx, args[1:], stderr)
	case "check-rules":
		return checkRules(args[1:], stdout)
	case "backup":
		return backup(ctx, args[1:], stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, version)
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	}
	fmt.Fprint(stderr, usage)
	return fmt.Errorf("unknown command %q", args[0])
}
```

`run` receives what `main` would otherwise take from globals: a context,
the arguments, and where to write. It reports failure by returning an
error. A test can now call
`run(ctx, []string{"check-rules", path}, &out, io.Discard)` and inspect
`out` and the error, with no process started.
`TestServeEndToEnd` runs the whole server this way.

`io.Writer` is the interface with the single method `Write`. `os.Stdout`,
a `bytes.Buffer` and a network connection all satisfy it, which is why one
parameter type serves both production and tests.

`os.Exit` ends the process immediately and deferred calls do not run. That
is the reason `stop()` is called explicitly before it, and a second reason
to keep `os.Exit` in `main` only: inside `run` and `serve`, `defer` can be
relied on.

**Typical mistake:** calling `log.Fatal` or `os.Exit` deep inside the
program. The deferred database close never runs, and the function cannot
be tested.

## Flags and environment variables

The `flag` package parses command-line flags. Each command has its own
`FlagSet`, so `serve` and `backup` can have different flags:

From `cmd/candledb/serve.go`:

```go
	flags := flag.NewFlagSet("candledb serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&c.addr, "addr", env("CANDLEDB_ADDR", ":8090"), "address to listen on [CANDLEDB_ADDR]")
	flags.StringVar(&c.dataDir, "data-dir", env("CANDLEDB_DATA_DIR", "./candledb-data"), "directory for the database and generated secrets [CANDLEDB_DATA_DIR]")
```

`StringVar` binds a flag to a variable: the name, the default, and the
help text. After `Parse`, the variable holds the default or the value
given. There are equivalents for other types (`BoolVar`, `IntVar`,
`DurationVar`, which accepts values like `90s` and `1h`).

`flag.ContinueOnError` makes `Parse` return an error for a bad flag. The
default behaviour of the global flag set is to exit the process, which is
the thing this file is avoiding.

The default of each flag comes from an environment variable when one is
set:

From `cmd/candledb/serve.go`:

```go
func env(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return fallback
}
```

`os.LookupEnv` distinguishes "not set" from "set to the empty string";
`os.Getenv` does not. The precedence that results is the conventional one:
a flag beats the environment, the environment beats the built-in default.
Containers are usually configured through the environment, people at a
terminal through flags.

Validation follows parsing, and the server refuses to start with a
configuration that cannot work:

From `cmd/candledb/serve.go`:

```go
	if err := flags.Parse(args); err != nil {
		return c, err
	}
	if (c.tlsCert == "") != (c.tlsKey == "") {
		return c, errors.New("-tls-cert and -tls-key must be given together")
	}
	if c.authRate <= 0 || c.authBurst <= 0 {
		return c, errors.New("-auth-rate and -auth-burst must be positive")
	}
	if c.adminKey != "" && len(c.adminKey) < 24 {
		return c, errors.New("-admin-key must be at least 24 characters")
	}
	return c, nil
```

`(c.tlsCert == "") != (c.tlsKey == "")` reads as "exactly one of the two is
empty". Comparing two booleans with `!=` is exclusive or.

Failing at start-up is a kindness. A misconfigured server that starts and
fails later fails at a worse time.

## Structured logging with `log/slog`

A log line meant for a person is a sentence. A log line meant for a
machine is a set of fields. `log/slog` produces both from the same call.

From `internal/adapters/httpapi/middleware.go`:

```go
		s.logger.Info("request",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(), "request_id", id)
```

The first argument is the message, a constant. The rest are alternating
keys and values. The message says what happened; everything that varies
goes into fields, so that logs can be filtered by `status` or `request_id`
without parsing text.

The *handler* decides the output format, and is chosen once:

From `cmd/candledb/serve.go`:

```go
func newLogger(c config, w io.Writer) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(c.logLevel)); err != nil {
		return nil, fmt.Errorf("unknown log level %q", c.logLevel)
	}
	opts := &slog.HandlerOptions{Level: level}
	switch c.logFormat {
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("unknown log format %q", c.logFormat)
}
```

The text handler writes `key=value` pairs, the JSON handler one JSON
object per line. The level filters: at `info`, calls to `logger.Debug` do
nothing. `level.UnmarshalText` parses `"debug"`, `"info"`, `"warn"` or
`"error"` into a `slog.Level`.

The logger is created here and passed to the components that need it, as
an option. Nothing in the program uses a global logger. A test can
therefore hand a component a logger that writes into a buffer, or one that
discards everything, which is the default inside `app.New`.

**Typical mistake:** building the message with `fmt.Sprintf("user %s
failed", id)`. The ID is now inside a string and cannot be searched for as
a field.

## Listening before serving

From `cmd/candledb/serve.go`:

```go
	listener, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		return err
	}
	logger.Info("candledb is listening", "addr", listener.Addr().String(), "version", version, "tls", cfg.tlsCert != "")
```

`http.ListenAndServe` does both steps in one call and only returns when the
server stops. Splitting them has two benefits. An address that is already
in use is reported right here, as an ordinary error before anything is
running. And with `-addr 127.0.0.1:0` the operating system picks a free
port, which `listener.Addr()` then reveals. The end-to-end test starts the
server that way and reads the port from this log line.

## Server timeouts

From `cmd/candledb/serve.go`:

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

The zero value of `http.Server` has no timeouts. A client can then open a
connection, send one byte a minute, and hold a goroutine and a file
descriptor indefinitely; enough such clients exhaust the server. Each
timeout bounds one phase:

- `ReadHeaderTimeout`: receiving the request line and headers.
- `ReadTimeout`: receiving the whole request, body included.
- `WriteTimeout`: from the end of the headers to the end of the response.
- `IdleTimeout`: how long a kept-alive connection may sit unused.

`WriteTimeout` would end every listen stream after sixty seconds. The
stream handler therefore sets its own deadline before each message with
`http.ResponseController`, as [chapter 5](05-http.md) shows.

`ErrorLog` routes the HTTP server's internal messages through the same
structured logger.

## Signals and graceful shutdown

An operating system tells a process to stop by sending it a *signal*:
`SIGINT` when you press Ctrl-C, `SIGTERM` from `docker stop`, `systemd` or
Kubernetes. By default the process dies at once, in the middle of whatever
it was doing. The first lines of `main`, quoted above, change that:
`signal.NotifyContext` returns a context that is cancelled when one of the
listed signals arrives. The signal has become a cancellation that the rest
of the program already knows how to observe.

`serve` waits for it in a `select`, as one of three things that can happen:

From `cmd/candledb/serve.go`:

```go
		case <-ctx.Done():
			logger.Info("shutting down")
			// Listen streams never finish on their own, so they are closed
			// first; Shutdown then waits for ordinary requests in flight.
			docs.Close()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil {
				return fmt.Errorf("shutdown: %w", err)
			}
			return nil
```

The order of the steps is the point.

1. `docs.Close()` ends all listen sessions. A listen stream is an HTTP
   request that never finishes on its own. The next step waits for requests
   to finish, so the streams must be ended first.
2. `server.Shutdown` stops accepting connections and waits for requests in
   flight to complete, up to the deadline of the context it is given.
3. `serve` returns, and its deferred calls run in reverse order of their
   registration. The database was opened first, so it is closed last, after
   everything that uses it.

The shutdown context is derived from `context.Background()`, not from
`ctx`. `ctx` is already cancelled, and a cancelled context would make
`Shutdown` give up immediately.

**Typical mistake:** closing the database first "to be safe". Requests
still in flight then fail with errors about a closed database.

## Reloading on SIGHUP

`SIGHUP` is by convention the signal for "reread your configuration". The
older, channel-based API is used for it:

From `cmd/candledb/serve.go`:

```go
	reload := make(chan os.Signal, 1)
	signal.Notify(reload, syscall.SIGHUP)
	defer signal.Stop(reload)
```

From `cmd/candledb/serve.go`:

```go
		case <-reload:
			// SIGHUP reloads the rules without a restart. A file that does
			// not parse is rejected and the current rules stay in force.
			ruleset, source, err := loadRules(cfg)
			if err != nil {
				logger.Error("rules reload failed, keeping the current rules", "error", err)
				continue
			}
			docs.SetRules(ruleset)
			logger.Info("security rules reloaded", "file", source)
```

`signal.Notify` delivers each signal as a value on the channel. The channel
is buffered because the `signal` package does not wait when it delivers: a
signal that arrives while nobody is receiving would be lost with an
unbuffered channel.

A rules file that fails to parse is logged and ignored; the rules in force
stay in force. `continue` goes back to the top of the `for` loop to wait
again. A reload must never be able to leave the server without rules or
take it down. The swap itself is the atomic pointer from
[chapter 8](08-interpreter.md).

## Counting what happens

Logs describe single events. *Metrics* are numbers that summarise many:
requests served, errors, time spent. The store is wrapped in a decorator
that counts:

From `internal/adapters/instrumented/store.go`:

```go
func (s *Store) Query(ctx context.Context, q domain.Query, keep func(domain.Document) bool) (docs []domain.Document, err error) {
	defer func(start time.Time) { s.Queries.record(start, err) }(time.Now())
	return s.next.Query(ctx, q, keep)
}
```

The line with `defer` is dense. `func(start time.Time) { ... }(time.Now())`
defines a function and calls it with `time.Now()` as argument. With `defer`
in front, the argument is evaluated now, when the `defer` statement runs,
and the body later, when `Query` returns. So `start` is the time at entry
and `record` runs at exit.

`record` also receives `err`, and it is the error that `Query` finally
returns. That works because the results are *named* in the signature,
`(docs []domain.Document, err error)`. A deferred function can read named
results after the `return` statement has set them.

The counters are served as text on `/metrics`, in the format Prometheus
reads, by `handleMetrics`. The endpoint requires the admin key.

## Building the binary

`go build ./cmd/candledb` is enough to get a working binary. The Makefile
adds flags that matter for a release:

From `Makefile`:

```make
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
```

From `Makefile`:

```make
build: ## build the server into bin/candledb
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/candledb ./cmd/candledb
```

- `-ldflags "-X main.version=..."` sets a string variable at link time.
  `main.go` declares `var version = "dev"`, and the build overwrites it with
  the output of `git describe`. The binary knows its own version without a
  generated file.
- `-s -w` leave out the symbol table and debug information, which makes the
  file smaller. Stack traces from panics still work.
- `-trimpath` removes the build machine's directory names from the binary.
- `CGO_ENABLED=0` forbids linking against C code. The result is a *static*
  binary: it needs no shared libraries from the system it runs on. This is
  possible because the SQLite driver is pure Go
  ([chapter 6](06-sql-and-encoding.md)).

A static binary can be copied to another machine of the same kind and run.
For a different kind of machine, Go cross-compiles by setting two
environment variables: `GOOS` (operating system) and `GOARCH` (processor).
No extra toolchain is needed.

## The container image

From `Dockerfile`:

```dockerfile
FROM golang:1.27 AS build
WORKDIR /src
# Copying go.mod and go.sum first lets Docker reuse the downloaded modules
# for every build in which only the source changed.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
# CGO_ENABLED=0 produces a static binary with no dependency on system
# libraries, which is what allows the empty base image below.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /candledb ./cmd/candledb
RUN mkdir /data

# distroless/static has no shell and no package manager: nothing to exploit,
# nothing to update. "nonroot" runs the server as an unprivileged user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /candledb /candledb
COPY --from=build --chown=nonroot:nonroot /data /data
ENV CANDLEDB_DATA_DIR=/data
VOLUME /data
EXPOSE 8090
ENTRYPOINT ["/candledb"]
CMD ["serve"]
```

This is a *multi-stage* build. The first stage has the Go toolchain and
produces the binary. The second stage starts from a different base image
and copies in only the binary and an empty data directory. The toolchain,
the source and the module cache are not in the image that ships.

The final base, `distroless/static`, contains no shell, no package manager
and no C library. It only works for static binaries, which is the practical
payoff of `CGO_ENABLED=0`. `nonroot` makes the server run as an
unprivileged user.

Copying `go.mod` and `go.sum` and downloading modules before copying the
source is about build speed: Docker caches each step, and the download
step is then reused whenever only the source changed.

## Three security habits

**File permissions.** The data directory holds the database and the token
signing secret.

From `cmd/candledb/serve.go`:

```go
	// 0700: the directory holds the database and the signing secret, so
	// only the server's own user may enter it.
	if err := os.MkdirAll(cfg.dataDir, 0o700); err != nil {
		return fmt.Errorf("creating data directory: %w", err)
```

From `cmd/candledb/serve.go`:

```go
	generated := rand.Text() + rand.Text()
	if err := os.WriteFile(path, []byte(generated+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("storing jwt secret: %w", err)
	}
	return []byte(generated), nil
```

`0o700` and `0o600` are octal literals. The three digits are the
permissions of the owner, the group and everyone else; 7 is read, write
and enter, 6 is read and write, 0 is nothing. The directory can only be
entered by the server's user, and the secret can only be read by it. The
database file and its backups are created with `0o600` as well
(`touchPrivate` and `Backup` in
[internal/adapters/sqlite/db.go](../../internal/adapters/sqlite/db.go)),
because they hold password hashes.

**The right random source.** Go has two. `math/rand` (and `math/rand/v2`)
is fast and predictable; with a fixed seed it produces the same sequence
every time. The tests use it for exactly that reason:

From `internal/domain/value_test.go`:

```go
	rng := rand.New(rand.NewPCG(1, 2)) // fixed seed: failures are reproducible
```

`crypto/rand` draws from the operating system's secure source and cannot
be predicted. Everything that must not be guessable uses it: document IDs,
session IDs, refresh tokens, the signing secret.

From `internal/app/ports.go`:

```go
// RandomIDs is the IDGenerator used in production. Each ID is 26 characters
// carrying 130 bits from the operating system's secure random source.
type RandomIDs struct{}

func (RandomIDs) NewID() string { return rand.Text() }
```

Both packages are called `rand` in the code. Check the import block to see
which one a file uses.

**Constant-time comparison.** Comparing a secret with `==` stops at the
first byte that differs. The time it takes therefore reveals how many
leading bytes of a guess were right, and with enough measurements a secret
can be recovered byte by byte.

From `internal/app/auth.go`:

```go
	// ConstantTimeCompare takes the same time whether the first or the last
	// byte differs, so the key cannot be guessed by timing the response.
	if a.cfg.AdminKey != "" && subtle.ConstantTimeCompare([]byte(bearer), []byte(a.cfg.AdminKey)) == 1 {
		return Principal{UID: "admin", Admin: true}, nil
```

`subtle.ConstantTimeCompare` takes time that depends only on the length of
its inputs. The check `a.cfg.AdminKey != ""` in front of it matters as
well: without it, a server with no admin key configured would treat an
empty bearer token as the admin key.

## Benchmarks and profiles

A benchmark is a function named `BenchmarkXxx` in a test file. It runs a
piece of code many times and reports the time per iteration.

From `internal/adapters/sqlite/bench_test.go`:

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

`b.Loop()` returns `true` until the testing package has enough iterations
for a stable figure. Everything before the loop is setup and is not timed.

A benchmark says how fast. A *profile* says where the time goes. The same
command can record one: `-cpuprofile` samples what the program is executing
about a hundred times a second, and `go tool pprof` summarises the samples
by function.

Measure before optimising. The profile in "Try it" below shows a share of
the query time inside SQLite's SQL parser, which suggests a concrete
improvement (reusing prepared statements for queries) and also shows that
several things one might have guessed are not where the time goes.

The server itself has no profiling endpoint. The standard library offers
one in `net/http/pprof`, and it is not mounted here. Profiles are taken
from benchmarks. Adding an endpoint restricted to the admin key is this
chapter's stretch exercise.

## What CI checks

Every push runs the static checks, the tests on three operating systems,
a coverage minimum, a short fuzzing run, a build for every platform, the
JavaScript client's tests, and a test of the container image.
[Chapter 11](11-continuous-integration.md) goes through all of it. The
short version is one command:

```sh
make check
```

## What "production ready" means here

It is worth being precise about the claim.

What the server does to deserve it: commits are atomic and durable;
everything that reaches it from outside is validated and bounded in size;
access is denied by default; secrets are compared and stored with care; it
has timeouts, rate limits on sign-in, health and readiness endpoints,
metrics, structured logs, online backup, a schema migration path, and a
clean shutdown; and its behaviour is pinned by tests that run under the
race detector.

What it is not: it is a single process on a single machine with a SQLite
file. There is no replication and no failover. If the machine is down, the
database is down, and durability is that of one disk plus your backups.
Write throughput is that of one writer. These are the properties of the
design, chosen for being lightweight, and they are fine for many
applications and wrong for others.
[Tour: operations](../tour/08-operations.md) describes how to run it within
those limits.

## Try it

Print the help of `serve` and read the flags, each with its environment
variable in brackets:

```sh
./bin/candledb serve -h
```

Build a binary with a version, and two for other platforms. They go to
`/tmp` so the repository's `bin/candledb` is left alone:

```sh
go build -ldflags "-X main.version=1.2.3" -o /tmp/candledb-demo ./cmd/candledb
/tmp/candledb-demo version

GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/candledb.exe ./cmd/candledb
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/candledb-arm64 ./cmd/candledb
file /tmp/candledb.exe /tmp/candledb-arm64 bin/candledb
```

The first prints `1.2.3`. `file` reports a Windows executable, an ARM
binary, and for the Linux ones the words `statically linked`.

Watch the life cycle. Start a server with JSON logs and no rules file:

```sh
CANDLEDB_LOG_FORMAT=json ./bin/candledb serve -addr 127.0.0.1:18123 -data-dir /tmp/candledb-ch10
```

It logs a warning that every request will be denied. In a second terminal:

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X PUT http://127.0.0.1:18123/v1/documents/a/b -d '{}'
printf 'match /a/{id} { allow read, write; }\n' > /tmp/candledb-ch10/candledb.rules
pkill -HUP -x candledb
curl -s -o /dev/null -w '%{http_code}\n' -X PUT http://127.0.0.1:18123/v1/documents/a/b -d '{}'
stat -c '%a %n' /tmp/candledb-ch10 /tmp/candledb-ch10/jwt.secret
```

`pkill -HUP -x candledb` sends `SIGHUP` to every process named exactly
`candledb`. The first request prints `403`. After the signal the server logs
`security rules reloaded` and the same request prints `200`. `stat` shows
`700` for the directory and `600` for the secret. Press Ctrl-C in the first
terminal and the last log line is `shutting down`.

Take a CPU profile of a benchmark and list the functions that used the
most time:

```sh
go test ./internal/adapters/sqlite -run '^$' -bench QueryEquality -cpuprofile /tmp/cpu.out -o /tmp/sqlite.test
go tool pprof -top /tmp/sqlite.test /tmp/cpu.out | head -20
```

The `flat` column is time spent in the function itself, `cum` includes
everything it called. Functions whose names begin with
`modernc.org/sqlite/lib._sqlite3` are SQLite; `_yy_reduce` is its SQL
parser.

Finally, run what CI runs locally:

```sh
make check
```

## Exercise

Make the maximum number of documents a query may return configurable.

**Goal.** `candledb serve -max-query-results 500` (or the environment
variable `CANDLEDB_MAX_QUERY_RESULTS`) sets the limit that is 10,000 by
default. A query without a limit that matches more documents than that is
refused with `resource_exhausted`.

**Files.** `cmd/candledb/serve.go` and `cmd/candledb/main_test.go`.

**Hints.**

- The limit already exists as `MaxQueryResults` in `app.Limits`, and there
  is an option that replaces the limits. Start from `app.DefaultLimits` and
  change one field, so the others keep their defaults.
- `env` returns a string and the flag is an integer. Decide what should
  happen when the environment variable holds something that is not a
  number, and remember the principle from the validation section.
- A limit of zero or less cannot work. Reject it at start-up, next to the
  other checks, and add a case to `TestServeFlagValidation`.
- To test the behaviour, extend `TestServeEndToEnd` or write a smaller test
  on the same model: start `serve` with a limit of 2, write three documents
  with the admin key, and query the collection.

**How to know it works.** The new validation case passes; your test sees
HTTP status 429 with the code `resource_exhausted` for the unlimited query
and 200 for the same query with `"limit": 2`; `./bin/candledb serve -h`
lists the flag after you rebuild.

**Stretch.** Mount the handlers of `net/http/pprof` under `/debug/pprof/`
so that they answer only to the admin key, as `/metrics` does. Think about
which of the server's timeouts a thirty-second CPU profile runs into, and
how the listen handler solved the same problem.

## Further reading

- [How to Write Go Code](https://go.dev/doc/code) and [Organizing a Go module](https://go.dev/doc/modules/layout)
- [flag package documentation](https://pkg.go.dev/flag)
- [Structured Logging with slog](https://go.dev/blog/slog)
- [os/signal package documentation](https://pkg.go.dev/os/signal) and [net/http `Server.Shutdown`](https://pkg.go.dev/net/http#Server.Shutdown)
- [Profiling Go Programs](https://go.dev/blog/pprof)
- [testing package documentation: benchmarks](https://pkg.go.dev/testing#hdr-Benchmarks)
- [Vulnerability Management for Go](https://go.dev/blog/vuln)
- [crypto/subtle package documentation](https://pkg.go.dev/crypto/subtle)

Next: [Continuous integration](11-continuous-integration.md)
