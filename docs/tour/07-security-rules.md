# 7. Security rules

This page is the reference for the rules language: the file in which you say
who may read and write which documents. It covers the structure of a rules
file, how a request is matched and decided, every operator, function and
method of the expression language, how rules apply to queries and listeners,
some common recipes, and how to check and reload a rules file.

## Why rules exist

Clients talk to the database directly, and a client cannot be trusted: anyone
can open the browser's developer tools and send any request. The rules run on
the server, for every single read and write, and are the only access control
there is. An app's rules file is its backend logic.

The language resembles Firestore's rules but is not identical; see
[Differences from Firestore](10-differences-from-firestore.md).

## A first example

```text
// Each user owns the documents under their own ID.
match /users/{uid} {
  allow read:  if request.auth != null;
  allow write: if request.auth != null && request.auth.uid == uid;
}
```

Read it as: for a document whose path has the form `users/<something>`, call
that something `uid`; anyone signed in may read it; only the user whose ID
equals `uid` may write it.

## Where the rules come from

The server loads `<data-dir>/candledb.rules` at start, or the file named by
`-rules`. Three situations are possible:

| Situation | Effect |
|---|---|
| A rules file exists | It is enforced. If it does not parse, the server refuses to start. |
| No rules file | **Everything is denied**, and the server logs a warning. |
| `-dev-open-rules` | Everything is allowed. For local development only. |

Requests made with the [admin key](06-auth.md#the-admin-key) skip the rules
in every case.

## How a request is decided

1. Every operation is on one document and has one **method** (below).
2. The server finds every `match` block whose path pattern matches the
   document's path.
3. The request is **allowed if at least one `allow` statement** in those
   blocks lists the method and has a condition that evaluates to `true`.
4. Otherwise it is denied.

So access is denied by default, statements only ever add permissions, and
there is no way to write a "deny" that overrides an "allow" elsewhere.

### Methods

| Method | Operation |
|---|---|
| `get` | reading one document by its path; also a document listener |
| `list` | a document being returned by a query or a query listener |
| `create` | a write to a document that does not exist |
| `update` | a write to a document that exists |
| `delete` | deleting a document |
| `read` | shorthand for `get, list` |
| `write` | shorthand for `create, update, delete` |

Which write method applies depends on the stored state, not on the request:
a `PUT` is a `create` the first time and an `update` afterwards. A
`verify` in a batch counts as a `get`.

## Structure of a rules file

A file is a sequence of `match` blocks and `function` definitions. Comments
start with `//` and run to the end of the line.

```text
match <path pattern> {
  allow <methods>: if <condition>;
  allow <methods>;                    // no condition: always allowed

  function name(params) { return <expression>; }

  match <relative path pattern> { ... }
}
```

### Path patterns

A pattern starts with `/` and consists of segments:

| Segment | Matches | Example |
|---|---|---|
| a literal | exactly that segment | `/users` |
| `{name}` | any one segment, and binds it to the variable `name` | `/users/{uid}` |
| `{name=**}` | all remaining segments, one or more, bound to `name` as a string with slashes | `/files/{rest=**}` |

A pattern must match the document's **entire** path. `match /users/{uid}`
matches `users/alice` and nothing deeper: it does not cover
`users/alice/posts/p1`.

Wildcard values are strings. In `match /users/{uid}`, a request for
`users/alice` evaluates the conditions with `uid == "alice"`.

A `{name=**}` wildcard must be the last segment, and a block that uses one
cannot contain nested `match` blocks.

### Nested blocks

A `match` inside another continues the path. These two forms are equivalent:

```text
match /users/{uid} {
  match /posts/{post} { allow read; }
}

match /users/{uid}/posts/{post} { allow read; }
```

Nesting has two uses: the inner block can use the outer block's wildcards and
functions, and related rules stay together. It grants nothing by itself. The
`allow` statements of the outer block apply only to documents matching the
outer pattern exactly, never to the subcollections.

To cover a document and everything below it, write a second block with a
recursive wildcard:

```text
match /users/{uid}             { allow read, write: if request.auth.uid == uid; }
match /users/{uid}/{rest=**}   { allow read, write: if request.auth.uid == uid; }
```

Several blocks may match the same document. Their permissions add up.

## The expression language

A condition is an expression that must produce the boolean `true`. Values
are the database's own types (see [The data model](02-data-model.md)): null,
bool, int, float, timestamp, string, list and map.

### Literals

| Literal | Examples |
|---|---|
| null, booleans | `null`, `true`, `false` |
| integers and floats | `42`, `1.5` |
| strings | `"text"` or `'text'`; escapes `\n`, `\t`, `\r`, `\\`, `\"`, `\'` |
| lists | `[1, "two", request.auth.uid]` |

Any other backslash sequence in a string is kept as written, so a regular
expression such as `"\d+"` needs no doubling. There is no literal for maps or
timestamps.

### Variables

| Variable | Value |
|---|---|
| `request.auth` | `null` for an anonymous caller, otherwise a map with `uid` and `token` |
| `request.auth.uid` | the caller's user ID, a string |
| `request.auth.token` | a map of the access token's claims; `email` for built-in accounts, plus any [custom claims](06-auth.md#bringing-your-own-tokens) |
| `request.method` | `"get"`, `"list"`, `"create"`, `"update"` or `"delete"` |
| `request.path` | the document's path, e.g. `"users/alice"` |
| `request.time` | the server's time, a timestamp |
| `request.resource` | for `create` and `update`: the document **as it would be after the write**; otherwise `null` |
| `resource` | the document **as it is stored now**; `null` if it does not exist |
| wildcards | each `{name}` of the enclosing patterns, as a string |

A document, in `resource`, `request.resource` and the result of `get()`, is
a map with three fields:

| Field | Value |
|---|---|
| `data` | the document's fields |
| `id` | the document ID |
| `path` | the full path |

`request.resource.data` is the complete result of the write, not just the
fields the request sent. After an update of one field, it holds every field
of the document, with transforms such as `$increment` already applied. A
rule can therefore compare `request.resource.data.owner` with
`resource.data.owner` to forbid changing the owner.

What is `null` when:

| Method | `resource` | `request.resource` |
|---|---|---|
| `get`, `list` | the document, or `null` when a missing document is fetched | `null` |
| `create` | `null` | the new document |
| `update` | the stored document | the new document |
| `delete` | the stored document, or `null` if there is none | `null` |

### Accessing fields

| Form | Meaning |
|---|---|
| `x.name` | field `name` of the map `x` |
| `x["name"]` | the same, with a computed key |
| `list[0]` | element of a list, counting from zero |

Reading a field that does not exist, a field of something that is not a map,
or an index outside the list is an **error**, not `null`. See
[Errors](#errors) for what that means. To test for a field first, use `in`;
to read with a fallback, use `.get(key, fallback)`.

### Operators

From loosest to tightest binding; operators on one row bind equally and
group left to right:

| Operators | Operands | Result |
|---|---|---|
| `\|\|` | bools | or |
| `&&` | bools | and |
| `==` `!=` | any | equality by the database's rules; `1 == 1.0`, and values of different kinds are simply unequal |
| `<` `<=` `>` `>=` | two values of the same kind | ordering; comparing different kinds is an error |
| `in` | value and list; string and map | whether the list has an equal element; whether the map has that key |
| `is` | value and a type name | type test |
| `+` `-` | numbers; `+` also joins two strings or two lists | |
| `*` `/` `%` | numbers | integer operands give an integer (`7 / 2 == 3`); otherwise a float |
| `!` `-` (prefix) | bool; number | not; negation |

Parentheses group as usual. Dividing an integer by zero is an error, as is
any arithmetic result that is not a finite number.

Type names for `is`: `null`, `bool`, `int`, `float`, `number` (either of the
two), `timestamp`, `string`, `list`, `map`.

```text
request.resource.data.title is string
request.resource.data.price is number
```

### Errors

An expression can fail: a missing field, a comparison of a string with a
number, a method on the wrong type. **A condition that fails counts as
`false`.** The request is not rejected with a special error; that `allow`
statement just does not grant anything.

This is the safe direction, and it is why the first example needs no null
check: for an anonymous caller `request.auth.uid` fails, so the condition is
false.

`&&` and `||` evaluate left to right and stop as soon as the result is known.
They are also **forgiving**: an error on one side does not matter if the
other side alone decides the result.

| Expression | Result |
|---|---|
| `<error> \|\| true` | `true` |
| `true \|\| <error>` | `true` (the right side is not evaluated) |
| `<error> \|\| false` | error, so the condition is false |
| `<error> && false` | `false` |
| `<error> && true` | error, so the condition is false |

The practical effect is that the order of the two sides never changes the
outcome, only the cost. `resource == null || resource.data.public == true`
is correct either way round.

A condition whose value is not a bool (say, a number) also counts as false.

### Methods

Methods are called on a value with a dot. Calling a method on a value of the
wrong type is an error.

| On | Method | Result |
|---|---|---|
| string | `.size()` | number of characters (not bytes) |
| string | `.lower()`, `.upper()` | case-converted copy |
| string | `.trim()` | copy without leading and trailing whitespace |
| string | `.matches(pattern)` | whether the **whole** string matches the regular expression |
| list | `.size()` | number of elements |
| list | `.hasAll(list)` | whether every element of the argument is in the list |
| list | `.hasAny(list)` | whether at least one element of the argument is in the list |
| list | `.hasOnly(list)` | whether every element of the list is in the argument |
| map | `.size()` | number of fields |
| map | `.keys()` | list of field names, sorted |
| map | `.values()` | list of values, in the order of the sorted field names |
| map | `.get(key, fallback)` | the field, or `fallback` if it is absent |

Regular expressions use Go's RE2 syntax, which has no backreferences or
lookaround and always runs in time proportional to the input, so a pattern
cannot be crafted to stall the server. A pattern may be at most 1000
characters. `.matches` is anchored at both ends: `"hello".matches("h")` is
false, `"hello".matches("h.*")` is true.

`keys()` with `hasOnly` and `hasAll` is the standard way to validate the
shape of a document:

```text
request.resource.data.keys().hasOnly(["title", "done", "created"])   // no other fields
request.resource.data.keys().hasAll(["title", "done"])               // these are required
```

### Looking up other documents

| Function | Result |
|---|---|
| `get(path)` | the document at `path` (a map with `data`, `id`, `path`), or `null` if it does not exist |
| `exists(path)` | whether a document exists at `path` |

The path is a string, without a leading slash, usually built by
concatenation:

```text
get("roles/" + request.auth.uid).data.role == "admin"
exists("rooms/" + room + "/members/" + request.auth.uid)
```

These lookups ignore the rules themselves; they are the server reading its
own data. Points to know:

- One evaluation may make at most **10** lookups. An eleventh is an error.
- In a batch, lookups see the data as it was **before** the batch, even for
  documents the same batch writes.
- Each lookup is a database read. In a query, the condition is evaluated for
  every candidate document, so a `list` rule that calls `get()` multiplies
  the cost of the query. Where possible, put the deciding data in the
  document itself or in a token claim.

### Functions

A function names an expression so that it can be reused:

```text
function signedIn() {
  return request.auth != null;
}

function isOwner(uid) {
  return signedIn() && request.auth.uid == uid;
}
```

- The body is exactly one `return <expression>;`.
- A function defined at the top level is visible everywhere. One defined
  inside a `match` block is visible in that block and the blocks nested in
  it, and its body can use that block's wildcards.
- The body sees `request`, `resource`, the wildcards in scope where the
  function is **defined**, and its own parameters.
- A function can call other functions, to a depth of 16 calls. Deeper
  nesting, which in practice means unbounded recursion, is an error.
- A function cannot be defined twice in the same scope, and cannot be named
  `get` or `exists`.

### Checked when loaded

When a rules file is loaded, every name is checked. An unknown variable,
function or method, a wrong number of arguments, or an unknown type after
`is` is reported with its line and column, and the file is rejected. A typo
therefore cannot silently deny everything in production.

## Rules and queries

For a single document, the rules give a yes or a no. For a query they act as
a **filter**:

> A query returns the matching documents that the caller is allowed to
> `list`. Documents the caller may not list are left out, as if they did not
> exist.

Given these rules and two notes owned by Ada, one of them public:

```text
match /notes/{id} {
  allow read: if resource == null || resource.data.public == true
              || (request.auth != null && resource.data.owner == request.auth.uid);
}
```

the same query returns different results for different callers, and no
error for either:

```text
POST /v1/query {"collection": "notes"}     anonymous  ->  n2
POST /v1/query {"collection": "notes"}     as Ada     ->  n1 n2
```

Details:

- The filter is applied **before the limit**. `limit: 10` returns ten
  readable documents if there are that many.
- A query never fails with `permission_denied`. A collection the caller may
  not read at all is indistinguishable from an empty one.
- The `list` condition is evaluated once per candidate document, with
  `resource` set to that document.

**Why filter instead of rejecting?** Firestore rejects a query unless it can
prove, from the query alone, that every possible result would be allowed.
That proof needs static analysis of rules against queries, which candledb
does not have. The obvious cheaper alternative, "run the query and fail if
any result is forbidden", has a flaw: it turns the error into a signal. A
caller could ask for `where ssn == "123-45-6789"` on a collection they may
not read and learn from "empty result" versus "permission denied" whether
such a document exists. Filtering leaks nothing, because a forbidden document
and a missing one look the same.

The cost is in debugging: a rule that is too strict shows up as missing
results instead of an error. When a query returns less than you expect, run
it with the admin key and compare.

### Single documents hide existence too

Reading one document by path is checked **before** looking at whether it
exists. Where the caller may not read, an existing and a missing document
give the same `permission_denied`. Where reading is allowed, a missing
document gives `not_found`.

For this to work the `get` rule is also evaluated for missing documents,
with `resource == null`. A rule that reads `resource.data.x` fails for them
and so denies, which is usually what you want. To let callers see that a
document is missing, allow the null case explicitly, as the `notes` rule
above does with `resource == null ||`.

### Listeners

A listener is checked like the corresponding read, continuously:

- A **document** listener needs `get`. If the rule denies when the listener
  is added, the request fails with `permission_denied`. If the document later
  changes so that the caller may no longer read it, the listener receives
  `removed`.
- A **query** listener is filtered by `list`, on the first snapshot and on
  every change. A document that becomes readable appears; one that stops
  being readable is removed.

One limit: the condition is re-evaluated when the *listened* document
changes. If access depends on another document through `get()` (a role, a
membership list) and only that other document changes, existing listeners
do not notice until the listened documents change or the client reconnects.

## Recipes

### Owner-only documents

```text
match /users/{uid}/todos/{todo} {
  allow read, write: if request.auth != null && request.auth.uid == uid;
}
```

The owner is in the path. Nothing in the data needs checking.

### Public read, owner write, owner stored in the document

```text
match /posts/{post} {
  allow read;
  allow create: if request.auth != null
                && request.resource.data.author == request.auth.uid;
  allow update: if request.auth != null
                && resource.data.author == request.auth.uid
                && request.resource.data.author == resource.data.author;
  allow delete: if request.auth != null && resource.data.author == request.auth.uid;
}
```

`create` checks the incoming document, since there is no stored one.
`update` checks the stored owner and that the write does not change it.

### Roles from a token claim

```text
match /reports/{id} {
  allow read:  if request.auth.token.role in ["staff", "admin"];
  allow write: if request.auth.token.role == "admin";
}
```

No lookup is needed. For callers without the claim the field access fails and
the condition is false.

### Roles from a document

```text
function role() {
  return get("roles/" + request.auth.uid).data.role;
}

match /roles/{uid} {
  allow get: if request.auth.uid == uid;   // users may see their own role
  // no write rule: roles are managed with the admin key
}
match /reports/{id} {
  allow read:  if role() in ["staff", "admin"];
  allow write: if role() == "admin";
}
```

### Membership

```text
match /rooms/{room} {
  allow read:   if request.auth.uid in resource.data.members;
  allow create: if request.auth.uid in request.resource.data.members;

  match /messages/{msg} {
    allow read:   if request.auth.uid in get("rooms/" + room).data.members;
    allow create: if request.auth.uid in get("rooms/" + room).data.members
                  && request.resource.data.from == request.auth.uid;
  }
}
```

### Validating data

Rules are also where the shape of the data is enforced, since there is no
schema.

```text
function validTodo(todo) {
  return todo.keys().hasOnly(["title", "done", "created"])
      && todo.title is string
      && todo.title.size() > 0
      && todo.title.size() <= 200
      && todo.done is bool;
}

match /users/{uid}/todos/{id} {
  allow read, delete:   if request.auth.uid == uid;
  allow create, update: if request.auth.uid == uid && validTodo(request.resource.data);
}
```

This is the rules file of the [todo example](../../examples/todo/README.md),
slightly shortened.

### Write-once fields and server timestamps

```text
// "created" must be the server's commit time and must never change after.
allow create: if request.resource.data.created == request.time;
allow update: if request.resource.data.created == resource.data.created;
```

For a write, `request.time` is the commit time, the same instant the
`$serverTimestamp` transform stores. Comparing a field with `request.time`
therefore requires that the client used the transform: a timestamp the
client chose itself would have to match the commit time to the microsecond.

## Checking and reloading

Validate a file without starting a server:

```sh
./bin/candledb check-rules candledb-data/candledb.rules
```

```text
candledb-data/candledb.rules: ok
```

A broken file gives the position of the first problem and a non-zero exit
status, which makes the command usable in CI:

```text
candledb: ./bad.rules: rules:2:18: unknown name nobody
```

A running server reloads its rules file when it receives the `SIGHUP`
signal:

```sh
kill -HUP $(pgrep -x candledb)
```

```text
level=INFO msg="security rules reloaded" file=candledb-data/candledb.rules
```

If the new file does not parse, the server logs the error and **keeps
enforcing the rules it had**:

```text
level=ERROR msg="rules reload failed, keeping the current rules" error="candledb-data/candledb.rules: rules:1:36: expected an expression, found \";\""
```

Requests in progress finish under the rules they started with. Listeners
that are already open are not re-evaluated by a reload; they pick up the new
rules as their documents change or when they reconnect.

## Limits

| Limit | Value |
|---|---|
| `get` and `exists` calls per evaluation | 10 |
| Nested function calls | 16 |
| Nesting depth of an expression | 64 |
| Length of a regular expression | 1000 characters |

The language has no loops, no variables and no way to write data, so every
evaluation ends quickly.

The implementation is in [`internal/rules`](../../internal/rules/doc.go); the
complete list of built-ins is in
[`builtins.go`](../../internal/rules/builtins.go).

Next: [8. Operations](08-operations.md)
