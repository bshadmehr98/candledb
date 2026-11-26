# 2. The data model

This page describes what the database stores: documents, the collections
that hold them, the types a field can have and how each is written in JSON,
the metadata the server keeps for every document, and the limits on all of
it. The numbers come from
[`internal/domain`](../../internal/domain/valuejson.go), where they are
defined.

## Documents and collections

A **document** is a set of named fields, like a JSON object. A **collection**
is a set of documents. Every document lives in exactly one collection and has
an **ID** that is unique within it.

There is no schema. Two documents in the same collection can have entirely
different fields, and a field can hold a number in one document and a string
in another.

Collections are implicit. You never create or delete one: a collection exists
while it contains at least one document.

## Paths

A document is addressed by its **path**: the collection name, a slash, and
the document ID.

```text
users/alice
```

A document can have collections of its own, called **subcollections**. Their
paths continue from the document's path:

```text
users/alice/posts            a collection
users/alice/posts/p1         a document in it
users/alice/posts/p1/likes   a collection under that document
```

Paths therefore alternate: collection, document, collection, document. A path
with an odd number of segments names a collection and one with an even number
names a document. The server checks this and says which one it expected:

```sh
curl -s http://localhost:8090/v1/documents/users
```

```json
{"error":{"code":"invalid_argument","message":"invalid argument: \"users\" is a collection path, not a document path"}}
```

A subcollection is independent of its parent document. Deleting
`users/alice` does not delete `users/alice/posts/p1`, and a subcollection can
exist under a document that was never written. A query on `users` never
returns documents of `users/alice/posts`.

### Rules for path segments

| Rule | Limit |
|---|---|
| Length of one segment | 1 to 1500 bytes |
| Number of segments in a path | at most 100 |
| Forbidden segments | `.` and `..` |
| Reserved segments | anything of the form `__name__` (starts and ends with two underscores) |
| Forbidden characters | `/` inside a segment, control characters, invalid UTF-8 |

Spaces and non-ASCII characters are allowed. In a URL they must be
percent-encoded, as usual.

## Value types

A field holds one **value**. There are seven kinds:

| Kind | JSON form | Notes |
|---|---|---|
| null | `null` | A field that exists and holds null is different from a missing field. |
| bool | `true`, `false` | |
| number | `42`, `-3`, `1.5`, `1e30` | Integers and doubles are distinct; see below. |
| timestamp | `{"$timestamp": "2026-03-04T05:06:07.123456Z"}` | An instant, in UTC, to the microsecond. |
| string | `"text"` | UTF-8. |
| list | `[1, "two", [3]]` | Ordered; elements may be of any kind, including lists and maps. |
| map | `{"city": "Paris"}` | Named fields; may nest. |

There are no other types. In particular there is no binary type, no
geographic point and no reference type. Store binary data as a base64 string
and a reference as the path string of the document it points to.

### Integers and doubles

JSON has a single number type, but candledb keeps two: 64-bit integers and
64-bit floating-point numbers ("doubles").

- A number written without a fraction or exponent is an **integer**: `7`,
  `-3`, `9007199254740993`. Integers are exact over the full 64-bit range.
- A number written with a fraction or exponent is a **double**: `7.0`,
  `1.5`, `1e30`.
- An integer too large for 64 bits is read as a double.
- `NaN` and infinities cannot be stored, because JSON cannot carry them.

A double keeps its decimal point when it is sent back, so `2.0` is returned
as `2.0` and not as `2`.

For comparing, sorting and filtering, the two are one kind: `1` equals `1.0`,
and a query for `age > 20` matches `20.5` and `21` alike. The comparison is
exact even for integers too large to be represented precisely as a double.

> JavaScript has only doubles. The JS client reads every number as a
> JavaScript number, so an integer above 2^53 loses precision in the browser
> even though the server stores it exactly. Use a string for such values if
> the frontend must show them exactly.

### Timestamps

JSON has no time type, so a timestamp is written as an object with the single
key `$timestamp` and an RFC 3339 string. Any offset and up to nanosecond
precision are accepted on input. The server converts to UTC and cuts the
value to microseconds:

```sh
curl -s -X PUT http://localhost:8090/v1/documents/types/demo -d '{
  "nothing": null, "yes": true,
  "int": 9007199254740993, "double": 2.0, "big": 1e30,
  "text": "héllo",
  "when": {"$timestamp": "2026-03-04T05:06:07.123456789+02:00"},
  "list": [1, "two", [3]],
  "map": {"nested": {"deep": true}}
}'
curl -s http://localhost:8090/v1/documents/types/demo
```

```json
{"path":"types/demo","id":"demo",
 "data":{"big":1e+30,"double":2.0,"int":9007199254740993,
         "list":[1,"two",[3]],"map":{"nested":{"deep":true}},
         "nothing":null,"text":"héllo",
         "when":{"$timestamp":"2026-03-04T03:06:07.123456Z"},"yes":true},
 "createTime":"2026-11-23T22:06:36.124172Z",
 "updateTime":"2026-11-23T22:06:36.124172Z","version":11}
```

The JS client converts in both directions: a `Date` you write is stored as a
timestamp, and a timestamp you read arrives as a `Date`. A `Date` has
millisecond precision, so the last three digits are lost on the way into
JavaScript.

### Field names

| Rule | Limit |
|---|---|
| Length | 1 to 1500 bytes |
| May not contain | `.` |
| May not start with | `$` |

The dot is reserved because it separates the parts of a **field path**:
`address.city` means "the field `city` inside the map `address`". Field paths
appear in updates, queries and rules. The dollar sign is reserved for typed
values such as `$timestamp` and for write transforms such as `$increment`
(see [Reading and writing](03-reading-and-writing.md#transforms)).

```sh
curl -s -X PUT http://localhost:8090/v1/documents/types/bad -d '{"a.b": 1}'
```

```json
{"error":{"code":"invalid_argument","message":"invalid argument: field name \"a.b\" contains a dot"}}
```

## Document metadata

Every document the server returns has this shape:

| Field | Meaning |
|---|---|
| `path` | The document's full path. |
| `id` | The last segment of the path. |
| `data` | Your fields. |
| `createTime` | When the document was first written. It survives later writes, but deleting and re-creating the document resets it. |
| `updateTime` | When the document was last written. |
| `version` | The database version of the commit that last wrote the document. |

Times in metadata are plain RFC 3339 strings with six fractional digits (they
are not wrapped in `$timestamp`, because their type is fixed).

### Versions

The database has one counter, the **version**, which goes up by one with
every successful commit, whatever it touches. A commit is one write or one
batch of writes. Each document remembers the version of the commit that last
changed it.

Versions therefore give a total order over all writes. They are used in two
places:

- A write can be made conditional on a document still being at the version
  you read. This is how transactions work; see
  [Reading and writing](03-reading-and-writing.md#preconditions).
- Every realtime message carries the version it brings the listener up to;
  see [Realtime](05-realtime.md).

A commit that fails does not consume a version.

## Sort order

Queries can order by any field, and a field may hold different kinds of value
in different documents, so there is one order across all values. Values of
different kinds sort by kind, in this order:

```text
null  <  bool  <  number  <  timestamp  <  string  <  list  <  map
```

Within a kind:

| Kind | Order |
|---|---|
| bool | `false` before `true` |
| number | numeric value; integers and doubles compare with each other |
| timestamp | chronological |
| string | by UTF-8 bytes. This is code point order: `"B"` sorts before `"a"`, and `"é"` after `"z"`. It is not alphabetical order in any language. |
| list | element by element; if one list is a prefix of the other, the shorter sorts first |
| map | by the sorted field names and then the values, pair by pair; fewer fields sort first when all pairs are equal |

Document IDs are compared the same way as strings.

Equality follows the same rules: two values are equal when neither sorts
before the other. `1` equals `1.0`; `"1"` equals neither.

## Limits on a document

| Limit | Value |
|---|---|
| Document size | about 1 MiB (1,048,576 bytes) |
| Nesting depth of lists and maps | 20 levels |
| Field name length | 1500 bytes |

The size is an estimate proportional to the encoded size, not the exact
length of the JSON: roughly one byte per null or bool, eight per number or
timestamp, the byte length plus one for a string, and the lengths of field
names. A request body is also limited, to 10 MiB, whatever it contains.

Limits on queries, batches and listeners are listed on the pages that cover
them and collected in [Operations](08-operations.md#limits).

Next: [3. Reading and writing](03-reading-and-writing.md)
