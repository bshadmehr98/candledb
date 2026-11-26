# 3. Reading and writing

This page covers every way to read and change a single document, then how to
change several at once: batches, preconditions, and transactions. Each
operation is shown as an HTTP request with `curl` and as a call to the
JavaScript client. The examples assume a server started with
`-dev-open-rules` as in the [quickstart](01-quickstart.md) and, for
JavaScript, a client created like this:

```js
import {
  createClient, increment, serverTimestamp, arrayUnion, arrayRemove, deleteField,
} from "candledb-client";

const db = createClient({ url: "http://localhost:8090" });
```

## Overview

| Operation | HTTP | Client | If the document is missing |
|---|---|---|---|
| Read | `GET /v1/documents/{path}` | `ref.get()` | 404 over HTTP; `snapshot.exists === false` in the client |
| Set (replace) | `PUT /v1/documents/{path}` | `ref.set(data)` | creates it |
| Set with merge | `PUT /v1/documents/{path}?merge=true` | `ref.set(data, { merge: true })` | creates it |
| Update | `PATCH /v1/documents/{path}` | `ref.update(data)` | fails with `not_found` |
| Delete | `DELETE /v1/documents/{path}` | `ref.delete()` | succeeds |
| Add | `POST /v1/documents/{collection}` | `collection.add(data)` | always creates a new one |
| Batch | `POST /v1/commit` | `db.batch()` | per write |

Every successful write answers with the new database version and the commit
time:

```json
{"version":5,"commitTime":"2026-11-23T22:06:35.974165Z"}
```

Client write methods return a promise that resolves when the server has
accepted the write and rejects with a `CandleDBError` when it has not.

## Read

```sh
curl -s http://localhost:8090/v1/documents/users/alice
```

```js
const snap = await db.doc("users/alice").get();
snap.exists;               // false if there is no such document
snap.data();               // the fields, or undefined
snap.get("address.city");  // one field, by field path
snap.id; snap.path; snap.version;
```

The client turns a 404 into a snapshot with `exists === false`, so reading a
missing document is not an error there.

## Set

Set replaces the whole document. Fields that are not in the request are gone
afterwards.

```sh
curl -s -X PUT http://localhost:8090/v1/documents/users/alice \
  -d '{"name": "Alice", "age": 30, "tags": ["admin", "early"]}'
```

```js
await db.doc("users/alice").set({ name: "Alice", age: 30, tags: ["admin", "early"] });
```

## Set with merge

With merge, the request is laid over what is stored. Fields you name are
written; everything else stays. Maps are merged field by field, to any depth,
so this adds `address.city` without disturbing other fields of `address`:

```sh
curl -s -X PUT "http://localhost:8090/v1/documents/users/alice?merge=true" \
  -d '{"address": {"city": "Paris"}}'
```

```js
await db.doc("users/alice").set({ address: { city: "Paris" } }, { merge: true });
```

Two details: a list is a value and is replaced as a whole, never merged
element by element; and an empty map `{}` is also written as a value, so
`{"address": {}}` empties the map.

## Update

Update changes the fields it names and requires the document to exist. Its
keys are **field paths**: a dot reaches into a map.

```sh
curl -s -X PATCH http://localhost:8090/v1/documents/users/alice \
  -d '{"address.zip": "75001", "age": 31}'
curl -s http://localhost:8090/v1/documents/users/alice
```

```json
{"path":"users/alice","id":"alice",
 "data":{"address":{"city":"Paris","zip":"75001"},"age":31,"name":"Alice","tags":["admin","early"]},
 "createTime":"2026-11-23T22:06:21.163176Z","updateTime":"2026-11-23T22:06:35.991123Z","version":6}
```

```js
await db.doc("users/alice").update({ "address.zip": "75001", age: 31 });
```

The difference from merge is in how a map value is treated. In an update the
value of a key replaces that field completely: `{"address": {"zip": "1"}}`
leaves `address` with only `zip`. Use the dotted form to change one nested
field.

Updating a document that does not exist is an error:

```sh
curl -s -X PATCH http://localhost:8090/v1/documents/users/nobody -d '{"age": 1}'
```

```json
{"error":{"code":"not_found","message":"not found: document users/nobody"}}
```

```js
try {
  await db.doc("users/nobody").update({ age: 1 });
} catch (err) {
  err.code;    // "not_found"
  err.status;  // 404
}
```

## Delete

```sh
curl -s -X DELETE http://localhost:8090/v1/documents/users/bob
```

```js
await db.doc("users/bob").delete();
```

Deleting a document that does not exist succeeds. Deleting a document does
not delete its subcollections.

## Add

Add creates a document with a random ID, for collections where the ID has no
meaning.

```sh
curl -s -X POST http://localhost:8090/v1/documents/users/alice/posts -d '{"title": "Hello"}'
```

```json
{"version":7,"commitTime":"2026-11-23T22:06:36.041190Z",
 "path":"users/alice/posts/35D6KDYBSQODLNIYOVCF3FWC6G","id":"35D6KDYBSQODLNIYOVCF3FWC6G"}
```

```js
const ref = await db.collection("users/alice/posts").add({ title: "Hello" });
ref.id;    // the new ID
ref.path;  // "users/alice/posts/<id>"
```

The server's IDs are 26 characters from a cryptographically secure random
source. The client does not call this endpoint: it generates a 20-character
ID itself and sends a `create`, so that the new document can appear in local
listeners before the server answers. `db.collection("x").doc()` with no
argument gives a reference with such an ID without writing anything.

## Transforms

A **transform** is a field value computed by the server at commit time from
what is stored. On the wire it is an object with one reserved key; the client
has a helper for each.

| Wire form | Client helper | Effect |
|---|---|---|
| `{"$serverTimestamp": true}` | `serverTimestamp()` | the commit time, as a timestamp |
| `{"$increment": n}` | `increment(n)` | adds `n` to the number; a missing or non-numeric field counts as 0 |
| `{"$arrayUnion": [...]}` | `arrayUnion(...items)` | appends the items that are not already in the list |
| `{"$arrayRemove": [...]}` | `arrayRemove(...items)` | removes every occurrence of the items |
| `{"$delete": true}` | `deleteField()` | removes the field; valid in update and in set with merge only |

```sh
curl -s -X PATCH http://localhost:8090/v1/documents/users/alice -d '{
  "visits":      {"$increment": 1},
  "lastSeen":    {"$serverTimestamp": true},
  "tags":        {"$arrayUnion": ["early", "beta"]},
  "address.zip": {"$delete": true}
}'
curl -s http://localhost:8090/v1/documents/users/alice
```

```json
{"path":"users/alice","id":"alice",
 "data":{"address":{"city":"Paris"},"age":31,
         "lastSeen":{"$timestamp":"2026-11-23T22:06:36.051480Z"},
         "name":"Alice","tags":["admin","early","beta"],"visits":1},
 "createTime":"2026-11-23T22:06:21.163176Z","updateTime":"2026-11-23T22:06:36.051480Z","version":8}
```

```js
await db.doc("users/alice").update({
  visits: increment(1),
  lastSeen: serverTimestamp(),
  tags: arrayUnion("early", "beta"),
  "address.zip": deleteField(),
});
```

Why they exist: without `increment`, adding one to a counter means reading
it, adding one in the client, and writing the result, and two clients doing
that at the same time lose one of the additions. A transform is applied
inside the commit, where writes happen strictly one after another.

More precisely:

- Incrementing an integer by an integer gives an integer; if a double is
  involved the result is a double. Integer results are clamped at the 64-bit
  limits instead of wrapping around.
- `$arrayUnion` and `$arrayRemove` on a field that is missing or not a list
  treat it as an empty list.
- Transforms can appear at any depth inside the maps of a set or a merge. In
  an update they must be the direct value of a key. They cannot appear inside
  a list.
- An object with one unknown `$` key is rejected, not stored.

## Batches

`POST /v1/commit` applies several writes **atomically**: either all of them
take effect or none does. The body is a list of writes, each with an `op`, a
`path`, and (except for `delete` and `verify`) `data`:

| `op` | Meaning |
|---|---|
| `set` | replace; with `"merge": true`, merge |
| `create` | like set, but fails with `already_exists` if the document exists |
| `update` | change named fields; fails with `not_found` if the document is missing |
| `delete` | remove |
| `verify` | change nothing; only check the precondition (see below) |

```sh
curl -s -X POST http://localhost:8090/v1/commit -d '{"writes": [
  {"op": "create", "path": "accounts/a", "data": {"balance": 100}},
  {"op": "create", "path": "accounts/b", "data": {"balance": 0}}
]}'
```

```json
{"version":9,"commitTime":"2026-11-23T22:06:36.069565Z"}
```

A whole batch is one commit, so it produces one version, and listeners see
all of its changes in one message.

If any write fails, nothing is applied. Here the second write fails, and the
first, which was valid, is not applied either:

```sh
curl -s -X POST http://localhost:8090/v1/commit -d '{"writes": [
  {"op": "update", "path": "accounts/a",       "data": {"balance": {"$increment": -30}}},
  {"op": "update", "path": "accounts/missing", "data": {"balance": {"$increment": 30}}}
]}'
```

```json
{"error":{"code":"not_found","message":"write 1 (update accounts/missing): not found: document accounts/missing"}}
```

The error names the failing write by its position in the list.

Writes in a batch apply in order, and a later write sees the result of an
earlier one on the same document. A batch may hold at most 500 writes.

In the client:

```js
await db.batch()
  .set(db.doc("accounts/x"), { balance: 100 })
  .set(db.doc("accounts/y"), { balance: 0 })
  .commit();
```

A batch has `set`, `create`, `update` and `delete`, each taking a document
reference first. The single-document endpoints above are shorthand for a
batch of one.

## Preconditions

A write in a batch can carry a **precondition**, a condition on the stored
document that must hold for the write, and so the whole batch, to go ahead.

| Precondition | Holds when |
|---|---|
| `{"exists": true}` | the document exists |
| `{"exists": false}` | the document does not exist |
| `{"version": n}` | the document exists and its `version` is exactly `n` |

```sh
curl -s -X POST http://localhost:8090/v1/commit -d '{"writes": [
  {"op": "update", "path": "accounts/a", "data": {"balance": 70}, "precondition": {"version": 7}}
]}'
```

```json
{"error":{"code":"failed_precondition","message":"failed precondition: document is at version 9, expected 7"}}
```

The status is 412. A version precondition says "apply this only if nobody
has changed the document since I read it", which is the building block for
transactions.

### Verify

Sometimes a decision depends on a document you read but do not write: you
read a price and write an order. The `verify` op attaches a precondition to
such a document. It changes nothing; it only makes the batch fail if the
precondition does not hold.

```sh
curl -s -X POST http://localhost:8090/v1/commit -d '{"writes": [
  {"op": "verify", "path": "accounts/b", "precondition": {"version": 7}},
  {"op": "set",    "path": "accounts/c", "data": {"balance": 5}}
]}'
```

```json
{"error":{"code":"failed_precondition","message":"write 0 (verify accounts/b): failed precondition: document is at version 9, expected 7"}}
```

A `verify` takes no `data`. It needs permission to read the document, since
it reveals whether the document exists and at which version.

## Transactions

A **transaction** reads some documents, decides what to write based on what
it read, and commits only if none of the documents it read has changed in the
meantime. CandleDB has no server-side transaction sessions. A transaction is
built from the pieces above, by the client:

1. Read each document and remember its version (or that it was missing).
2. Compute the writes.
3. Send one batch in which every document that was read carries a
   precondition: on the write itself if the document is also written,
   otherwise on a `verify`.
4. If the server answers `failed_precondition` (or `already_exists`), someone
   else got there first: start again from step 1.

This scheme is called **optimistic concurrency**: nothing is locked, and
conflicts are detected at the end and retried. It is cheap when conflicts are
rare, which is the normal case for a frontend backend.

The client's `runTransaction` does all four steps:

```js
await db.runTransaction(async (tx) => {
  const from = await tx.get(db.doc("accounts/x"));
  const to   = await tx.get(db.doc("accounts/y"));
  tx.update(db.doc("accounts/x"), { balance: from.get("balance") - 30 });
  tx.update(db.doc("accounts/y"), { balance: to.get("balance") + 30 });
});
```

Rules for the function you pass:

- Do all reads (`tx.get`) before the first write. A read after a write is
  rejected.
- The function may run several times, so it must not have side effects of
  its own, such as sending an email or changing variables that later code
  relies on.
- It is retried up to 5 times by default. Pass `{ maxAttempts: n }` as the
  second argument to `runTransaction` to change that. When the attempts are
  used up, the last `failed_precondition` error is thrown.
- Whatever the function returns is the result of `runTransaction`.
- A transaction that writes nothing sends nothing.

Over plain HTTP the same loop is: `GET` the document and note its `version`,
then `POST /v1/commit` with `"precondition": {"version": ...}`, and repeat
both on a 412.

For a counter or similar, prefer `increment`: it needs no read, so it can
never conflict.

## How writes look to listeners

Writes made through the client appear in that client's own listeners
immediately, marked as pending, and are confirmed or rolled back when the
server answers. That behaviour is described in
[Realtime](05-realtime.md#local-writes-show-immediately).

Next: [4. Queries](04-queries.md)
