# 6. SQL and byte encoding

This chapter covers the SQLite storage adapter: how Go talks to a SQL
database, and how candledb turns values into bytes that sort correctly.

## What you will learn

- How `database/sql` works: drivers, the connection pool, queries, rows.
- Why the adapter opens the same file twice, once for writing and once for reading.
- How to run a transaction so that it can never be left open by mistake.
- How schema migrations are tracked without a migration library.
- Why bound parameters prevent SQL injection, and where this code still builds SQL text.
- The difference between `string` and `[]byte`, and how to write integers and floats as bytes.
- The bit manipulation that makes encoded numbers sort like the numbers themselves.

## The feature

Everything the server stores lives in one SQLite file, `candledb.db`, in the
data directory. The adapter in `internal/adapters/sqlite` implements two
ports from [chapter 4](04-interfaces-and-ports.md): `app.Store` for
documents and `app.UserStore` for accounts
([tour: accounts](../tour/06-auth.md)).

A document is one row in the `docs` table. Every field of every document
also gets rows in the `idx` table, keyed by an encoding of the field's
value. That table is what lets a query such as "age >= 18, ordered by age"
read only the rows it needs ([tour: queries](../tour/04-queries.md)). The
design is explained in
[architecture: design decisions](../architecture/03-design-decisions.md);
this chapter explains the Go.

## Where the code is

- [`internal/adapters/sqlite/db.go`](../../internal/adapters/sqlite/db.go): opening the file, the schema, migrations, the transaction helper, backup.
- [`internal/adapters/sqlite/store.go`](../../internal/adapters/sqlite/store.go): `Get`, `Version`, `Apply`, and the list of index rows a document needs.
- [`internal/adapters/sqlite/keyenc.go`](../../internal/adapters/sqlite/keyenc.go): the order-preserving encoding of values.
- [`internal/adapters/sqlite/users.go`](../../internal/adapters/sqlite/users.go): users and refresh tokens.
- [`internal/adapters/sqlite/plan.go`](../../internal/adapters/sqlite/plan.go): queries. It is the subject of [chapter 7](07-query-engine.md); here it only supplies one example.

## Drivers and the blank import

`database/sql` is the standard library's interface to SQL databases. It
contains no code for any particular database. That code comes from a
*driver*, a separate package that registers itself with `database/sql`
under a name.

From `internal/adapters/sqlite/db.go`:

```go
import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime"

	_ "modernc.org/sqlite" // registers the "sqlite" driver with database/sql
)
```

The last import has `_` in place of a name. This is a *blank import*: the
package is linked into the program and its `init` functions run, but the
importing file refers to nothing in it. The driver's `init` registers the
name `"sqlite"`, which is the first argument of `sql.Open` later in the file.

Go normally refuses to compile a file with an unused import. The blank
identifier is how you say "I want this package only for its side effect."

This driver, `modernc.org/sqlite`, is SQLite translated into Go. The more
common driver wraps the C library and needs a C compiler to build. Using the
pure Go one is what lets the server build as a single static binary, which
[chapter 10](10-production.md) returns to.

**Typical mistake:** forgetting the blank import. The program compiles, and
fails when it starts with `sql: unknown driver "sqlite"`.

## `*sql.DB` is a pool, not a connection

`sql.Open` returns a `*sql.DB`. Despite the name, it is not one connection.
It is a pool that opens connections when they are needed, hands one to each
operation, and takes it back afterwards. A `*sql.DB` is safe to use from
many goroutines at once and is meant to be created once and kept for the
life of the program.

Two consequences are easy to miss. First, `sql.Open` does not connect to
anything; it only checks its arguments. The first real operation opens the
first connection. Second, two statements run one after the other through a
`*sql.DB` may use two different connections. Anything that must happen on
one connection, such as a transaction, needs the transaction API shown below.

The adapter keeps two pools for the same file:

From `internal/adapters/sqlite/db.go`:

```go
type DB struct {
	// SQLite allows many readers but only one writer at a time. Giving
	// writes their own handle limited to one connection turns that rule
	// into ordinary queueing inside database/sql, instead of "database is
	// locked" errors under load.
	write *sql.DB
	read  *sql.DB
}
```

SQLite allows any number of readers but only one writer at a time. If
several connections try to write, all but one receive a "database is
locked" error or wait for a timeout. The adapter avoids the situation
instead of handling it:

From `internal/adapters/sqlite/db.go`:

```go
	dsn := func(extra string) string {
		return "file:" + (&url.URL{Path: path}).EscapedPath() +
			"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)" + extra
	}

	write, err := sql.Open("sqlite", dsn("&_txlock=immediate"))
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	write.SetMaxOpenConns(1)
```

`SetMaxOpenConns(1)` limits the write pool to a single connection. A second
goroutine that wants to write simply waits inside `database/sql` until the
connection is free. The one-writer rule of SQLite has become ordinary
queueing.

The string passed to `sql.Open` is the *data source name* (DSN). Its format
belongs to the driver. Here it is a file path followed by options. Each
`_pragma=` option makes the driver run a SQLite `PRAGMA` statement on every
new connection:

- `busy_timeout(5000)`: wait up to five seconds for a lock before failing.
- `journal_mode(WAL)`: use a write-ahead log, so readers are not blocked while a write is in progress.
- `synchronous(NORMAL)`: the level of flushing to disk that SQLite recommends with WAL.

`_txlock=immediate`, added only for the write pool, makes a transaction take
the write lock when it begins and not at its first write.

`dsn` is a function value stored in a local variable. It can use `path`
from the surrounding function. Such a function is a *closure*; the pattern
appears again in this chapter.

The read pool is opened after the schema is in place:

From `internal/adapters/sqlite/db.go`:

```go
	read, err := sql.Open("sqlite", dsn("&_pragma=query_only(1)"))
	if err != nil {
		write.Close()
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	read.SetMaxOpenConns(max(4, runtime.NumCPU()))
	db.read = read
```

Its connections are marked `query_only`, so a bug that sent a write through
the read pool would fail loudly. Note the `write.Close()` on the error path:
when a function has already acquired something and then fails, it must
release what it acquired.

**Typical mistake:** calling `sql.Open` inside a request handler. That
creates a new pool per request and leaks connections.

## Reading one row

From `internal/adapters/sqlite/store.go`:

```go
func (db *DB) Get(ctx context.Context, path domain.DocPath) (domain.Document, error) {
	parent := path.Parent()
	row := db.read.QueryRowContext(ctx,
		"SELECT "+docColumns+" FROM docs WHERE parent = ? AND id = ?", string(parent), path.ID())
	doc, err := scanDoc(row, parent)
	if errors.Is(err, sql.ErrNoRows) {
		return doc, fmt.Errorf("%w: document %s", domain.ErrNotFound, path)
	}
	return doc, err
}
```

`QueryRowContext` runs a query expected to return at most one row. The `?`
marks are placeholders, and the values after the SQL text are bound to them
in order.

`QueryRowContext` never returns an error itself. The error is held back
until `Scan` is called, which here happens inside `scanDoc`. When the query
matched no row, `Scan` returns the sentinel `sql.ErrNoRows`. The adapter
translates it into the domain's `ErrNotFound`, so the rest of the program
never sees a `database/sql` error. Sentinel errors and `errors.Is` are the
subject of [chapter 2](02-errors.md).

Every method here ends in `Context` and takes a `ctx`. If the HTTP request
that caused the query is cancelled, the query is abandoned. The
context-free variants (`QueryRow`, `Exec`) exist, but server code should not
use them.

`scanDoc` accepts either one row or a cursor over many rows, because both
have a `Scan` method:

From `internal/adapters/sqlite/store.go`:

```go
// scanner is satisfied by both *sql.Row and *sql.Rows, so one function can
// decode a document from either.
type scanner interface {
	Scan(dest ...any) error
}
```

`database/sql` does not define this interface. The adapter declares the
one-method interface it needs, and `*sql.Row` and `*sql.Rows` satisfy it
without knowing about it. This is the same idea as the ports in
[chapter 4](04-interfaces-and-ports.md), at the smallest scale.

## Reading many rows

From `internal/adapters/sqlite/plan.go`:

```go
	rows, err := db.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var (
		docs []domain.Document
		last *position
	)
	for rows.Next() {
		var key []byte
		doc, err := scanDoc(prefixed{rows, &key}, q.Collection)
		if err != nil {
			return nil, nil, err
		}
		docs = append(docs, doc)
		last = &position{key: key, id: doc.Path.ID()}
	}
	return docs, last, rows.Err()
```

`QueryContext` returns a `*sql.Rows`, a cursor. Three rules apply to it.

1. **Close it.** While a `Rows` is open it holds a connection from the pool.
   `defer rows.Close()` right after the error check guarantees the
   connection goes back on every path out of the function.
2. **Loop with `rows.Next()`.** It advances to the next row and returns
   `false` when there are no more, or when something went wrong.
3. **Check `rows.Err()` after the loop.** A `false` from `Next` does not say
   which of the two cases happened. `rows.Err()` does. Here it is the last
   value returned, so a failure half-way through is not mistaken for the end
   of the results.

`args...` passes a slice as the variadic arguments. The slice has type
`[]any`, built up alongside the SQL text.

**Typical mistake:** leaving out `rows.Err()`. The loop ends early on a
network or disk error and the function returns a short list as if it were
complete.

## Transactions and the `inTx` helper

A transaction is a group of statements that take effect together or not at
all. In `database/sql` you call `BeginTx`, which reserves one connection
and returns a `*sql.Tx`. You then run statements on the `Tx`, and finish
with `Commit` or `Rollback`. A transaction that is never finished keeps its
connection for ever, and with a write pool of size one that would stop all
writes.

The adapter writes that sequence once:

From `internal/adapters/sqlite/db.go`:

```go
// inTx runs fn inside a write transaction, committing if fn returns nil and
// rolling back otherwise. Wrapping the begin/commit/rollback dance in one
// helper means no caller can forget a rollback on an error path.
func (db *DB) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		// Join keeps both errors if the rollback fails too.
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}
```

`inTx` takes a function as its parameter. The caller supplies the
statements; `inTx` supplies the begin, and the commit or rollback. No caller
can forget the rollback on an error path, because no caller writes it.

`errors.Join` combines the original error with a possible error from
`Rollback`. If the rollback succeeds it returns `nil`, and `Join` ignores
`nil` values, so the result then carries only the original error.

Here is the caller that stores a commit:

From `internal/adapters/sqlite/store.go`:

```go
	return db.inTx(ctx, func(tx *sql.Tx) error {
		// A commit inserts many index rows, so that statement is compiled
		// once and reused.
		insertIndex, err := tx.PrepareContext(ctx,
			"INSERT OR IGNORE INTO idx (parent, field, kind, key, id) VALUES (?, ?, ?, ?, ?)")
		if err != nil {
			return err
		}
		defer insertIndex.Close()
```

The function literal passed to `inTx` is a closure. It uses `ctx` and
`batch` from `Apply` and receives `tx` from `inTx`. Inside it, every
statement goes through `tx`, never through `db.write`. A statement sent to
`db.write` from inside the closure would ask the pool for a second
connection, and the pool has only one, which the transaction is holding.
The call would wait for itself until its context was cancelled.

**Typical mistake:** using the pool inside a transaction. With a larger
pool the statement runs, but outside the transaction, so it is not rolled
back with the rest.

## Prepared statements

The excerpt above calls `tx.PrepareContext`. Preparing a statement asks the
database to parse and plan it once. The returned `*sql.Stmt` can then be
executed many times with different arguments.

`Apply` inserts one index row per field of every document in the batch,
which can be hundreds of rows, so the insert is prepared once and reused in
the loop. The other statements in `Apply` run once per document and are
left as plain `ExecContext` calls. A `Stmt` holds resources in the database
until it is closed, hence the `defer insertIndex.Close()`.

`ExecContext` is the third query method: it is for statements that return
no rows.

## Migrations with `user_version`

The schema changes over the life of a product. Each database file has to
know which changes it already has. SQLite gives every file an integer for
the application's own use, read and written with `PRAGMA user_version`. The
adapter stores the number of applied schema steps there.

From `internal/adapters/sqlite/db.go`:

```go
func (db *DB) migrate(ctx context.Context) error {
	var applied int
	if err := db.write.QueryRowContext(ctx, "PRAGMA user_version").Scan(&applied); err != nil {
		return err
	}
	if applied > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this program supports (%d)", applied, len(migrations))
	}
	for i := applied; i < len(migrations); i++ {
		err := db.inTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
				return err
			}
			// PRAGMA does not accept bound parameters; i is our own
			// integer, so formatting it into the statement is safe.
			_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1))
			return err
		})
		if err != nil {
			return fmt.Errorf("schema migration %d: %w", i+1, err)
		}
	}
	return nil
}
```

`migrations` is a slice of SQL strings. A new file reports version 0 and
runs every step. A file at version 1 runs the steps from index 1 on. A file
with a higher version than the program knows was written by a newer
candledb, and the program refuses to touch it.

Each step and its version bump run in one transaction. If the program is
killed half-way through a step, the file still has the old schema and the
old number, and the step runs again next time.

The rule that makes this safe is in the comment above `migrations`: add a
step, never edit one that has shipped.

## SQL injection, and where text is concatenated

SQL injection happens when a program builds a statement by pasting user
input into SQL text. If the input contains SQL syntax, the database
executes it. The defence is to send the statement and the values
separately, which is what the `?` placeholders do. A bound value is data;
the database never parses it as SQL.

In this adapter every value that originates outside the program is bound:
paths, field names, index keys, emails, token hashes. Even the destination
of a backup is bound (`VACUUM INTO ?`).

SQL text is still assembled from parts in a few places. It is worth knowing
them, because they are where a future change could introduce a hole.

From `internal/adapters/sqlite/users.go`:

```go
// user runs the one user query with the given condition. where is always
// one of the two constants above, never caller input, so building the SQL by
// concatenation is safe here.
func (s *UserStore) user(ctx context.Context, where string, arg any) (app.User, error) {
	var (
		u       app.User
		created int64
	)
	err := s.db.read.QueryRowContext(ctx,
		"SELECT id, email, password_hash, created_at FROM users WHERE "+where, arg).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return u, fmt.Errorf("%w: user", domain.ErrNotFound)
	}
	u.CreatedAt = time.UnixMicro(created).UTC()
	return u, err
}
```

`where` is appended to the SQL. That is safe only because of who calls
`user`: the two methods directly above it pass the literals `"email = ?"`
and `"id = ?"`. The email or ID itself travels as `arg`. The function is
unexported, so no code outside the package can pass anything else.

The other places follow the same reasoning:

- `plan.statement` in `plan.go` builds a query from fixed fragments. What
  varies is chosen by the program from a short list (`">"` or `">="`, `ASC`
  or `DESC`), never copied from a request.
- `docColumns` is a constant list of column names.
- `migrate` formats the loop counter into `PRAGMA user_version = %d`,
  because SQLite does not accept a placeholder in a `PRAGMA`. The value is
  the program's own integer.

**Typical mistake:** `fmt.Sprintf("... WHERE email = '%s'", email)`. It
works in every test and is exploitable in production.

## An atomic take with `DELETE ... RETURNING`

A refresh token may be used once ([tour: accounts](../tour/06-auth.md)). If
two requests arrive with the same token at the same moment, exactly one may
succeed.

From `internal/adapters/sqlite/users.go`:

```go
func (s *UserStore) TakeRefreshToken(ctx context.Context, hash string) (app.RefreshToken, error) {
	t := app.RefreshToken{Hash: hash}
	var expires int64
	// DELETE ... RETURNING removes the row and hands it back in a single
	// statement, so two requests racing with the same token cannot both
	// receive it.
	err := s.db.write.QueryRowContext(ctx,
		"DELETE FROM refresh_tokens WHERE hash = ? RETURNING user_id, expires_at", hash).
		Scan(&t.UserID, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return t, fmt.Errorf("%w: refresh token", domain.ErrNotFound)
	}
	t.ExpiresAt = time.UnixMicro(expires).UTC()
	return t, err
}
```

A `SELECT` followed by a `DELETE` would have a gap between the two
statements in which a second request could also select the row.
`DELETE ... RETURNING` deletes the row and returns its columns in one
statement. For the second request there is no row left, `Scan` reports
`sql.ErrNoRows`, and the caller is told the token is unknown.

The statement returns a row, so it is run with `QueryRowContext` even though
it is a write, and it goes through the write pool. The contract test
`refresh tokens are single use` in
[`storetest.go`](../../internal/app/storetest/storetest.go) races eight
goroutines for one token and requires exactly one winner.

## Strings and byte slices

The second half of this chapter is about `keyenc.go`. It needs two types
that look alike.

A `string` is an immutable sequence of bytes. A `[]byte` is a slice of
bytes that can be changed and grown. Converting between them (`[]byte(s)`,
`string(b)`) copies the data. Indexing a string, `s[i]`, gives a byte, not
a character. [Chapter 8](08-interpreter.md) covers characters.

The encoder builds its output with `append`:

From `internal/adapters/sqlite/keyenc.go`:

```go
// encodeKey returns the order-preserving encoding of v.
func encodeKey(v domain.Value) []byte { return appendKey(nil, v) }

// appendKey appends the encoding of v to buf and returns the extended
// slice, in the style of the standard library's strconv.AppendInt. Building
// into one growing buffer avoids allocating a new slice per nested value.
func appendKey(buf []byte, v domain.Value) []byte {
	switch x := v.(type) {
	case domain.Null:
		return append(buf, tagNull)
	case domain.Bool:
		if x {
			return append(buf, tagBool, 1)
		}
		return append(buf, tagBool, 0)
```

`append` adds elements to a slice and returns the resulting slice. The
result must be assigned or returned, because when the slice runs out of
room `append` allocates a larger array and the old slice value no longer
refers to the data.

`appendKey` follows a convention from the standard library (for example
`strconv.AppendInt`): take the buffer as the first parameter, append to it,
return it. Nested values append to the same buffer, so encoding a map with
ten fields does not create ten small slices. `encodeKey` starts the process
with `nil`, which is a valid empty slice to append to.

The `switch x := v.(type)` is a type switch over the closed set of value
types from [chapter 1](01-types-and-values.md).

## Why encode at all

SQLite compares two `BLOB` values byte by byte, from the left, the way
`bytes.Compare` does. Its B-tree index keeps rows sorted by that comparison.
If the bytes stored for two values compare the same way the values do, then
a range of values is a contiguous range of index rows, and the database can
find it without looking at anything else.

So the requirement is:

    Compare(a, b) < 0   exactly when   bytes.Compare(encodeKey(a), encodeKey(b)) < 0

The first byte of every key is a tag for the kind of value, and the tags
increase in the order kinds sort in: null, bool, number, timestamp, string,
array, map. Values of different kinds are therefore ordered by their first
byte. The rest of this chapter is about ordering values of the same kind.

## Integers as bytes: big-endian and the sign bit

From `internal/adapters/sqlite/keyenc.go`:

```go
	case domain.Int, domain.Double:
		// The same (float, integer) pair that domain.Compare orders numbers
		// by, each made sortable as unsigned big-endian bytes.
		f, i := domain.NumberKey(v)
		buf = append(buf, tagNumber)
		buf = binary.BigEndian.AppendUint64(buf, sortableFloat(f))
		return binary.BigEndian.AppendUint64(buf, sortableInt(i))
```

`encoding/binary` converts between integers and bytes. A `uint64` occupies
eight bytes, and there are two conventions for their order. *Big-endian*
writes the most significant byte first; *little-endian* writes it last.
Only big-endian has the property needed here: comparing the bytes from the
left gives the same answer as comparing the numbers, just as comparing
`0123` and `0456` digit by digit works because the most significant digit
comes first.

That holds for unsigned integers. A signed `int64` is stored in *two's
complement*, in which negative numbers have the top bit set. Read as
unsigned, every negative number looks larger than every positive one.

From `internal/adapters/sqlite/keyenc.go`:

```go
// sortableInt maps int64 to uint64 so that unsigned order equals signed
// order: flipping the sign bit moves the negative half below the positive.
func sortableInt(i int64) uint64 { return uint64(i) ^ (1 << 63) }

// sortableFloat does the same for float64. Positive floats already compare
// correctly as unsigned integers once the sign bit is set; negative floats
// compare backwards, so all their bits are inverted.
func sortableFloat(f float64) uint64 {
	bits := math.Float64bits(f)
	if bits&(1<<63) != 0 {
		return ^bits
	}
	return bits | (1 << 63)
}
```

`^` with two operands is XOR, and `1 << 63` is a value with only the top
bit set. `uint64(i) ^ (1 << 63)` flips the top bit and leaves the rest.

| `i` | bits of `uint64(i)` | after flipping the top bit |
|---|---|---|
| smallest `int64` | `8000000000000000` | `0000000000000000` |
| -1 | `FFFFFFFFFFFFFFFF` | `7FFFFFFFFFFFFFFF` |
| 0 | `0000000000000000` | `8000000000000000` |
| 1 | `0000000000000001` | `8000000000000001` |
| largest `int64` | `7FFFFFFFFFFFFFFF` | `FFFFFFFFFFFFFFFF` |

Read the right-hand column from top to bottom: it increases. The negative
half has moved below the positive half, and within each half the order was
already right.

## Floats as bytes

A `float64` is also 64 bits: one sign bit, then eleven bits of exponent,
then 52 bits of fraction. `math.Float64bits` returns those bits as a
`uint64` without changing them.

For positive floats the layout already sorts correctly as an unsigned
integer, because a larger exponent or a larger fraction means a larger
number. They only need to be placed above the negatives, so `sortableFloat`
sets the top bit (`bits | (1 << 63)`; `|` is OR).

Negative floats have the sign bit set and are otherwise stored like their
positive counterpart. So among negatives, a larger bit pattern means a
number further from zero, which is a *smaller* number. Inverting all the
bits (`^bits`; `^` with one operand is NOT) reverses their order and clears
the top bit in the same step.

| `f` | `Float64bits(f)` | `sortableFloat(f)` |
|---|---|---|
| -2.0 | `C000000000000000` | `3FFFFFFFFFFFFFFF` |
| -1.0 | `BFF0000000000000` | `400FFFFFFFFFFFFF` |
| 0.0 | `0000000000000000` | `8000000000000000` |
| 1.0 | `3FF0000000000000` | `BFF0000000000000` |
| 2.0 | `4000000000000000` | `C000000000000000` |

Again the right-hand column increases from top to bottom.

A number key holds both: the float form, then the integer form. The reason
is that `Int` and `Double` values are compared with each other, and a
`float64` cannot represent every large integer exactly. `domain.NumberKey`
defines the pair, and the same pair decides `domain.Compare`, so the value
order and the byte order cannot drift apart.

**Typical mistake:** using `binary.LittleEndian` because it is the machine's
native order. Everything round-trips correctly and the index sorts wrongly.

## Strings that can be followed by more bytes

A string's own bytes already sort correctly. The problem is knowing where
it ends when it sits inside an array or a map and more bytes follow.

From `internal/adapters/sqlite/keyenc.go`:

```go
// appendEscaped writes a string so that it can be followed by more data
// without ambiguity. The string ends with the two bytes 00 01; a zero byte
// inside the string is written as 00 FF. Because 01 < FF, a string still
// sorts before any longer string it is a prefix of.
func appendEscaped(buf []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		if s[i] == 0x00 {
			buf = append(buf, 0x00, 0xFF)
		} else {
			buf = append(buf, s[i])
		}
	}
	return append(buf, 0x00, 0x01)
}
```

The string is ended by the two bytes `00 01`. A zero byte inside the string
is written as `00 FF`, so the terminator cannot be confused with content.
The values are chosen for sorting: `"a"` is `61 00 01` and `"a\x00"` is
`61 00 FF 00 01`. At the third byte `01` is less than `FF`, so the shorter
string sorts first, as it should.

The loop uses an index and `s[i]`, which reads bytes. A `for ... range`
loop over a string would decode characters instead, and the encoding is
defined on bytes.

## From a document to index rows

From `internal/adapters/sqlite/store.go`:

```go
func indexEntries(data domain.Map) []indexEntry {
	var entries []indexEntry
	var visit func(field string, v domain.Value)
	visit = func(field string, v domain.Value) {
		entries = append(entries, indexEntry{field, kindValue, encodeKey(v)})
		switch x := v.(type) {
		case domain.Array:
			for _, e := range x {
				entries = append(entries, indexEntry{field, kindElement, encodeKey(e)})
			}
		case domain.Map:
			for name, child := range x {
				visit(field+"."+name, child)
			}
		}
	}
	for name, v := range data {
		visit(name, v)
	}
	return entries
}
```

`visit` is a function value that calls itself. It is declared with `var`
on one line and assigned on the next, because a function literal can only
refer to a variable that already exists. It appends to `entries`, a
variable of the enclosing function: another closure.

Each field yields one row for its value. An array also yields a row per
element, with a different `kind`, for `array-contains` queries. A map is
visited again for each of its fields, with the field name extended by a
dot. That is why `address.city` is queryable on its own.

## Try it

Run the tests that pin down this chapter's two main claims: data survives a
restart, and the encoding preserves order.

```sh
go test ./internal/adapters/sqlite -run 'TestKeyEncodingPreservesOrder|TestDataSurvivesReopen|TestUserStoreContract' -v
```

Then look at a real database. Start a server with its data in a scratch
directory:

```sh
./bin/candledb serve -addr 127.0.0.1:18121 -data-dir /tmp/candledb-ch06 -dev-open-rules
```

In a second terminal, store one document and read the tables with the
`sqlite3` command-line tool:

```sh
curl -s -X PUT http://127.0.0.1:18121/v1/documents/people/ada \
  -d '{"name": "ab", "age": 1, "tags": ["x"], "address": {"city": "Rome"}}'

sqlite3 /tmp/candledb-ch06/candledb.db "PRAGMA user_version; PRAGMA journal_mode;"
sqlite3 /tmp/candledb-ch06/candledb.db "SELECT field, kind, hex(key) FROM idx ORDER BY field, kind;"
```

The first command prints `1` and `wal`: one migration applied, write-ahead
logging on. The second prints the index rows of the document:

```text
address|0|604063697479000140526F6D65000100
address.city|0|40526F6D650001
age|0|20BFF00000000000008000000000000001
name|0|4061620001
tags|0|504078000100
tags|1|40780001
```

Decode a few by hand:

- `name`: tag `40` (string), then `61 62` (`ab`), then the terminator `00 01`.
- `age`: tag `20` (number), then `BFF0000000000000`, which is
  `sortableFloat(1.0)` from the table above, then `8000000000000001`, which
  is `sortableInt(1)`.
- `tags` appears twice: once with kind 0 for the whole array (tag `50`, the
  element, the terminator `00`) and once with kind 1 for the element `"x"`.

Finally, ask SQLite how it would run a range query on the index:

```sh
sqlite3 /tmp/candledb-ch06/candledb.db "EXPLAIN QUERY PLAN SELECT id FROM idx WHERE parent='people' AND field='age' AND kind=0 AND key >= x'20' ORDER BY key, id LIMIT 10;"
```

The answer, `SEARCH idx USING PRIMARY KEY (parent=? AND field=? AND kind=? AND key>?)`,
shows that the query is a search in the primary key with no separate
sorting step. Stop the server with Ctrl-C when you are done.

## Exercise

Add a method `Stats` to the SQLite adapter that reports how large the
database is.

**Goal.** `func (db *DB) Stats(ctx context.Context) (Stats, error)` returns
a struct with the number of documents and the number of index entries.

**Files.** `internal/adapters/sqlite/store.go` for the method and the
`Stats` type, `internal/adapters/sqlite/store_test.go` for the test.

**Hints.**

- `SELECT count(*) FROM docs` returns one row with one column. Which of the
  three query methods fits?
- You can compute both numbers in a single statement by selecting two
  sub-queries, and `Scan` accepts several destinations.
- Which pool should a read-only statement use?
- For the test, the `open` helper gives you an empty database. Store the
  `people/ada` document from "Try it" with `Apply`, and work out from the
  output above how many index entries it must produce.

**How to know it works.** Your test passes, it fails if you swap the two
numbers, and `go vet ./...` is clean.

**Stretch.** Add a `candledb stats` command next to `backup` in
[`cmd/candledb/tools.go`](../../cmd/candledb/tools.go) that opens the
database and prints the two numbers. `backup` is a model for the flag
handling.

## Further reading

- [database/sql package documentation](https://pkg.go.dev/database/sql)
- [Tutorial: Accessing a relational database](https://go.dev/doc/tutorial/database-access)
- [Executing transactions](https://go.dev/doc/database/execute-transactions) and [Avoiding SQL injection risk](https://go.dev/doc/database/sql-injection)
- [encoding/binary package documentation](https://pkg.go.dev/encoding/binary)
- [Go Slices: usage and internals](https://go.dev/blog/slices-intro)
- [Strings, bytes, runes and characters in Go](https://go.dev/blog/strings)

Next: [7. The query engine](07-query-engine.md)
