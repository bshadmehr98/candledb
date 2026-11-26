# 00 · How to read this

This track teaches Go by walking through candledb's own source. The code is
the real server, not a simplified teaching version; the explanations live in
these chapters, and the code stays as it would be in any production project.

## Who this is for

You can already program in some other language, and you want to learn Go by
reading a complete program instead of isolated snippets. No Go knowledge is
assumed. Knowledge of what candledb does is assumed, which is why the tour
comes first.

## Reading order

The documentation has three tracks. Read them in this order:

1. **[The tour](../tour/01-quickstart.md)** explains what candledb is and how
   to use it. It contains no Go. Read all of it first: every later chapter
   takes for granted that you know what a document, a query, a listener and
   a rule are.
2. **Go by feature** (this track) takes one part of the tool per chapter,
   shows where its code is and teaches the Go that part uses. The chapters go
   from simple to advanced, so read them in order.
3. **[Architecture](../architecture/01-hexagonal.md)** explains how the parts
   are arranged (hexagonal architecture) and the design patterns in the code.
   You can read it after chapter 04 of this track, which introduces the idea
   from the Go side, or at the end.

## The chapters

| Chapter | Part of candledb | Go you learn |
|---|---|---|
| 00 · How to read this | the repository | tooling, modules, packages |
| [01 · Types and values](01-types-and-values.md) | documents, values, paths | variables, structs, named types, methods, slices, maps, interfaces, type switches |
| [02 · Errors](02-errors.md) | how failures are reported | error values, wrapping, `errors.Is` and `errors.As`, `defer`, `panic` |
| [03 · Testing](03-testing.md) | the test suite | table tests, subtests, fakes, property tests, fuzzing, benchmarks |
| [04 · Interfaces and ports](04-interfaces-and-ports.md) | storage and the other ports | interfaces, embedding, mutexes, function values |
| [05 · HTTP](05-http.md) | the HTTP API | `net/http`, `encoding/json`, `context`, middleware |
| [06 · SQL and encoding](06-sql-and-encoding.md) | the SQLite store | `database/sql`, transactions, byte encoding |
| [07 · The query engine](07-query-engine.md) | query planning and execution | sorting, generics, building SQL safely |
| [08 · An interpreter](08-interpreter.md) | the security rules language | lexing, parsing, recursion, tables of functions |
| [09 · Concurrency](09-concurrency.md) | realtime listeners | goroutines, channels, `select`, locks, the race detector |
| [10 · Production](10-production.md) | the `candledb` command | flags, logging, signals, graceful shutdown |

Chapters 01 to 05 each end with an exercise: a small, real change to this
codebase. Do them. Reading code teaches you to recognise Go; only writing it
teaches you to produce it. The exercises of chapters 01 and 05 belong
together, so keep your work from one chapter to the next.

## Install Go

Install Go from <https://go.dev/dl/>. The `go.mod` file at the root of the
repository names the version the project was written with:

```
module github.com/bshadmehr98/candledb

go 1.27.2
```

Any release at or after that version works. Check yours with `go version`.

You also need an editor with Go support. Visual Studio Code with the official
Go extension, or GoLand, both give you the two features you will use all the
time: "go to definition" and "find references". Install one of them before
you continue; reading a codebase without go-to-definition is slow.

## The commands you need

Go ships as one program, `go`, with subcommands. Run all of these from the
root of the repository.

| Command | What it does |
|---|---|
| `go build ./...` | compiles every package and reports errors; writes no files |
| `go build -o bin/candledb ./cmd/candledb` | compiles the server into the file `bin/candledb` |
| `go run ./cmd/candledb serve -dev-open-rules` | compiles and runs in one step |
| `go test ./...` | runs every test |
| `go test ./internal/domain -run TestCompareOrder -v` | runs the tests of one package whose name matches a pattern, verbosely |
| `go vet ./...` | reports code that compiles but is probably wrong |
| `gofmt -l .` | lists files that are not in the standard format; `gofmt -w .` rewrites them |
| `go doc ./internal/domain Compare` | prints the documentation of one identifier |

`./...` is a pattern that means "this directory and everything below it".

Two things here differ from many other languages. First, there is exactly one
code format, and `gofmt` produces it. Nobody discusses brace placement in Go;
your editor runs `gofmt` when you save. Second, documentation is the comment
directly above a declaration, and `go doc` prints it. That is why every
exported name in this codebase has a comment that starts with the name.

The `Makefile` wraps the common combinations. `make help` lists them:

```
  help         list the targets
  build        build the server into bin/candledb
  run          run a development server with open rules
  test         run all Go tests
  test-short   run Go tests, skipping the long randomised ones
  test-race    run all Go tests with the race detector
  fuzz         fuzz the rules parser for 30 seconds
  bench        run the storage benchmarks
  cover        write coverage.out and print the total
  lint         check formatting and run go vet
  sdk          build the JavaScript client
  sdk-test     test the JavaScript client against the server
  check        everything CI runs
  docker       build the container image
  clean        remove build output
```

## Modules and packages

A **module** is a tree of Go code that is versioned and released as one unit.
The file `go.mod` at its root declares it. Ours starts with `module candledb`,
so the module's name is `candledb`. `go.mod` also lists the modules we depend
on, with exact versions; `go.sum` holds checksums of those so that a download
cannot be tampered with.

A **package** is one directory of `.go` files that are compiled together.
Every file starts with a `package` clause, and all files in a directory must
name the same package. The files of a package see each other's declarations
freely; there is nothing to import between files of one directory.

A package is imported by its **import path**: the module name followed by the
directory. The directory `internal/domain` is therefore imported as
`"github.com/bshadmehr98/candledb/internal/domain"`, and code then refers to its contents with the
package name as a prefix, such as `domain.Compare`.

Visibility is decided by the first letter of a name. A name that starts with
an upper-case letter is **exported**: other packages can use it. A name that
starts with a lower-case letter is visible only inside its own package. There
are no `public` or `private` keywords. `domain.Compare` is exported;
`validateSegment` in the same package is not.

Go forbids import cycles: if package A imports B, then B cannot import A,
directly or through other packages. This rule shapes the whole layout of the
project, as chapter 04 shows.

## The layout of this repository

```
cmd/candledb/                 the program: flags, wiring, startup and shutdown
internal/
  domain/                     values, documents, paths, queries, writes; pure logic, no I/O
    domaintest/               random values for property tests
  rules/                      the security rules language: lexer, parser, evaluator
  app/                        the use cases (get, query, commit, listen, accounts) and the ports
    storetest/                the test suite every storage adapter must pass
  adapters/
    memory/                   storage in Go maps; for tests, and the simplest adapter to read
    sqlite/                   storage in a SQLite file, with indexes and a query planner
    instrumented/             a wrapper that counts and times storage calls
    bcrypthash/               password hashing
    jwtauth/                  access tokens
    httpapi/                  the HTTP and JSON API, including the realtime stream
test/integration/             tests of the assembled server over real HTTP
sdk/js/                       the TypeScript client (not Go)
examples/todo/                a small app built on candledb
docs/                         what you are reading
```

Two directory names have a meaning to the Go tools.

`internal/` restricts imports. Code under `internal/` can only be imported by
code inside the same module (precisely: by code rooted at the parent of the
`internal` directory). Nothing outside this repository can depend on our
packages, so we are free to change them. Almost all of candledb is under
`internal/` for that reason: it is a program, not a library.

`cmd/` is a convention, not a rule: it holds the packages that build into
programs. A package named `main` with a function `main` is a program;
`cmd/candledb` is the only one here.

You can list the packages of the module with `go list ./...`.

## How to find your way around

When a chapter mentions a function, open it. Three ways to get there:

- In your editor, put the cursor on a name and use "go to definition". This
  is the fastest way and works across packages and into the standard library.
- On the command line, `go doc ./internal/app Store` prints the declaration
  and documentation of `Store` in package `app`.
  `go doc -src ./internal/domain Equal` prints the source of a function, and
  `go doc -all ./internal/app` prints everything a package exports.
- Every package in this repository has a comment explaining its role, in a
  file named `doc.go` or at the top of its main file. `go doc ./internal/app`
  prints it. Read it before you read the package.

Excerpts in these chapters are copied from the source and labelled with their
file. They are shortened only by leaving out whole lines before or after, never
by editing. The excerpt is a starting point; the file around it is the
real text.

## Try it

```sh
go version
go build ./...
go test -short ./...
go doc ./internal/domain
go doc ./internal/domain Compare
go list ./...
make build
./bin/candledb version
```

`go test -short ./...` should print one `ok` line per package that has tests.
If it does, your setup works and you are ready for chapter 01.

## Further reading

- [A Tour of Go](https://go.dev/tour/): the official interactive
  introduction. Doing it alongside chapters 01 to 04 works well.
- [How to Write Go Code](https://go.dev/doc/code): modules, packages and the
  `go` command in more detail.
- [Effective Go](https://go.dev/doc/effective_go): the idioms of the
  language. Some parts are dated, but it is still the best description of
  the style this codebase follows.

**Next:** [01 · Types and values](01-types-and-values.md)
