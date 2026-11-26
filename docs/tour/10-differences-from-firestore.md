# 10. Differences from Firestore

CandleDB borrows Firestore's model: documents in collections, queries over
one collection, declarative security rules, realtime listeners, a client
with optimistic updates. It is not a compatible reimplementation. This page
lists what carries over and what does not, so that someone who knows
Firestore knows what to expect, and someone choosing between them can see
the trade. The statements about candledb were checked against its source;
the Firestore side reflects its public documentation and may have moved on.

## At a glance

| | Firestore | CandleDB |
|---|---|---|
| Deployment | managed service, replicated, scales horizontally | one binary and one SQLite file on one machine |
| Wire protocol | gRPC and a typed REST API | plain JSON over HTTP, Server-Sent Events for listeners |
| Client libraries | many platforms | one JavaScript/TypeScript client |
| API compatibility | | none: different endpoints, different client API, a different rules dialect |

## The same

These behave the way a Firestore user expects.

| Area | Behaviour |
|---|---|
| Data model | Schemaless documents addressed by alternating collection/document paths; subcollections; collections exist implicitly; deleting a document leaves its subcollections. |
| Document size | About 1 MiB, nesting up to 20 levels, names up to 1500 bytes. |
| Writes | `set`, set with merge, `update` with dotted field paths, `delete`, `add` with a generated ID; atomic batches of up to 500 writes. |
| Transforms | Server timestamp, increment, array union, array remove, field delete. |
| Ordering across types | One order over all values, with types ranked (null, booleans, numbers, timestamps, strings, lists, maps); integers and doubles compare with each other; strings by UTF-8 bytes. |
| Query operators | `==`, `!=`, `<`, `<=`, `>`, `>=`, `in`, `not-in`, `array-contains`, `array-contains-any`; a document must have the field to match, including for `!=` and `not-in`. |
| Ordering | Documents lacking an ordered field are excluded; ties broken by document ID. |
| Cursors | `startAt`, `startAfter`, `endAt`, `endBefore`, by values or by document. |
| Rules | `match` blocks with `{wildcards}` and `{rest=**}`; `allow` with `read`, `write`, `get`, `list`, `create`, `update`, `delete`; default deny; `request.auth`, `request.resource`, `resource`; a failing condition counts as false; custom functions. |
| Listeners | An initial snapshot followed by `added` / `modified` / `removed` changes; local writes shown immediately with `hasPendingWrites`. |
| Transactions | Optimistic: reads are validated at commit, and the function is retried on conflict. |

## Different

### Queries

| Topic | Firestore | CandleDB | Why |
|---|---|---|---|
| Composite indexes | A query combining filters or orderings on several fields needs a composite index declared in advance; without it the query is rejected. | Never needed. Every query runs. | CandleDB scans one automatic single-field index and checks the remaining conditions document by document ([Queries](04-queries.md#how-queries-are-executed)). Simpler to use; the cost of a query is no longer bounded by its result size. |
| Performance guarantee | Query time is proportional to the result size, not the collection size. | True only for queries the chosen index delivers in final order (one ordering, or an equality filter). Others read all candidates. | The same trade as above, seen from the other side. |
| Range filter and ordering | A range filter implies ordering by that field first. | No connection. Without `orderBy`, results are in document ID order even when there is a range filter. | With a residual filter there is no reason to tie the two together. Add `orderBy` if you want that order. |
| Range filters on several fields | Restricted, and index-dependent. | Unrestricted. | Residual filtering. |
| `!=` and `not-in` | Exclude documents where the field is null, in most cases. | Match any document that has the field and does not equal the value, including null. | One uniform rule: the field must exist; then plain inequality. |
| Number of `in` values and filters | Has its own limits. | 30 values per list, 30 filters, 10 orderings. | |
| Collection-group queries | Supported. | Not supported. A query reads exactly one collection. | Not implemented. |
| `OR` between filters | Supported. | Not supported, except within one field through `in`. | Not implemented. |
| Aggregations (`count`, `sum`, `avg`) | Supported. | Not supported. | Not implemented. |
| Offset | Supported (and billed). | Not supported. Paginate with cursors. | |
| Result size | Unbounded, streamed. | At most 10,000 documents per query; larger unlimited results are an error. | The whole result is held in memory and sent as one JSON response. |
| Consistency across pages | | Each page is its own query against current data. | |

### Security rules

| Topic | Firestore | CandleDB | Why |
|---|---|---|---|
| Rules and queries | "Rules are not filters": a query is rejected unless the rules provably allow every possible result. | **Rules are filters**: a query returns the documents the caller may read and silently omits the rest. It is never rejected for permissions. | Proving a query safe needs static analysis candledb does not have, and rejecting after the fact would let callers probe for the existence of documents. See [Security rules](07-security-rules.md#rules-and-queries). |
| Paths in `get()` / `exists()` | A path literal: `/databases/$(database)/documents/users/$(uid)`. | A string: `"users/" + uid`. | Simpler; there is only one database. |
| File preamble | `rules_version`, `service cloud.firestore { match /databases/{database}/documents { ... } }`. | None. `match` blocks start at the top level. | |
| `getAfter()`, `existsAfter()` | See the state after the batch. | Not available. `get()` sees the state before the batch. | Not implemented. |
| Lookup limit | Its own limits per request. | 10 `get`/`exists` calls per evaluation. | |
| `{name=**}` | Can match zero segments in the current rules version. | Matches one or more segments. | |
| Function bodies | May use `let` bindings. | One `return` expression. | |
| Built-in library | Large: `math`, `timestamp`, `duration`, `latlng`, map `diff()`, sets, string `split`/`replace`, and more. | Small: `size`, `matches`, `lower`, `upper`, `trim`, `hasAll`, `hasAny`, `hasOnly`, `keys`, `values`, `get`; `in`; `is`. | Kept small on purpose; [the full list](07-security-rules.md#methods). |
| Map and timestamp literals, ternary `? :` | Available. | Not available. | |
| `request.time` versus server timestamps | A field written with a server timestamp equals `request.time`. | Same: for a write, `request.time` is the commit time. | See the [recipe](07-security-rules.md#write-once-fields-and-server-timestamps). |
| `request.query` (limit, orderBy) | Available for `list` rules. | Not available. | Rules see documents, not queries. |
| Errors in `&&` and `\|\|` | | An error on one side is ignored when the other side decides the result, in either order. | |
| Deployment | Uploaded through the console or CLI. | A file read at start and on `SIGHUP`; `candledb check-rules` validates it. | |

### Data types

| Topic | Firestore | CandleDB |
|---|---|---|
| Bytes, geographic point, document reference, vector | Supported. | Not available. Use a base64 string, a map of two numbers, a path string. |
| Timestamp precision | Microseconds in practice. | Microseconds, UTC. |
| NaN and infinity | Storable. | Rejected: they cannot be written in JSON. |
| Integer versus double | Typed on the wire. | Decided by how the number is written in JSON: `2` is an integer, `2.0` a double. The JS client sees both as a JavaScript number. |
| Mixed integer and double comparison | Numeric. | Numeric and exact, including integers beyond 2^53. |
| Field names | Almost any string; special characters quoted with backticks in field paths. | May not contain `.` or start with `$`. There is no quoting. |
| Timestamps in JSON | A typed field. | `{"$timestamp": "..."}`; a one-key object whose key starts with `$` is reserved. |

### Writes and transactions

| Topic | Firestore | CandleDB |
|---|---|---|
| Transactions | A server-side transaction; reads inside it are consistent with each other. | Assembled by the client from version preconditions and `verify` writes on one commit. Reads are separate requests and are *validated* at commit, not taken from one snapshot. |
| Read-only transactions | Supported. | A transaction that writes nothing sends nothing and so validates nothing. |
| Preconditions | `exists`, `updateTime`. | `exists`, `version`. |
| Document versions | `updateTime`. | An integer `version`, shared by the whole database and incremented per commit. |
| Write throughput | Scales out; per-document rate limits. | All commits pass through one writer, one at a time. |
| Deleting a collection | Tooling exists. | No operation; delete the documents. |

### Realtime and the client

| Topic | Firestore | CandleDB |
|---|---|---|
| Transport | A bidirectional gRPC or WebChannel stream. | A Server-Sent Events stream for messages, ordinary HTTP requests for adding and removing listeners. |
| Reconnection | Resumes from a resume token; only missed changes are sent. | The listener is re-sent its complete result. The client compares it with what it had and reports only real differences to your code. |
| Offline persistence | The client can cache data on disk, serve reads from it, and queue writes while offline. | None. State is in memory; a write made while the server is unreachable fails with `unavailable`. |
| Reads from cache | `get()` can be served locally. | `get()` always asks the server. |
| Change order information | Changes carry old and new index. | Changes carry no position; the client sorts. |
| Optimistic writes | Across tabs with multi-tab persistence. | Within one client object. Other tabs learn of the write from the server. |
| Slow listeners | | A session more than 256 commits behind is closed and must reconnect. |
| Listener count | | 100 listeners per stream, 10,000 streams per server. |
| Staleness under rules | | A listener's access is re-checked when its own documents change, not when a document consulted through `get()` changes. |

### Accounts

| Topic | Firebase Authentication | CandleDB |
|---|---|---|
| Sign-in methods | Email, phone, many identity providers, anonymous accounts. | Email and password only. |
| Email verification, password reset, account deletion | Built in. | Not available. |
| Custom tokens | Minted with a service account; RS256. | Any HS256 JWT signed with the server's secret; all non-standard claims appear in `request.auth.token`. |
| Admin access | Service accounts and IAM. | One shared admin key. |
| Token refresh | Handled by the SDK. | Handled by the SDK; refresh tokens are single-use and rotate. Tabs sharing a stored session pick up each other's refreshed tokens. |

### Operations

| Topic | Firestore | CandleDB |
|---|---|---|
| Availability | Replicated across zones or regions. | One process. No replication or failover. |
| Backups | Managed, with point-in-time recovery. | `candledb backup` writes a consistent file; restore by copying it into place. No point-in-time recovery. |
| Triggers, functions, extensions | Cloud Functions and others. | None. Trusted logic runs in your own process with the admin key. |
| Local emulator | A separate emulator. | The server itself; `-dev-open-rules` for convenience. |
| Cost model | Per read, write, and stored byte. | A machine. |

## Choosing

CandleDB fits when one machine is enough, you want to run the whole thing
yourself, and you value being able to read and change every part of it. It
is a poor fit when you need high availability, horizontal write scaling,
offline-first mobile clients, or the breadth of Firebase's surrounding
services.

Porting an app between the two is a rewrite of the data-access layer and the
rules, not a configuration change. The concepts transfer; the code does not.

This is the last page of the tour. The other two tracks, listed in the
[documentation index](../README.md), walk through the same system from the
inside: one teaches Go feature by feature, the other explains how the parts
fit together.
