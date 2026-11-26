# 4. Queries

This page describes how to select documents from a collection: the filter
operators and exactly what each one matches, ordering, limits, cursors and
pagination, and the cap on result size. It ends with a short account of how
the server executes a query, which explains what is fast and what is not.

## The shape of a query

A query reads from **one collection**. It is sent as JSON to
`POST /v1/query`:

```json
{
  "collection": "cities",
  "where":   [{"field": "country", "op": "==", "value": "DE"}],
  "orderBy": [{"field": "population", "direction": "desc"}],
  "limit":   10,
  "startAt": {"values": [2000000], "inclusive": false},
  "endAt":   {"values": [500000],  "inclusive": true}
}
```

Only `collection` is required. Unknown keys are rejected, so a misspelt
`limt` is an error rather than a silently unlimited query. The response is
`{"documents": [...]}` with whole documents, in query order.

The client builds the same thing by chaining:

```js
const snapshot = await db.collection("cities")
  .where("country", "==", "DE")
  .orderBy("population", "desc")
  .limit(10)
  .get();

snapshot.docs;     // array of document snapshots
snapshot.size;     // how many
snapshot.empty;    // true if none
snapshot.forEach((doc) => console.log(doc.id, doc.data()));
```

Each method returns a new query and leaves the one it was called on
unchanged, so a base query can be reused.

The examples on this page use these seven documents in `cities`:

| ID | name | country | population | tags |
|---|---|---|---|---|
| `ams` | Amsterdam | NL | 930000 | port, canals |
| `ber` | Berlin | DE | 3700000 | capital |
| `ham` | Hamburg | DE | 1900000 | port |
| `par` | Paris | FR | 2100000 | capital |
| `rot` | Rotterdam | NL | 660000 | port |
| `xxx` | Unknown | NL | `"n/a"` (a string) | *(no field)* |
| `yyy` | Nowhere | *(no field)* | *(no field)* | *(no field)* |

## Filters

Each entry of `where` is a **filter** with a `field` (a field path such as
`address.city`), an `op`, and a `value`. A document must pass every filter;
there is no "or" between filters.

Two rules apply to every operator and explain most surprises:

1. **The field must exist.** A document without the field never matches, not
   even for `!=` or `not-in`.
2. **Values are compared with the database's own ordering and equality**
   (see [The data model](02-data-model.md#sort-order)). `1` equals `1.0`.

| Operator | Matches when the field... | Result on the example |
|---|---|---|
| `==` | equals the value | `country == "NL"`: ams rot xxx |
| `!=` | exists and does not equal the value | `country != "NL"`: ber ham par |
| `<` `<=` `>` `>=` | is of the **same kind** as the value and compares accordingly | `population > 1000000`: ber ham par |
| `in` | equals any value in the given list | `country in ["FR","DE"]`: ber ham par |
| `not-in` | exists and equals none of the values in the list | `country not-in ["FR","DE"]`: ams rot xxx |
| `array-contains` | is a list that has an element equal to the value | `tags array-contains "port"`: ams ham rot |
| `array-contains-any` | is a list that has an element equal to any value in the given list | `tags array-contains-any ["capital","canals"]`: ams ber par |

Notes on the results:

- `yyy` has no `country` and so is in neither the `!=` nor the `not-in`
  result.
- `population > 1000000` does not return `xxx`, although strings sort after
  numbers. The four ordering operators only match values of the same kind as
  the filter value: numbers with numbers, strings with strings, and so on.
  `population <= 930000` likewise returns only ams and rot.
- `==`, `!=`, `in` and `not-in` do compare across kinds. A string is simply
  never equal to a number, so for `population != 5`, `xxx` matches.
- `== null` matches fields that exist and hold null. To find fields that are
  set to something, use `!= null`.
- Lists and maps can be compared as wholes: `tags == ["port"]` matches a
  list with exactly that content.

The lists given to `in`, `not-in` and `array-contains-any` need 1 to 30
values. A query may have up to 30 filters.

Filters can be combined freely, including several on different fields, and
ranges on a field other than the one the query is ordered by:

```sh
curl -s -X POST http://localhost:8090/v1/query -d '{"collection": "cities",
  "where": [{"field": "country",    "op": "==", "value": "DE"},
            {"field": "population", "op": ">",  "value": 2000000}]}'
```

This returns `ber`. No index has to be declared for any combination.

## Ordering

`orderBy` lists up to 10 fields, each ascending (`"asc"`, the default) or
descending (`"desc"`). The order of values is the database-wide order, so a
field holding mixed kinds sorts numbers before strings:

```text
orderBy population            ->  rot ams ham par ber xxx
orderBy population desc, limit 2  ->  xxx ber
```

Three rules complete the picture:

- **A document that lacks an ordered field is not in the result.** `yyy`
  appears in neither list above. Ordering by a field therefore also filters
  on its existence.
- **Ties are broken by document ID**, in the direction of the last
  `orderBy`. The order is therefore always total: the same query over the
  same data returns the same sequence.
- **Without `orderBy`, results are in ascending document ID order.** This
  holds even when the query has a range filter; the result is not implicitly
  sorted by the filtered field.

With two orderings the second decides among equal values of the first:

```text
orderBy country, population desc  ->  ber ham par xxx ams rot
```

Within `NL`, `xxx` comes first because its population is a string, and
strings are the largest kind present.

## Limit

`limit` cuts the result to the first *n* documents in query order. `0` or
absent means no limit.

Security rules are applied before the limit. If the rules hide some matching
documents from the caller, a limit of 10 still returns 10 readable documents
when that many exist (see [Security rules](07-security-rules.md#rules-and-queries)).

## Cursors and pagination

A **cursor** is a position in the ordered result. `startAt` says where the
result begins and `endAt` where it ends. Each has:

- `values`: one value per `orderBy` field, in order, optionally followed by
  a document ID (a string). Fewer values than orderings are allowed; then
  only those leading fields are compared.
- `inclusive`: whether a document exactly at the position is part of the
  result.

The four cursor methods of the client map onto these two fields:

| Client | Wire |
|---|---|
| `startAt(...values)` | `startAt`, `inclusive: true` |
| `startAfter(...values)` | `startAt`, `inclusive: false` |
| `endAt(...values)` | `endAt`, `inclusive: true` |
| `endBefore(...values)` | `endAt`, `inclusive: false` |

A cursor on values alone selects a range:

```js
// 930000 <= population < 2100000, by population
await db.collection("cities").orderBy("population").startAt(930000).endBefore(2100000).get();
// -> ams, ham
```

Adding the document ID pins the position to one document. That is what
pagination needs, because several documents can share the ordered value. To
fetch the next page, start after the last document of the previous page:

```sh
# Page 1
curl -s -X POST http://localhost:8090/v1/query -d '{"collection": "cities",
  "orderBy": [{"field": "country"}], "limit": 2}'
# -> ber ham        (the last one has country "DE" and ID "ham")

# Page 2
curl -s -X POST http://localhost:8090/v1/query -d '{"collection": "cities",
  "orderBy": [{"field": "country"}], "limit": 2,
  "startAt": {"values": ["DE", "ham"], "inclusive": false}}'
# -> par ams
```

In the client, pass the last snapshot itself and it extracts the values and
the ID:

```js
let page = db.collection("cities").orderBy("country").limit(3);
for (;;) {
  const result = await page.get();
  if (result.empty) break;
  result.forEach((doc) => console.log(doc.id));
  page = page.startAfter(result.docs[result.docs.length - 1]);
}
// ber ham par, then ams rot xxx
```

Without `orderBy` a cursor has exactly one value, the document ID:

```json
{"collection": "cities", "limit": 3, "startAt": {"values": ["ham"], "inclusive": false}}
```

returns `par rot xxx`.

Pagination by cursor does not slow down on later pages the way skipping by
offset does, because the server starts reading at the cursor position. There
is no offset parameter.

Each page is a separate query, run against the data as it is at that moment.
If documents are added or removed between two requests, the pages reflect
that; there is no snapshot held across pages.

## The result cap

One query may return at most **10,000 documents**.

- A `limit` above 10,000 is rejected as `invalid_argument` (400).
- A query without a limit whose result would exceed 10,000 documents fails
  with `resource_exhausted` (429) and the message "the query matches more
  than 10000 documents; add a limit and paginate". It is not silently cut
  short, because a truncated result that looks complete is worse than an
  error.

The same cap applies to the first snapshot of a listener.

## Errors

| Situation | Code | Status |
|---|---|---|
| Unknown operator, bad direction, bad cursor, misspelt key, document path given as collection | `invalid_argument` | 400 |
| `limit` above the cap | `invalid_argument` | 400 |
| Unlimited result above the cap | `resource_exhausted` | 429 |

A query over a collection that does not exist, or that the rules hide
entirely, is not an error. It returns an empty list.

```sh
curl -s -X POST http://localhost:8090/v1/query \
  -d '{"collection": "cities", "where": [{"field": "name", "op": "like", "value": "A%"}]}'
```

```json
{"error":{"code":"invalid_argument","message":"invalid argument: unknown operator \"like\""}}
```

## How queries are executed

You do not need this section to use queries, but it tells you what to expect
of their speed.

Every field of every document is indexed automatically: for each collection
and field there is a sorted list of (value, document ID) entries. This
includes nested fields under their dotted path, and each element of a list
(which is what `array-contains` searches). Nothing has to be declared.

To answer a query the server picks **one** index and scans a range of it.
Every other condition of the query is then checked against each document the
scan produces. The choice is:

| Query | What is scanned | Can stop at the limit? |
|---|---|---|
| has `orderBy` | the index of the first ordered field, in the requested direction, narrowed by filters and cursors on that field | yes, with one ordering; no, with several |
| no `orderBy`, has an `==` or `array-contains` filter | the entries equal to that value | yes |
| no `orderBy`, has `in` or `array-contains-any` | the entries of each listed value | no |
| no `orderBy`, has a range filter | the range | no |
| nothing usable (`!=`, `not-in` only, or no filter) | the collection in ID order | yes |

"Can stop at the limit" is the difference between cheap and expensive. Where
the scan already delivers documents in final order, the server stops reading
as soon as it has enough, so "the ten highest scores" costs the same in a
collection of a thousand documents as in one of a million. Where it does not,
every candidate is read and sorted before the limit applies.

Practical consequences:

- **Order by one field and limit** is the fast path for large collections.
- A filter on a field *other* than the ordered one is checked document by
  document along the scan. It is correct, but if few documents pass it, many
  are read to fill the limit.
- Two orderings, or `in` with a limit, read all candidates before returning
  the first one.
- `!=` and `not-in` never narrow the scan.

This differs from Firestore, which refuses a query that it cannot answer from
a single (possibly composite) index. CandleDB answers every query and leaves
the cost to you. [Design decisions](../architecture/03-design-decisions.md)
discusses the trade-off, and the planner itself is in
[`internal/adapters/sqlite/plan.go`](../../internal/adapters/sqlite/plan.go).

Next: [5. Realtime](05-realtime.md)
