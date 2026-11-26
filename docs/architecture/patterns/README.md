# Design patterns in candledb

A design pattern is a named solution to a problem that keeps coming up. The
name is the useful part. Once two people both know what "decorator" means,
one can say "the metrics are a decorator around the store" and the other
knows the shape of the code before opening the file.

The classic catalogue is the 1994 book *Design Patterns* by Gamma, Helm,
Johnson and Vlissides (the "Gang of Four"), written with C++ and Smalltalk
in mind. Later lists added others, including ports and adapters, repository
and, from the Go community, functional options.

## What a pattern is not

- **Not a goal.** Nobody is better off because a codebase "uses twelve
  patterns". A pattern is worth its cost only where its problem exists.
- **Not a library.** You cannot import a pattern. It is a way of arranging
  your own types and functions.
- **Not tied to a syntax.** The books show class diagrams, because their
  languages had classes. In Go the same idea is often an interface with one
  method, a function value, or a struct of data. A pattern has not
  disappeared because it became three lines long.

## The warning

The most common misuse of patterns is to apply them in advance. A beginner
learns "strategy" and wraps every `if` in an interface; learns "factory" and
never calls a constructor directly again. The result is code in which
nothing is where you would look for it.

The rule followed in this codebase: write the plain code first. Reach for a
pattern when the plain code shows a specific pain (the same logic copied
into four handlers, a function with nine parameters, a test that cannot be
written). Each page in this directory therefore starts with the problem as
it appeared here, and ends with the costs and a section on when not to use
the pattern.

## The patterns used here

| Pattern | The problem it solves here | Where |
|---|---|---|
| [Ports and adapters](ports-and-adapters.md) (with repository) | The use cases must not depend on SQLite, and must be testable without it | [`app/ports.go`](../../../internal/app/ports.go), [`adapters/sqlite`](../../../internal/adapters/sqlite), [`adapters/memory`](../../../internal/adapters/memory) |
| [Observer](observer.md) | A commit must reach every interested listener without knowing who they are and without waiting for them | [`app/listen.go`](../../../internal/app/listen.go) |
| [Strategy](strategy.md) | One query executor must run several different access paths; one scan must apply a filter it knows nothing about | [`sqlite/plan.go`](../../../internal/adapters/sqlite/plan.go), [`rules/builtins.go`](../../../internal/rules/builtins.go) |
| [Interpreter](interpreter.md) | Operators must be able to express access rules without recompiling the server | [`internal/rules`](../../../internal/rules) |
| [Decorator](decorator.md) | Logging, CORS, authentication and metrics must wrap every request, or every store call, without being written into each one | [`httpapi/middleware.go`](../../../internal/adapters/httpapi/middleware.go), [`instrumented/store.go`](../../../internal/adapters/instrumented/store.go) |
| [Command](command.md) | A change must be validated, authorised, batched and applied atomically, which requires handling it as data before carrying it out | [`domain/write.go`](../../../internal/domain/write.go) |
| [Functional options](functional-options.md) | Constructors need optional settings without a growing parameter list | [`app/service.go`](../../../internal/app/service.go), [`httpapi/server.go`](../../../internal/adapters/httpapi/server.go) |

A suggested reading order is the order of the table: it runs from the
pattern that shapes the whole program to the one that shapes a single
function signature.

Some smaller named ideas appear in the code without a page of their own:

- **Composition root**: all wiring in `cmd/candledb`
  ([hexagonal](../01-hexagonal.md)).
- **Null object**: `rules.DenyAll()` and `rules.AllowAll()` are rule sets
  that need no special case in the caller, and the default logger discards
  everything instead of being `nil`.
- **Token bucket**: the rate limiter in
  [`httpapi/middleware.go`](../../../internal/adapters/httpapi/middleware.go).
  This is an algorithm more than a design pattern, but it has a name worth
  knowing.
- **Sentinel errors**: the shared error categories in
  [`domain/errors.go`](../../../internal/domain/errors.go)
  ([Go chapter 2](../../go/02-errors.md)).

## Patterns deliberately not used

Knowing why a pattern is absent teaches as much as seeing one applied.

| Pattern | Why it is not here |
|---|---|
| **Singleton** (one global instance) | Globals hide dependencies and make tests interfere with each other. Every object here is created in `main` or in a test and passed to whoever needs it. The only package-level variable that changes while the server runs is the compiled-regex cache in `rules/builtins.go`, which is a cache, not a service. |
| **Abstract factory** / factory method | Go has no constructors to abstract over. `sqlite.Open` and `memory.NewStore` are plain functions, and the choice between them is one line in the composition root. A factory would add a type whose only job is to make that call. |
| **Visitor** | The classic way to add operations to a tree of node types without editing them. In Go a type switch does the job directly: `eval` in the rules engine and `Compare` in the domain are each one function with a `case` per type. Both node sets are closed, so there is no third party who needs to add an operation from outside. See [interpreter](interpreter.md). |
| **Builder** | Useful when an object needs many steps to construct. Here, struct literals and functional options cover it. (The JavaScript client *does* use a builder for queries: `where().orderBy().limit()` reads well in application code.) |
| **Template method** / inheritance hierarchies | Go has no inheritance. Shared behaviour is a function that takes the varying part as a parameter, for example `storetest.Run(t, open)`. |
| **Mediator**, **event bus** | A general publish-subscribe bus with string topics would hide which code reacts to a commit. The hub is a specific observer with one kind of event and one publisher, and both are visible in the types. |
| **Generic repository** (`Repository[T]`) | There are two stores with different needs (`Store`, `UserStore`). A generic base would capture only `Get` and would have to be bypassed for everything interesting. |
| **Unit of work** as an object | `Service.Commit` is a unit of work, but as a function call with a batch, not as an object that tracks changes across calls. Nothing spans requests, so there is nothing to track. |
| **CQRS**, **event sourcing** | Separate write and read models, or storing events instead of state, solve problems of scale and audit that this project does not have. They would triple the storage code. |

## How the pattern pages are organised

Every page has the same sections:

1. **The problem**: what was awkward in this project.
2. **The pattern**: the general idea, with a small diagram.
3. **In candledb**: the files, with short excerpts copied from the source.
4. **What Go changes**: how the Go version differs from the classic one.
5. **Costs and when not to use it.**
6. **Questions to check your understanding**: answerable by reading the
   code. The answers are folded away below each list.
7. **Try it**: a small change to make yourself.

The "Try it" exercises are not implemented anywhere in the repository. Do
them on a branch; each one states how to tell that it works.

Start with [ports and adapters](ports-and-adapters.md).
