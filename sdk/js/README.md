# candledb-client

The JavaScript and TypeScript client for the candledb document database.
It has no runtime dependencies and runs in browsers and in Node 20 or newer.

```sh
npm install        # installs TypeScript, the only dev dependency
npm run build      # compiles src/ to dist/
npm test           # builds, then runs the tests against a real server
```

The tests start the server binary from `../../bin/candledb` (built on demand
with `go build`); set `CANDLEDB_BIN` to use another one.

## Getting started

```ts
import { createClient, increment, serverTimestamp } from "candledb-client";

const db = createClient({ url: "http://localhost:8090", storage: localStorage });

await db.auth.signUp("ada@example.com", "a long password");

const todos = db.collection("todos");
const ref = await todos.add({ title: "Write docs", done: false, created: serverTimestamp() });
await ref.update({ done: true, edits: increment(1) });

const open = await todos.where("done", "==", false).orderBy("created", "desc").limit(20).get();
open.forEach((doc) => console.log(doc.id, doc.data()));
```

## Documents

```ts
const ref = db.doc("users/alice");            // or db.collection("users").doc("alice")

await ref.set({ name: "Alice", address: { city: "Paris" } });   // replace
await ref.set({ age: 30 }, { merge: true });                    // merge into what is there
await ref.update({ "address.city": "Lyon" });                   // dotted paths; fails if missing
await ref.delete();

const snap = await ref.get();
snap.exists; snap.id; snap.path; snap.version;
snap.data();                 // undefined if the document does not exist
snap.get("address.city");
```

`Date` values are stored as timestamps and come back as `Date`. Field names
may not contain `.` or start with `$`.

Values the server computes at commit time:

| Helper | Effect |
|---|---|
| `serverTimestamp()` | the server's commit time |
| `increment(n)` | adds `n` to a number (a missing field counts as 0) |
| `arrayUnion(...items)` | appends the items that are not present yet |
| `arrayRemove(...items)` | removes every occurrence of the items |
| `deleteField()` | removes the field (in `update` and merging `set` only) |

## Queries

```ts
db.collection("users")
  .where("age", ">=", 18)              // == != < <= > >= in not-in array-contains array-contains-any
  .where("tags", "array-contains", "admin")
  .orderBy("age", "desc")
  .limit(10)
  .startAfter(lastSnapshot);           // or values: startAt(30), endBefore(40, "docId")
```

Results are ordered by the `orderBy` fields and then by document ID. A
document that lacks an ordered field is not part of the result. Queries only
return documents the security rules let the caller read.

To paginate, pass the last document of a page to `startAfter`.

## Realtime

```ts
const stop = db.collection("todos").where("done", "==", false).onSnapshot(
  (snapshot) => {
    for (const change of snapshot.docChanges()) {
      console.log(change.type, change.doc.id);       // added | modified | removed
    }
    render(snapshot.docs);
  },
  (error) => console.error(error.code),
);

stop();   // stop listening
```

Document references have the same `onSnapshot`. All listeners of a client
share one connection, which is opened with the first listener, closed with
the last, and re-established automatically after a network failure or a
server restart. After a reconnect a listener is only called if its result
really differs from what it showed before.

The stream is also reopened if it stays completely silent for 60 seconds
(the server sends a keep-alive every 20), which catches connections that
died without either side being told. The timings can be changed with
`createClient({ url, reconnect: { minDelayMs, maxDelayMs, idleTimeoutMs } })`.

### Local writes show immediately

A write made through the client appears in that client's listeners at once,
before the server has answered, with `metadata.hasPendingWrites` set to
`true`. When the server confirms it, the listener fires again with the flag
cleared. If the server refuses the write, the listener returns to the
previous state and the write's promise rejects.

Values of `serverTimestamp()` are estimated from the local clock until the
confirmed snapshot arrives.

## Batches and transactions

```ts
await db.batch()
  .set(db.doc("a/1"), { n: 1 })
  .update(db.doc("a/2"), { n: increment(1) })
  .delete(db.doc("a/3"))
  .commit();                      // all or nothing

await db.runTransaction(async (tx) => {
  const account = await tx.get(db.doc("accounts/alice"));
  tx.update(db.doc("accounts/alice"), { balance: account.get("balance") - 10 });
});
```

A transaction commits only if every document it read is unchanged since the
read, whether or not it also writes that document; otherwise the function
runs again (5 attempts by default, `{ maxAttempts }` to change it). Do all
reads before the first write, and keep the function free of side effects.

## Accounts

```ts
await db.auth.signUp(email, password);
await db.auth.signIn(email, password);
await db.auth.signOut();
db.auth.currentUser;                               // { id, email, createdAt } or null
const stop = db.auth.onAuthStateChanged((user) => { /* ... */ });
```

The access token is renewed automatically shortly before it expires and
whenever the server rejects it. Pass `storage` (anything shaped like
`localStorage`; methods may be async) to keep the user signed in across page
loads. Signing in or out reconnects the realtime stream, so listeners
immediately reflect what the new identity may read.

Server-side code can bypass security rules with the server's admin key:

```ts
const admin = createClient({ url, adminKey: process.env.CANDLEDB_ADMIN_KEY });
```

Never ship the admin key to a browser.

## Errors

Every failed operation rejects with a `CandleDBError` carrying the server's
`code` and the HTTP `status`:

| code | status | meaning |
|---|---|---|
| `invalid_argument` | 400 | malformed path, data or query (also raised locally, status 0) |
| `unauthenticated` | 401 | missing, expired or wrong credentials |
| `permission_denied` | 403 | the security rules refuse the operation |
| `not_found` | 404 | `update` of a document that does not exist |
| `already_exists` | 409 | `create` of a document that exists; email already registered |
| `failed_precondition` | 412 | a transaction lost a race |
| `resource_exhausted` | 429 | rate limit, or a query result that is too large |
| `unavailable` | 0 | the server could not be reached |

## Source layout

| File | Contents |
|---|---|
| `src/client.ts` | client, references, queries, snapshots, batches, transactions |
| `src/auth.ts` | accounts and token renewal |
| `src/http.ts` | the JSON transport |
| `src/listen.ts` | the realtime stream, reconnection, optimistic views |
| `src/query.ts` | query description and a local implementation of query semantics |
| `src/mutations.ts` | writes and their local estimate |
| `src/values.ts` | value encoding, sentinels and the server's ordering |
| `src/types.ts` | wire types and `CandleDBError` |
