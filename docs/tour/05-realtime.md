# 5. Realtime

This page describes how a client is told about changes as they happen. The
first half is the protocol itself, which you need if you write your own
client or debug with `curl`. The second half is what the JavaScript client
builds on top of it: a shared connection, automatic reconnection, and local
writes that show before the server has confirmed them.

## The idea

A client **listens** to a document or to a query. It first receives the
current state, and from then on a message for every commit that affects what
it listens to. The state it holds is always the result of applying those
messages in order, with nothing missed and nothing applied twice.

The transport is **Server-Sent Events** (SSE): an ordinary HTTP response
that is never finished, on which the server writes one message at a time.
Messages flow only from server to client. Requests from the client (adding
and removing listeners) go over separate, ordinary HTTP requests.

## The protocol

### 1. Open the stream

```sh
curl -sN http://localhost:8090/v1/listen
```

The response has `Content-Type: text/event-stream`. Each message is one line
beginning with `data: ` followed by a JSON object, then a blank line. The
first message is always:

```text
data: {"type":"ready","session":"NBWFZEDFU2ZVDLE7TD532Y3VPF"}
```

The **session** is the server's name for this stream. Its ID is random and
unguessable, and it is valid only for as long as the stream stays open.

If the request carries an `Authorization` header, the session belongs to that
caller, and security rules are evaluated for that caller throughout.

### 2. Add targets

A **target** is one thing the session listens to. The client picks an integer
ID for it, unique within the session, and names either a document or a query
(the same JSON as for [`/v1/query`](04-queries.md)):

```sh
curl -s -X POST http://localhost:8090/v1/listen/$SESSION/targets \
  -d '{"id": 1, "query": {"collection": "users", "where": [{"field": "age", "op": ">=", "value": 18}]}}'

curl -s -X POST http://localhost:8090/v1/listen/$SESSION/targets \
  -d '{"id": 2, "document": "users/alice"}'
```

Success is `204 No Content`. The request must carry the same credentials as
the stream; to anyone else the session does not exist and the answer is 404.

One stream carries any number of targets (up to 100). This matters because
browsers allow only a handful of simultaneous connections to one server.

### 3. Receive a snapshot, then changes

The first message for a target is a **snapshot**: the complete current
result.

```text
data: {"type":"snapshot","target":1,"version":2,"changes":[
        {"kind":"added","doc":{"path":"users/alice","id":"alice","data":{...},"createTime":"...","updateTime":"...","version":1}},
        {"kind":"added","doc":{"path":"users/bob",  "id":"bob",  "data":{...},"createTime":"...","updateTime":"...","version":2}}]}
```

For a query target the documents are in query order. For a document target
the list has one entry, or none if the document does not exist.

After that, each commit that concerns the target produces one `change`
message:

```text
data: {"type":"change","target":1,"version":3,"changes":[{"kind":"modified","doc":{"path":"users/bob", ...}}]}

data: {"type":"change","target":1,"version":4,"changes":[{"kind":"removed","path":"users/bob"}]}
```

| `kind` | Meaning | Carries |
|---|---|---|
| `added` | the document entered the result | `doc` |
| `modified` | the document is in the result and changed | `doc` |
| `removed` | the document left the result (deleted, or no longer matching) | `path` |

"Entered" and "left" are about the result, not about existence. A document
that is edited so that it stops matching a query's filter is `removed` for
that listener although it still exists.

A batch of writes is one commit and so arrives as one message.

### What a client must do with changes

The server keeps almost no memory of what each client holds, so a client must
treat changes as **idempotent upserts**:

- `added` and `modified` both mean "this is the document now". A `modified`
  for a document the client does not have is an addition; an `added` for one
  it has is a replacement.
- A `removed` for a document the client does not have is ignored.

The server does send `modified` for a document the client has never seen. It
happens when a document that already matched a query becomes readable to the
caller under the security rules.

Change messages do not say where in the order a document belongs. A client
that shows a sorted query result sorts it itself, using the rules in
[The data model](02-data-model.md#sort-order).

### Versions

Every snapshot and change carries a `version`: the database version the
message brings that target up to (see
[The data model](02-data-model.md#versions)). Versions on one target only
ever increase.

A target also receives a message for a commit that touched its collection (or
its document) without changing anything visible to it. That message has an
empty change list:

```text
data: {"type":"change","target":1,"version":7}
```

(An empty `changes` list is left out of the JSON; treat a missing list as
empty.) Its purpose is the version. A client that has just made a write and
received the commit's version in the HTTP response knows its listeners are up
to date with that write once they have seen a message with that version or a
higher one.

### 4. Remove a target

```sh
curl -s -X DELETE http://localhost:8090/v1/listen/$SESSION/targets/1
```

No further messages arrive for that ID. Closing the stream removes all
targets and ends the session.

### Errors and the end of a stream

Adding a target can fail like any request:

| Situation | Code | Status |
|---|---|---|
| Unknown session, or someone else's | `not_found` | 404 |
| Target ID already in use on this session | `already_exists` | 409 |
| Neither or both of `document` and `query`; invalid query | `invalid_argument` | 400 |
| The rules do not let the caller read the document | `permission_denied` | 403 |
| More than 100 targets; query result above 10,000 documents | `resource_exhausted` | 429 |
| The stream has ended | `session_closed` | 410 |

For a *query* target the rules never cause an error. They filter: the
listener is only ever shown documents the caller may read. A *document*
target that the caller may not read is refused up front.

The server ends a stream by sending one last message and closing the
connection:

```text
data: {"type":"error","code":"session_closed","message":"reconnect to resume"}
data: {"type":"error","code":"unauthenticated","message":"access token has expired"}
```

| Code | Cause | What to do |
|---|---|---|
| `session_closed` | the server is shutting down, or this client fell too far behind | reconnect |
| `unauthenticated` | the access token the stream was opened with has expired | get a new token, then reconnect |

After a reconnect the client has a new session with no targets. It adds them
again and receives a fresh snapshot of each. There are no resume tokens: a
reconnected listener is always re-sent its whole result.

### Keep-alives

When nothing else is sent for 20 seconds the server writes a comment line:

```text
: ping
```

SSE clients ignore lines starting with a colon. The line keeps proxies from
closing an idle connection and lets the server notice a client that has
gone away. A client can use the same signal in reverse: if nothing at all
arrives for well over 20 seconds, the connection is dead.

### Slow clients

The server never lets a slow listener hold up writers. Each session has a
queue of 256 pending commits. If a client reads so slowly that its queue
fills, the server closes that session with `session_closed`. The client
recovers the usual way, by reconnecting and receiving fresh snapshots.

A single message that cannot be written to the connection within 30 seconds
also ends the stream.

### Limits

| Limit | Value |
|---|---|
| Listen sessions per server | 10,000 |
| Targets per session | 100 |
| Commits a session may fall behind | 256 |
| Documents in one snapshot | 10,000 |

## What the JavaScript client adds

```js
const stop = db.collection("todos").where("done", "==", false).onSnapshot(
  (snapshot) => {
    for (const change of snapshot.docChanges()) {
      console.log(change.type, change.doc.id);   // added | modified | removed
    }
    render(snapshot.docs);                       // the full, sorted result
  },
  (error) => console.error(error.code),
);

stop();   // stop listening
```

`db.doc("todos/t1").onSnapshot(...)` works the same way and delivers a
document snapshot (`exists`, `data()`, and so on).

### One shared stream

All listeners of one client share a single stream. It is opened when the
first listener is added and closed when the last one is removed. `db.close()`
stops every listener at once.

### Full snapshots, sorted

The callback always receives the complete current result, sorted by the
query's ordering and cut to its limit, plus `docChanges()` listing what
differs from the previous call. The client does the sorting with a port of
the server's comparison rules, so the order on screen is the order a
`get()` would return.

### Reconnection

If the connection drops, the server restarts, or the stream ends with
`session_closed`, the client reconnects by itself. It waits 0.5 seconds
before the first attempt and doubles the wait after each failure, up to 30
seconds, with random jitter so that many clients cut off at the same moment
do not all return at once. Both bounds can be set with the `reconnect` option
of `createClient` (`{ minDelayMs, maxDelayMs }`).

After reconnecting, it adds every target again and compares the fresh
snapshot with what it had. The callback is called only if something really
differs, and `docChanges()` then lists just those differences. Listeners need
no special handling for a reconnect.

When the stream ends with `unauthenticated`, the client renews its access
token first and then reconnects. Signing in or out also reconnects the
stream, so that listeners immediately show what the new identity may read.

### Local writes show immediately

A write made through the client appears in that client's own listeners at
once, before the server has answered. This is often called **latency
compensation**; the write is applied **optimistically**.

```js
db.doc("todos/t1").onSnapshot((snap) => {
  console.log(snap.get("done"), snap.metadata.hasPendingWrites);
});

await db.doc("todos/t1").update({ done: true });
// logs:  true true     immediately, from the local write
//        true false    once the server has confirmed it
```

- While any local write that affects a listener is unconfirmed, its
  snapshots have `metadata.hasPendingWrites === true`, and so does each
  affected document snapshot. Use it to grey out an item or show a spinner.
- When the server accepts the write, the listener fires again with the flag
  cleared.
- When the server refuses the write (for example, the rules deny it), the
  listener goes back to the previous state and the promise returned by the
  write rejects.
- Server-computed values are estimated meanwhile: `serverTimestamp()` shows
  the local clock, `increment` adds locally. The confirmed snapshot carries
  the real values.

The local write is applied to every listener it affects, including query
listeners: a new document that matches a query's filters is inserted at its
sorted position.

Only writes made through the same client object are shown optimistically.
Other tabs and devices learn of the write from the server, as a normal
change.

### What the client does not do

There is no offline persistence. Listener state lives in memory, and a write
attempted while the server is unreachable fails with the code `unavailable`
instead of being queued for later.

Next: [6. Accounts](06-auth.md)
