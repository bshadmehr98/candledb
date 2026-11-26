# candledb documentation

The documentation has three tracks. They are written to be read in this
order, because each builds on the one before:

1. **Tour**: what candledb does and how to use it. No Go.
2. **Go by feature**: the Go language, learned by reading one part of the
   codebase per chapter.
3. **Architecture**: why the code is organised the way it is.

Reading the tour first matters. It is much easier to understand a piece of
code when you already know what it is supposed to do.

## 1. Tour

| | |
|---|---|
| [01 Quickstart](tour/01-quickstart.md) | Run the server, write, query and listen in ten minutes |
| [02 Data model](tour/02-data-model.md) | Documents, collections, paths, value types and limits |
| [03 Reading and writing](tour/03-reading-and-writing.md) | Set, update, delete, transforms, batches, transactions |
| [04 Queries](tour/04-queries.md) | Filters, ordering, cursors and how queries are executed |
| [05 Realtime](tour/05-realtime.md) | Listeners, the stream protocol and optimistic writes |
| [06 Auth](tour/06-auth.md) | Accounts, tokens and the admin key |
| [07 Security rules](tour/07-security-rules.md) | The rules language, in full |
| [08 Operations](tour/08-operations.md) | Configuration, TLS, backups, monitoring, deployment |
| [09 HTTP API](tour/09-http-api.md) | Reference for every endpoint |
| [10 Differences from Firestore](tour/10-differences-from-firestore.md) | What matches, what differs, what is missing |

## 2. Go by feature

Each chapter takes one part of candledb, shows where its code is, teaches
the Go that part uses, and ends with an exercise. The chapters go from the
simplest Go to the most advanced.

| | Part of candledb | Go you learn |
|---|---|---|
| [00 How to read this](go/00-how-to-read-this.md) | The repository | Tooling, modules, packages |
| [01 Types and values](go/01-types-and-values.md) | The domain model | Structs, methods, slices, maps, interfaces, type switches |
| [02 Errors](go/02-errors.md) | Error handling throughout | Error values, wrapping, `errors.Is` and `errors.As`, `defer` |
| [03 Testing](go/03-testing.md) | The test suite | Table tests, fakes, property tests, fuzzing, benchmarks |
| [04 Interfaces and ports](go/04-interfaces-and-ports.md) | Storage ports, the memory store | Interfaces, embedding, mutexes |
| [05 HTTP](go/05-http.md) | The HTTP API | `net/http`, JSON, `context`, middleware |
| [06 SQL and encoding](go/06-sql-and-encoding.md) | The SQLite store | `database/sql`, transactions, bytes and bits |
| [07 Query engine](go/07-query-engine.md) | Queries and indexes | `slices`, sorting, generics, iterators |
| [08 Interpreter](go/08-interpreter.md) | Security rules | Lexing, parsing, recursion, function tables |
| [09 Concurrency](go/09-concurrency.md) | Realtime listeners | Goroutines, channels, `select`, locks, the race detector |
| [10 Production](go/10-production.md) | The `candledb` command | Flags, logging, signals, shutdown, building and shipping |
| [11 Continuous integration](go/11-continuous-integration.md) | The CI workflows | What each automated check finds, running it locally, reading a failure |
| [12 Releasing](go/12-releasing.md) | The release workflow | Versions, tags, Go module releases, multi-architecture images, publishing safely |

## 3. Architecture

| | |
|---|---|
| [01 Hexagonal architecture](architecture/01-hexagonal.md) | Ports, adapters and the dependency rule, mapped onto this code |
| [02 Request walk-through](architecture/02-request-walkthrough.md) | Three requests followed through every layer |
| [03 Design decisions](architecture/03-design-decisions.md) | What was decided, why, and what it costs |
| [04 Testing strategy](architecture/04-testing-strategy.md) | Which kind of test catches which kind of mistake |
| [Design patterns](architecture/patterns/README.md) | The seven patterns used here, one page each |

The patterns: [ports and adapters](architecture/patterns/ports-and-adapters.md),
[observer](architecture/patterns/observer.md),
[strategy](architecture/patterns/strategy.md),
[interpreter](architecture/patterns/interpreter.md),
[decorator](architecture/patterns/decorator.md),
[command](architecture/patterns/command.md) and
[functional options](architecture/patterns/functional-options.md).

## How to study with this

- Keep the code open next to the chapter. Every chapter links to the files
  it discusses; read those files, not only the excerpts.
- Run things. Each Go chapter has a "Try it" section with commands.
- Do the exercises. Reading code teaches recognition; writing it teaches the
  language. Each exercise is a small real feature, and the existing tests
  tell you when you have broken something.
- Start storage with the memory adapter
  ([internal/adapters/memory](../internal/adapters/memory)). It does in 100
  lines what the SQLite adapter needs 600 for, so it shows what an adapter
  must do before you see how a real one does it.
