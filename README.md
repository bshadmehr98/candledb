# candledb

A lightweight document database with realtime sync, in the spirit of
Firestore: a backend that a frontend talks to directly, with no application
server of your own in between. It is one static binary and one SQLite file.

It is also a codebase written to be read. The documentation walks through it
to teach Go, hexagonal architecture and a handful of design patterns. See
[Learning from this codebase](#learning-from-this-codebase).

## What it does

- **Documents and collections**: schemaless JSON documents at paths such as
  `users/alice/posts/p1`, with subcollections.
- **Queries**: filters, ordering, limits and cursors. Every field is indexed
  automatically; there are no indexes to declare.
- **Realtime listeners**: subscribe to a document or a query and receive
  changes as they are committed, over Server-Sent Events.
- **Security rules**: a small declarative language decides who may read and
  write what, evaluated on the server for every request.
- **Atomic writes**: batches, preconditions, server-side transforms
  (`increment`, `serverTimestamp`, `arrayUnion`, ...) and optimistic
  transactions.
- **Accounts**: built-in email and password sign-up with access and refresh
  tokens, or bring your own HS256 tokens.
- **A JavaScript client** ([sdk/js](sdk/js)) with automatic
  reconnection and optimistic local writes.

## Quick start

You need Go 1.27 or newer, and Node 20 or newer for the JavaScript client.

```sh
make build                          # builds bin/candledb
./bin/candledb serve -dev-open-rules
```

`-dev-open-rules` lets anyone read and write everything, which is only for
trying things out. In a second terminal:

```sh
curl -X PUT localhost:8090/v1/documents/users/ada -d '{"name": "Ada", "age": 36}'
curl localhost:8090/v1/documents/users/ada
curl -X POST localhost:8090/v1/query \
  -d '{"collection": "users", "where": [{"field": "age", "op": ">", "value": 30}]}'
```

From JavaScript:

```js
import { createClient } from "candledb-client";

const db = createClient({ url: "http://localhost:8090" });
await db.doc("users/ada").set({ name: "Ada", age: 36 });

db.collection("users").where("age", ">", 30).onSnapshot((snapshot) => {
  console.log(snapshot.docs.map((doc) => doc.data()));
});
```

Without `-dev-open-rules` the server reads `candledb-data/candledb.rules`;
with no rules file it denies everything. A first rules file:

```
match /users/{uid} {
  allow read: if true;
  allow write: if request.auth != null && request.auth.uid == uid;
}
```

[examples/todo](examples/todo) is a complete app built this way: one HTML
file and one rules file.

## Documentation

All of it is under [docs/](docs/README.md), in three tracks meant to be read
in order:

| Track | What it covers |
|---|---|
| [Tour](docs/tour/01-quickstart.md) | Using candledb: data model, queries, realtime, auth, rules, operations, HTTP API. No Go. |
| [Go by feature](docs/go/00-how-to-read-this.md) | The Go language, taught one part of this codebase at a time, from structs to concurrency. |
| [Architecture](docs/architecture/01-hexagonal.md) | Hexagonal architecture and the design patterns used here, and why. |

## Learning from this codebase

The code is production code, not a simplified teaching version. The teaching
is in the docs, which point at real files and explain them. Each Go chapter
ends with an exercise: a small real feature for you to add.

The layout follows hexagonal architecture:

```
cmd/candledb/            the program: wires everything together
internal/
  domain/                values, documents, paths, queries, writes (pure Go, no I/O)
  rules/                 the security rules language: lexer, parser, evaluator
  app/                   use cases, and the ports (interfaces) they need
  adapters/
    httpapi/             HTTP and Server-Sent Events    (drives the app)
    sqlite/              persistent storage             (driven by the app)
    memory/              in-memory storage for tests    (read this adapter first)
    jwtauth/             access tokens
    bcrypthash/          password hashing
    instrumented/        metrics wrapper around a store
sdk/js/                  TypeScript client
test/integration/        tests of the whole server over real HTTP
examples/todo/           a small complete app
docs/                    the three documentation tracks
```

## Development

```sh
make help         # list all targets
make test         # Go tests
make test-race    # Go tests with the race detector
make sdk-test     # JavaScript client tests against the real server
make check        # everything CI runs, except the container test
make docker-smoke # build the container image and test it
make bench        # storage benchmarks
make fuzz         # fuzz the rules parser
```

## Running in production

[docs/tour/08-operations.md](docs/tour/08-operations.md) covers
configuration, TLS, backups, monitoring and a deployment checklist. In
short:

```sh
candledb serve -data-dir /var/lib/candledb -admin-key "$ADMIN_KEY" -cors-origins https://app.example.com
candledb backup -data-dir /var/lib/candledb /backups/candledb-$(date +%F).db
candledb check-rules candledb.rules && kill -HUP "$(pidof candledb)"   # reload rules
```

Know the limits before relying on it. candledb is a single process on a
single machine. It has no replication or clustering, so its availability is
that of the machine it runs on, and its write throughput is that of one
SQLite file. [docs/tour/10-differences-from-firestore.md](docs/tour/10-differences-from-firestore.md)
lists what it does differently from Firestore and what it leaves out.
