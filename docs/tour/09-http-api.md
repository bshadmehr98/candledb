# 9. HTTP API

This page is the reference for the HTTP interface: conventions that apply to
every request, the error format and codes, the JSON shapes shared between
endpoints, and then each endpoint in turn. The earlier pages explain the
concepts; this one is for looking things up. The routes are defined in
[`internal/adapters/httpapi/server.go`](../../internal/adapters/httpapi/server.go).

## Conventions

| Topic | Rule |
|---|---|
| Base URL | wherever the server listens; the default port is 8090 |
| Request bodies | JSON. The `Content-Type` header is not checked. |
| Response bodies | JSON with `Content-Type: application/json; charset=utf-8`, except `/metrics` (text) and `/v1/listen` (event stream) |
| Unknown fields | rejected with `invalid_argument` in every request body that has a fixed shape (commit, query, target, account requests) |
| Body size | at most 10 MiB; more gives 413 `request_too_large` |
| Authentication | optional `Authorization: Bearer <token>`, where the token is an access token or the admin key |
| Request ID | every response has `X-Request-Id`; send the header yourself (up to 64 characters) to choose the value |
| CORS | preflight `OPTIONS` requests are answered for every path; see [Operations](08-operations.md#cors) |

### Authentication outcomes

| Request has | Result |
|---|---|
| no `Authorization` header | proceeds as an anonymous caller |
| a valid access token | proceeds as that user |
| the admin key | proceeds as admin; security rules are skipped |
| anything else (wrong scheme, malformed, forged or expired token) | 401 `unauthenticated`, on every endpoint including `/healthz` |

## Errors

Every error response has this body:

```json
{"error": {"code": "not_found", "message": "not found: document users/nobody"}}
```

`code` is stable and meant for programs. `message` is meant for people and
may change. When a batch of several writes fails, the message names the
failing write by its position in the batch, counting from zero
(`write 1 (update accounts/missing): ...`). A request with a single write
has no such prefix.

| Code | Status | Meaning |
|---|---|---|
| `invalid_argument` | 400 | malformed JSON, path, data, query or parameter |
| `unauthenticated` | 401 | bad credentials; wrong email or password; invalid refresh token; endpoint needs a signed-in user |
| `permission_denied` | 403 | the security rules refuse the operation; sign-up is disabled; `/metrics` without the admin key |
| `not_found` | 404 | no such document, user or listen session |
| `already_exists` | 409 | `create` of an existing document; email already registered; listen target ID in use |
| `session_closed` | 410 | the listen session has ended |
| `failed_precondition` | 412 | a precondition of a write did not hold |
| `request_too_large` | 413 | request body above the limit |
| `resource_exhausted` | 429 | rate limit; query result above 10,000 documents; too many sessions or targets |
| `unavailable` | 503 | the server cannot serve the request right now |
| `internal` | 500 | a fault in the server. The message is always `internal error`; the cause is in the server log under the request ID. |

Requests that match no route are answered by the HTTP library, not by this
format: 404 for an unknown path and 405 for a known path with the wrong
method, both with a plain-text body.

## Shared shapes

### Values

Field values are JSON values with two refinements, described in
[The data model](02-data-model.md#value-types):

- A number without fraction or exponent is an integer; otherwise a double. A
  double is always written back with a fraction or exponent (`2.0`).
- A timestamp is `{"$timestamp": "<RFC 3339>"}`. On output it is UTC with
  six fractional digits.

### Document

```json
{
  "path": "users/alice",
  "id": "alice",
  "data": {"name": "Alice"},
  "createTime": "2026-11-23T22:06:21.163176Z",
  "updateTime": "2026-11-23T22:06:35.991123Z",
  "version": 6
}
```

### Commit result

```json
{"version": 9, "commitTime": "2026-11-23T22:06:36.069565Z"}
```

`version` is the database version the commit produced; every document it
wrote carries it. `POST /v1/documents/{collection}` adds `path` and `id` of
the created document.

### Write

One element of a commit's `writes`:

```json
{
  "op": "update",
  "path": "accounts/a",
  "data": {"balance": {"$increment": -30}},
  "merge": false,
  "precondition": {"exists": true, "version": 7}
}
```

| Field | Rule |
|---|---|
| `op` | `set`, `create`, `update`, `delete` or `verify` |
| `path` | a document path |
| `data` | a JSON object; required for `set`, `create` and `update`, forbidden for `delete` and `verify` |
| `merge` | only with `set`: merge into the stored document instead of replacing it |
| `precondition` | optional; `exists` (bool) and/or `version` (integer) |

How `data` is read depends on the operation:

| Operation | Keys | Nested maps |
|---|---|---|
| `set`, `create` | field names | stored as given |
| `set` with `merge` | field names | merged recursively with what is stored; an empty map is written as a value |
| `update` | field paths (`address.city`) | each value replaces that field as a whole |

Transforms, as values inside `data`:

| Form | Allowed in |
|---|---|
| `{"$serverTimestamp": true}` | all |
| `{"$increment": <number>}` | all |
| `{"$arrayUnion": [<values>]}` | all |
| `{"$arrayRemove": [<values>]}` | all |
| `{"$delete": true}` | `update` and `set` with `merge` |

In `set`, `create` and merge they may appear at any depth inside maps; in
`update` only as the direct value of a key; never inside a list. See
[Reading and writing](03-reading-and-writing.md#transforms) for their exact
effect.

### Query

```json
{
  "collection": "cities",
  "where":   [{"field": "country", "op": "==", "value": "DE"}],
  "orderBy": [{"field": "population", "direction": "desc"}],
  "limit": 10,
  "startAt": {"values": [2000000, "ham"], "inclusive": false},
  "endAt":   {"values": [500000], "inclusive": true}
}
```

| Field | Rule |
|---|---|
| `collection` | required; a collection path |
| `where` | up to 30 filters; `field` is a field path; `op` is one of `==` `!=` `<` `<=` `>` `>=` `in` `not-in` `array-contains` `array-contains-any`; `value` is required (a list of 1 to 30 values for `in`, `not-in`, `array-contains-any`) |
| `orderBy` | up to 10 entries; `direction` is `asc` (default) or `desc`; a field may appear once |
| `limit` | 0 to 10,000; 0 or absent means no limit |
| `startAt`, `endAt` | `values`: 1 to (number of orderings + 1) values; the extra last one is a document ID and must be a string. `inclusive` defaults to `false`. |

Semantics are on the [Queries](04-queries.md) page.

## Documents

For all of these, `{path}` is a document path and may contain slashes.
Security rules apply unless the admin key is used.

### `GET /v1/documents/{path}`

Returns the [document](#document).

| Status | When |
|---|---|
| 200 | found and readable |
| 403 `permission_denied` | the rules deny `get`, whether or not the document exists |
| 404 `not_found` | reading is allowed and there is no such document |
| 400 `invalid_argument` | not a valid document path |

### `PUT /v1/documents/{path}`

Sets the document to the request body (a JSON object). With the query
parameter `?merge=true`, merges the body into the stored document. Returns a
[commit result](#commit-result). Rule method: `create` or `update`.

### `PATCH /v1/documents/{path}`

Updates the fields named in the body; keys are field paths. Returns a commit
result. 404 `not_found` if the document does not exist. Rule method:
`update`.

### `DELETE /v1/documents/{path}`

Deletes the document. No body. Returns a commit result, also when there was
nothing to delete. Rule method: `delete`.

### `POST /v1/documents/{collection}`

Creates a document with a server-generated ID (26 characters) in the
collection. The body is the data. Returns a commit result with `path` and
`id`. Rule method: `create`.

```json
{"version":7,"commitTime":"2026-11-23T22:06:36.041190Z",
 "path":"users/alice/posts/35D6KDYBSQODLNIYOVCF3FWC6G","id":"35D6KDYBSQODLNIYOVCF3FWC6G"}
```

## Commit

### `POST /v1/commit`

Applies 1 to 500 writes atomically.

```json
{"writes": [ <write>, <write>, ... ]}
```

Returns a [commit result](#commit-result). On any failure nothing is applied
and the version does not advance.

| Status | When |
|---|---|
| 200 | every write was applied |
| 400 `invalid_argument` | empty or oversized batch; malformed write; resulting document too large or too deep |
| 403 `permission_denied` | the rules deny one of the writes |
| 404 `not_found` | an `update` of a missing document |
| 409 `already_exists` | a `create` of an existing document |
| 412 `failed_precondition` | a precondition did not hold |

Writes apply in order, and later writes see the effect of earlier ones in
the same batch. Preconditions and rules are evaluated per write, against the
document as it stands at that point in the batch; a rule's `get()` sees the
state before the batch.

## Query

### `POST /v1/query`

The body is a [query](#query). Returns the matching documents the caller may
`list`, in query order:

```json
{"documents": [ <document>, ... ]}
```

| Status | When |
|---|---|
| 200 | always for a valid query, including an empty result |
| 400 `invalid_argument` | malformed query; `limit` above 10,000 |
| 429 `resource_exhausted` | no limit given and more than 10,000 documents match |

## Realtime

The protocol is explained on the [Realtime](05-realtime.md) page.

### `GET /v1/listen`

Opens a listen session. The response is `text/event-stream` and stays open.
Each message is a line `data: <json>` followed by an empty line. Lines
starting with `:` are keep-alives, sent every 20 seconds.

| Message | Shape |
|---|---|
| ready | `{"type":"ready","session":"<id>"}`; always first |
| snapshot | `{"type":"snapshot","target":<id>,"version":<n>,"changes":[...]}`; the complete current result of a target |
| change | `{"type":"change","target":<id>,"version":<n>,"changes":[...]}` |
| error | `{"type":"error","code":"<code>","message":"..."}`; the stream closes after it |

An element of `changes` is `{"kind":"added","doc":<document>}`,
`{"kind":"modified","doc":<document>}` or `{"kind":"removed","path":"<path>"}`.
An empty `changes` list is omitted from the message.

Stream error codes: `session_closed` (server shutdown, or the client fell
behind) and `unauthenticated` (the access token the stream was opened with
has expired).

If the session cannot be opened the response is a normal error instead of a
stream: 401 for bad credentials, 429 `resource_exhausted` when the server
already has 10,000 sessions.

### `POST /v1/listen/{session}/targets`

Adds a listener to the session. The request must carry the same credentials
as the stream.

```json
{"id": 1, "document": "users/alice"}
{"id": 2, "query": { ...a query... }}
```

`id` is an integer chosen by the client, unique within the session. Exactly
one of `document` and `query` must be given. Answers `204 No Content`; the
snapshot then arrives on the stream.

| Status | When |
|---|---|
| 204 | added |
| 400 `invalid_argument` | neither or both of `document` and `query`; invalid path or query |
| 403 `permission_denied` | `document` target the caller may not `get` |
| 404 `not_found` | unknown session, or one opened with other credentials |
| 409 `already_exists` | the `id` is in use on this session |
| 410 `session_closed` | the session has ended |
| 429 `resource_exhausted` | the session already has 100 targets; the query matches more than 10,000 documents |

### `DELETE /v1/listen/{session}/targets/{id}`

Removes a listener. Answers `204 No Content`, also when the ID is unknown.
404 for an unknown or foreign session; 400 if `{id}` is not a number.

## Accounts

Explained on the [Accounts](06-auth.md) page. Sign-up, sign-in and refresh
are rate limited per client address (429 `resource_exhausted` with a
`Retry-After` header).

### Session response

```json
{
  "user": {"id": "VIOT2ZUZ...", "email": "ada@example.com", "createdAt": "2026-11-23T22:07:24.673374Z"},
  "accessToken": "eyJhbGciOi...",
  "refreshToken": "LWRFXLGH...",
  "expiresAt": "2026-11-23T23:07:24.673547Z"
}
```

`expiresAt` is when the access token expires. The response has
`Cache-Control: no-store`.

### `POST /v1/auth/signup`

Body `{"email": "...", "password": "..."}`. Creates an account and returns a
session response.

| Status | When |
|---|---|
| 200 | created |
| 400 `invalid_argument` | not an email address; password not 8 to 72 bytes |
| 403 `permission_denied` | the server runs with `-disable-signup` |
| 409 `already_exists` | the email is registered |

### `POST /v1/auth/signin`

Body as for sign-up. Returns a session response, or 401 `unauthenticated`
with the same message for a wrong password and an unknown email.

### `POST /v1/auth/refresh`

Body `{"refreshToken": "..."}`. Returns a session response with a new access
token and a new refresh token; the token sent is no longer valid. 401
`unauthenticated` for a used, expired or unknown token.

### `POST /v1/auth/signout`

Body `{"refreshToken": "..."}`. Revokes the refresh token. Answers
`204 No Content`, also for an unknown token.

### `GET /v1/auth/me`

Returns the caller's account:

```json
{"id":"VIOT2ZUZIFQXJYLJSVQ2DMAX26","email":"ada@example.com","createdAt":"2026-11-23T22:07:24.673374Z"}
```

401 `unauthenticated` for an anonymous caller or the admin key; 404
`not_found` for a valid token whose subject is not a built-in account.

## Operational endpoints

Described in [Operations](08-operations.md#health-checks).

| Endpoint | Auth | Response |
|---|---|---|
| `GET /healthz` | none | 200 `{"status":"ok"}` |
| `GET /readyz` | none | 200 `{"status":"ok"}`, or 503 `{"status":"unavailable"}` when the database does not answer |
| `GET /metrics` | admin key | 200, Prometheus text format; 403 `permission_denied` otherwise |

## Summary of routes

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v1/documents/{path}` | read a document |
| `PUT` | `/v1/documents/{path}` | set; `?merge=true` to merge |
| `PATCH` | `/v1/documents/{path}` | update fields |
| `DELETE` | `/v1/documents/{path}` | delete |
| `POST` | `/v1/documents/{collection}` | add with a generated ID |
| `POST` | `/v1/commit` | atomic batch of writes |
| `POST` | `/v1/query` | run a query |
| `GET` | `/v1/listen` | open the realtime stream |
| `POST` | `/v1/listen/{session}/targets` | add a listener |
| `DELETE` | `/v1/listen/{session}/targets/{id}` | remove a listener |
| `POST` | `/v1/auth/signup` | create an account |
| `POST` | `/v1/auth/signin` | sign in |
| `POST` | `/v1/auth/refresh` | renew tokens |
| `POST` | `/v1/auth/signout` | revoke a refresh token |
| `GET` | `/v1/auth/me` | the signed-in user |
| `GET` | `/healthz` | liveness |
| `GET` | `/readyz` | readiness |
| `GET` | `/metrics` | Prometheus metrics |

Next: [10. Differences from Firestore](10-differences-from-firestore.md)
