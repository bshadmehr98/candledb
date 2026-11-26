# 1. Quickstart

This page takes you from a fresh checkout to a running server, a stored
document, a query, and a live listener, first with `curl` and then with the
JavaScript client. It takes about ten minutes. Every command and every output
shown here was run against a real server; only IDs and timestamps will differ
on your machine.

## What candledb is

CandleDB is a document database that a frontend talks to directly. There is
no application server of your own in between. Three things make that safe and
useful:

- **Documents and collections.** Data is stored as JSON-like documents,
  grouped into collections and addressed by a path such as `users/alice`.
- **Security rules.** A small rules file on the server decides who may read
  and write which documents. Because clients are not trusted, the rules are
  the only thing standing between the internet and your data.
- **Realtime listeners.** A client can subscribe to a document or a query and
  is told about every change as it is committed.

The server is one binary with one data directory. It stores everything in a
single SQLite file.

## Build and run

You need Go 1.27 or newer. From the repository root:

```sh
make build        # writes bin/candledb
./bin/candledb serve -dev-open-rules
```

The server prints two lines and keeps running:

```text
level=WARN msg="DEVELOPMENT MODE: security rules are disabled, anyone can read and write everything"
level=INFO msg="candledb is listening" addr=[::]:8090 version=dev tls=false
```

`-dev-open-rules` turns the security rules off so you can experiment. Without
it, and without a rules file, the server denies every request (see
[Security rules](07-security-rules.md)). Never use the flag on a server that
other people can reach.

The server created a directory `./candledb-data` holding the database. Delete
it to start over.

## Store a document

A **document** is a JSON object stored at a path. `PUT` writes it:

```sh
curl -s -X PUT http://localhost:8090/v1/documents/users/alice \
  -d '{"name": "Alice", "age": 30, "tags": ["admin", "early"]}'
```

```json
{"version":1,"commitTime":"2026-11-23T22:06:21.163176Z"}
```

The path `users/alice` means "the document with ID `alice` in the collection
`users`". The collection did not have to be created first; it exists because
a document is in it.

Read the document back:

```sh
curl -s http://localhost:8090/v1/documents/users/alice
```

```json
{"path":"users/alice","id":"alice",
 "data":{"age":30,"name":"Alice","tags":["admin","early"]},
 "createTime":"2026-11-23T22:06:21.163176Z",
 "updateTime":"2026-11-23T22:06:21.163176Z",
 "version":1}
```

Your data is under `data`. The other fields are metadata the server
maintains; [The data model](02-data-model.md) explains them.

Add a second document so there is something to query:

```sh
curl -s -X PUT http://localhost:8090/v1/documents/users/bob \
  -d '{"name": "Bob", "age": 25}'
```

## Run a query

A **query** selects documents from one collection. It is sent as JSON to
`POST /v1/query`:

```sh
curl -s -X POST http://localhost:8090/v1/query -d '{
  "collection": "users",
  "where":   [{"field": "age", "op": ">=", "value": 18}],
  "orderBy": [{"field": "age", "direction": "desc"}],
  "limit": 10
}'
```

```json
{"documents":[
  {"path":"users/alice","id":"alice","data":{"age":30,"name":"Alice","tags":["admin","early"]}, "...": "..."},
  {"path":"users/bob","id":"bob","data":{"age":25,"name":"Bob"}, "...": "..."}
]}
```

(Each document has the same metadata fields as before; they are cut here.)

## Listen for changes

Listening takes two steps. First open the **stream**, a response that never
ends and carries one message per line starting with `data:`. Leave this
running in its own terminal (`-N` stops curl from buffering):

```sh
curl -sN http://localhost:8090/v1/listen
```

```text
data: {"type":"ready","session":"NBWFZEDFU2ZVDLE7TD532Y3VPF"}
```

The first message names your **session**. In a second terminal, tell that
session what to listen to. The thing listened to is called a **target**, and
you choose a number to identify it:

```sh
curl -s -X POST http://localhost:8090/v1/listen/NBWFZEDFU2ZVDLE7TD532Y3VPF/targets -d '{
  "id": 1,
  "query": {"collection": "users", "where": [{"field": "age", "op": ">=", "value": 18}]}
}'
```

The stream immediately shows the current result of the query, a **snapshot**:

```text
data: {"type":"snapshot","target":1,"version":2,"changes":[
        {"kind":"added","doc":{"path":"users/alice", ...}},
        {"kind":"added","doc":{"path":"users/bob", ...}}]}
```

Now change and then delete Bob:

```sh
curl -s -X PATCH  http://localhost:8090/v1/documents/users/bob -d '{"age": 26}'
curl -s -X DELETE http://localhost:8090/v1/documents/users/bob
```

Each write arrives on the stream as a `change` message:

```text
data: {"type":"change","target":1,"version":3,"changes":[{"kind":"modified","doc":{"path":"users/bob","id":"bob","data":{"age":26,"name":"Bob"}, ...}}]}

data: {"type":"change","target":1,"version":4,"changes":[{"kind":"removed","path":"users/bob"}]}
```

Stop the stream with Ctrl-C. [Realtime](05-realtime.md) describes the
protocol in full.

## The same from JavaScript

You will rarely speak the HTTP protocol by hand. The client in
[`sdk/js`](../../sdk/js/README.md) wraps it and adds reconnection, token
handling and optimistic updates. Build it once:

```sh
make sdk          # runs npm install and compiles sdk/js into sdk/js/dist
```

Then, in a file such as `try.mjs` at the repository root:

```js
import { createClient } from "./sdk/js/dist/index.js";

const db = createClient({ url: "http://localhost:8090" });

// Store and read a document.
await db.doc("users/carol").set({ name: "Carol", age: 41 });
const snap = await db.doc("users/carol").get();
console.log(snap.exists, snap.id, snap.data());

// Run a query.
const adults = await db.collection("users")
  .where("age", ">=", 18).orderBy("age", "desc").limit(10).get();
adults.forEach((doc) => console.log(doc.id, doc.get("age")));

// Listen to the same query.
const stop = db.collection("users").where("age", ">=", 18).onSnapshot((snapshot) => {
  for (const change of snapshot.docChanges()) {
    console.log(change.type, change.doc.id, snapshot.metadata.hasPendingWrites);
  }
});

await new Promise((r) => setTimeout(r, 300));
await db.doc("users/dave").set({ name: "Dave", age: 19 });
await new Promise((r) => setTimeout(r, 300));

stop();        // stop listening
db.close();    // close the client's stream so Node can exit
```

Run it with `node try.mjs`:

```text
true carol { age: 41, name: 'Carol' }
carol 41
alice 30
added alice false
added carol false
added dave true
modified dave false
```

The last two lines show the client's **optimistic update**. The listener
heard about Dave at once, before the server had answered, with
`hasPendingWrites` set to `true`. When the server confirmed the write the
listener fired again with the flag cleared.

In a browser the code is the same. The import comes from your bundler or from
the built file, and `fetch` is the browser's own.

## A complete example

[`examples/todo`](../../examples/todo/README.md) is a small working app:
accounts, a private todo list per user, live updates between browser tabs.
It is one HTML file and one rules file, and its README lists the three
commands needed to run it.

## Where to go from here

The rest of this track covers one topic per page, in the order you are likely
to need them:

| Page | Topic |
|---|---|
| [2. The data model](02-data-model.md) | documents, collections, value types, limits |
| [3. Reading and writing](03-reading-and-writing.md) | every write operation, batches, transactions |
| [4. Queries](04-queries.md) | filters, ordering, pagination |
| [5. Realtime](05-realtime.md) | the listen protocol and what the client adds |
| [6. Accounts](06-auth.md) | sign-up, tokens, the admin key |
| [7. Security rules](07-security-rules.md) | the rules language |
| [8. Operations](08-operations.md) | configuration, backup, monitoring, deployment |
| [9. HTTP API](09-http-api.md) | reference for every endpoint |
| [10. Differences from Firestore](10-differences-from-firestore.md) | what matches and what does not |

Next: [2. The data model](02-data-model.md)
