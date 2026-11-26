# 7. The query engine

This chapter follows a query from its definition in the domain to the index
scan that answers it, and uses that path to teach the tools Go offers for
working with collections of values.

## What you will learn

- The `slices` and `strings` packages, which replace most hand-written loops.
- Comparison functions that return an `int`, and the `cmp` package.
- Method values: passing `q.Compare` as if it were a plain function.
- When to use a struct value and when a pointer, including a pointer as "optional".
- Labelled `break`.
- Generics: type parameters and constraints, with the one generic function in this codebase, and why there is only one.
- Iterators and `range` over a function.
- How a query is planned and executed, and why a planning mistake cannot produce a wrong answer.

## The feature

A query selects documents from one collection by filters, puts them in an
order, and optionally cuts the result to a window and a limit
([tour: queries](../tour/04-queries.md)).

There are two implementations. The domain package defines what a query
*means* with a few short functions that work on documents in memory. The
SQLite adapter gets the same answer faster by scanning an index. The memory
adapter uses the first directly. The SQLite adapter uses the second to find
candidates and the first to decide what really matches.

## Where the code is

- [`internal/domain/query.go`](../../internal/domain/query.go): the `Query` type, validation, `Matches`, `Compare`, cursors, and `Apply`, the reference definition.
- [`internal/domain/value.go`](../../internal/domain/value.go): `Compare` for values, which everything else builds on.
- [`internal/adapters/memory/store.go`](../../internal/adapters/memory/store.go): `Query` in a dozen lines, by calling `Apply`.
- [`internal/adapters/sqlite/plan.go`](../../internal/adapters/sqlite/plan.go): `planQuery` chooses an access path; `Query` executes it.
- [`internal/adapters/sqlite/keyenc_test.go`](../../internal/adapters/sqlite/keyenc_test.go): the differential test that compares the two implementations.

## Comparison functions and the `cmp` package

Go's convention for ordering two values is a function that returns an
`int`: negative if the first sorts before the second, zero if they are
equal, positive otherwise. One function with three outcomes replaces
separate "less than" and "equals" functions, and every sorting and
searching function in the standard library accepts it.

`domain.Compare` is such a function for values. Here is its case for arrays:

From `internal/domain/value.go`:

```go
	case Array:
		bv := b.(Array)
		for i := range min(len(av), len(bv)) {
			if c := Compare(av[i], bv[i]); c != 0 {
				return c
			}
		}
		return cmp.Compare(len(av), len(bv))
```

The loop compares element by element and returns at the first difference.
If one array is a prefix of the other, the shorter one sorts first, which
is what the last line decides.

`cmp.Compare` is the standard comparison for the built-in ordered types:
integers, floats and strings. It saves writing the three-way `if` by hand.
`min` is a built-in function, as is `max`. `for i := range n` with an
integer `n` counts from 0 to `n-1`.

A comparison for documents is built from the comparison for values:

From `internal/domain/query.go`:

```go
func (q Query) Compare(a, b Document) int {
	for _, o := range q.Orders {
		av, _ := a.Data.Get(o.Field)
		bv, _ := b.Data.Get(o.Field)
		if c := Compare(av, bv); c != 0 {
			return flip(c, o.Desc)
		}
	}
	return flip(strings.Compare(a.Path.ID(), b.Path.ID()), q.IDDesc())
}
```

It walks the orderings in turn. The first field on which the documents
differ decides. If they agree on all of them, the document ID breaks the
tie, so the order is always total: no two different documents compare as
equal.

From `internal/domain/query.go`:

```go
func flip(c int, desc bool) int {
	if desc {
		return -c
	}
	return c
}
```

Descending order is the ascending result negated. This is why the
convention is useful: reversing an order is one minus sign.

**Typical mistake:** returning only -1 or 0 ("less" or "not less"). Sorting
needs all three answers, and a comparison that is not consistent makes the
result depend on the input order. `TestCompareIsATotalOrder` in
[`value_test.go`](../../internal/domain/value_test.go) checks the required
properties on random values.

## The `slices` package and method values

`Apply` is the whole definition of what a query returns:

From `internal/domain/query.go`:

```go
func (q Query) Apply(candidates []Document, keep func(Document) bool) []Document {
	out := make([]Document, 0, len(candidates))
	for _, doc := range candidates {
		if q.Matches(doc) && q.InWindow(doc) && (keep == nil || keep(doc)) {
			out = append(out, doc)
		}
	}
	slices.SortFunc(out, q.Compare)
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out
}
```

Filter, sort, cut to the limit. Three things in it are worth a closer look.

`make([]Document, 0, len(candidates))` creates a slice with length 0 and
room for `len(candidates)` elements. The appends that follow never need to
reallocate.

`slices.SortFunc(out, q.Compare)` sorts in place using a comparison
function. `q.Compare` here is a *method value*. `Compare` is a method that
needs a `Query` to work on. Writing `q.Compare` without calling it produces
an ordinary function of type `func(a, b Document) int` that remembers `q`.
`SortFunc` does not know that a query is involved.

`out[:q.Limit]` is a slice expression. It returns a slice that shares the
same underlying array and is merely shorter. Nothing is copied.

Method values appear in smaller places as well:

From `internal/domain/query.go`:

```go
	case OpArrayContainsAny:
		arr, isArray := v.(Array)
		return isArray && slices.ContainsFunc(f.Value.(Array), arr.Contains)
```

`slices.ContainsFunc` reports whether any element of a slice satisfies a
function. The function passed is `arr.Contains`, the `Contains` method bound
to the document's array. Read aloud: "does the list of wanted values hold
any value that `arr` contains?"

`v.(Array)` is a type assertion. The two-result form `arr, isArray :=
v.(Array)` never panics; it reports failure in the second result. The
one-result form `f.Value.(Array)` panics on a mismatch, and is used here
because `Validate` has already established that the value of this operator
is a list.

`slices.Concat` appears where the HTTP adapter builds field paths:

From `internal/adapters/httpapi/wire.go`:

```go
		// slices.Concat copies, so sibling fields never share (and
		// overwrite) one underlying array.
		path := slices.Concat(prefix, domain.FieldPath{name})
```

The comment names a real hazard. `append(prefix, name)` may write into
spare room of `prefix`'s array. Two sibling fields appending to the same
prefix would then share memory, and the second would overwrite the first.
`slices.Concat` always allocates a new array.

**Typical mistake:** keeping the result of `append(a, x)` in a new variable
while still using `a`, and assuming the two are independent.

## Values, pointers, and a pointer as "optional"

From `internal/domain/query.go`:

```go
type Query struct {
	Collection CollectionPath
	Filters    []Filter
	Orders     []Order
	Limit      int // 0 means no limit
	Start      *Cursor
	End        *Cursor
}
```

A `Query` is passed around by value. Methods are declared on `Query`, not
`*Query`, and calling one copies the struct. The copy is cheap, a few
slices and integers, and it gives a guarantee: a function that receives a
query cannot change the caller's.

The service uses that when it changes the limit of a query it was given
(`q.Limit = ceiling + 1` in
[`read.go`](../../internal/app/read.go)). The change is to a copy.

The copy is shallow. The slices inside still point at the same arrays, so
"cannot change the caller's" holds for the struct's own fields, not for the
elements of `Filters`. The code never modifies those.

`Start` and `End` are pointers for a different reason. A query may or may
not have a start cursor, and Go has no built-in optional type. A pointer
has one more possible value than the struct it points to: `nil`.

From `internal/domain/query.go`:

```go
func (q Query) AfterStart(doc Document) bool {
	if q.Start == nil {
		return true
	}
	c := q.compareToCursor(doc, q.Start)
	return c > 0 || (c == 0 && q.Start.Inclusive)
}
```

`nil` means "no cursor", and the method deals with that case first.

**Typical mistake:** dereferencing an optional pointer without the `nil`
check. The compiler does not object, and the program panics at run time.

## `strings.Builder`

Building a string piece by piece with `+=` copies the whole string each
time. `strings.Builder` collects the pieces in a growing buffer and
produces the string once.

From `internal/adapters/sqlite/plan.go`:

```go
	if sp.lo != nil {
		sql.WriteString(" AND i.key " + pick(sp.loOpen, ">", ">=") + " ?")
		args = append(args, sp.lo)
	}
	if sp.hi != nil {
		sql.WriteString(" AND i.key " + pick(sp.hiOpen, "<", "<=") + " ?")
		args = append(args, sp.hi)
	}
	if after != nil {
		// A row-value comparison: continue strictly after the last row in
		// (key, id) order.
		sql.WriteString(" AND (i.key, i.id) " + cmpAfter + " (?, ?)")
		args = append(args, after.key, after.id)
	}
	sql.WriteString(" ORDER BY i.key " + direction + ", i.id " + direction + " LIMIT ?")
	return sql.String(), append(args, limit)
```

The SQL text and the argument list are built side by side. Each time a
`?` is written to the builder, the matching value is appended to `args`.
The text fragments are fixed; `pick` chooses between two operators
according to whether the bound is included.
[Chapter 6](06-sql-and-encoding.md) explains why this is safe.

`(i.key, i.id) > (?, ?)` is a row-value comparison. It means "strictly
after this row in (key, id) order", which is how the scan continues from
where the previous page stopped.

## Iterators: `range` over a function

From `internal/domain/path.go`:

```go
func validatePath(s string) (segments int, err error) {
	if s == "" {
		return 0, fmt.Errorf("%w: path is empty", ErrInvalidArgument)
	}
	for seg := range strings.SplitSeq(s, "/") {
		segments++
		if err := validateSegment(seg); err != nil {
			return 0, fmt.Errorf("%w in path %q", err, s)
		}
	}
	if segments > MaxPathSegments {
		return 0, fmt.Errorf("%w: path has more than %d segments", ErrInvalidArgument, MaxPathSegments)
	}
	return segments, nil
}
```

`strings.Split` would return a slice of all the segments. `strings.SplitSeq`
returns an *iterator* instead: the segments are produced one at a time as
the loop asks for them, and no slice is allocated.

An iterator of strings is a function of this shape:

```go
func(yield func(string) bool)
```

When a `for ... range` loop is given such a function, Go calls it once and
passes the loop body as `yield`. The iterator calls `yield` for each
element. If the loop body executes `break` or `return`, `yield` returns
`false`, and the iterator must stop. The standard library names this type
`iter.Seq[string]`.

For reading code, the loop behaves like any other `range` loop. The body
above even returns from the surrounding function in the middle of the
iteration.

This codebase only consumes iterators from the standard library and defines
none of its own. `Store.Query` returns a slice, because every caller needs
the complete result.

## Generics, used once

A generic function has *type parameters*: placeholders for types, filled in
at each call. The listener registry keeps two lookup tables that differ
only in their key type, one keyed by document path and one by collection
path:

From `internal/app/listen.go`:

```go
// adjust is generic over the key type so that one function serves both the
// by-document and the by-collection table.
func adjust[K comparable](table map[K]map[*Session]int, key K, s *Session, delta int) {
	set := table[key]
	if set == nil {
		set = map[*Session]int{}
		table[key] = set
	}
	set[s] += delta
	if set[s] <= 0 {
		delete(set, s)
	}
	if len(set) == 0 {
		delete(table, key)
	}
}
```

`[K comparable]` declares a type parameter `K` and a *constraint* on it.
`comparable` is a built-in constraint satisfied by types whose values can be
compared with `==`, which is exactly what a map key requires. Inside the
function, `K` is used like any type.

The callers do not name the type:

From `internal/app/listen.go`:

```go
	if t.query != nil {
		adjust(h.byCollection, t.query.Collection, s, delta)
	} else {
		adjust(h.byDoc, t.doc, s, delta)
	}
```

The compiler infers `K` from the first argument: `CollectionPath` in one
call and `DocPath` in the other. Without generics the function would exist
twice, or take `any` and lose type checking.

That is the only generic function in the code base, and that is deliberate.
The alternatives were considered case by case:

- `domain.Value` is an interface with a closed set of implementations, not
  a generic type. A document field can hold any kind of value, decided at
  run time. Type parameters are fixed at compile time and cannot express that.
- `scanDoc` takes a small interface (`scanner`). It needs one method, and an
  interface says so more directly than a constraint.
- The standard generic functions (`slices.SortFunc`, `slices.Contains`,
  `min`, `max`) are used everywhere. Most of the benefit of generics comes
  from calling generic code, not from writing it.

A reasonable rule for a newcomer: write the function for one concrete type.
When a second copy that differs only in a type appears, make it generic.

**Typical mistake:** adding type parameters to make code "flexible" before
there is a second type. The signature becomes harder to read and nothing is
gained.

## How a query runs: the reference

In the memory adapter the whole query method is this:

From `internal/adapters/memory/store.go`:

```go
func (s *Store) Query(_ context.Context, q domain.Query, keep func(domain.Document) bool) ([]domain.Document, error) {
	// Copy the candidates out and release the lock before filtering. keep
	// may call back into Get, and a goroutine must not take a read lock it
	// already holds: if a writer queued up in between, both would wait
	// forever.
	s.mu.RLock()
	collection := s.collections[q.Collection]
	candidates := make([]domain.Document, 0, len(collection))
	for _, doc := range collection {
		candidates = append(candidates, doc)
	}
	s.mu.RUnlock()

	return q.Apply(candidates, keep), nil
}
```

It copies the collection's documents out under the lock and hands them to
`q.Apply`. The cost is proportional to the size of the collection, which is
fine for tests and wrong for a database.

The comment explains the copy. The reason belongs to
[chapter 9](09-concurrency.md); for now, note that `keep`, the extra filter
the security rules supply, runs after the lock is released.

## How a query runs: planning

The SQLite adapter first decides how to read candidates. The decision is
recorded in a struct:

From `internal/adapters/sqlite/plan.go`:

```go
type plan struct {
	// useIndex selects the idx table; otherwise the docs table is scanned
	// in document ID order.
	useIndex bool
	field    string
	kind     int
	// spans are the key ranges to scan. Most plans have one; "in" and
	// "array-contains-any" have one per listed value.
	spans []span
	// idFrom and idTo bound a docs-table scan (inclusive, "" = open).
	idFrom, idTo string
	desc         bool
	// ordered reports that rows arrive in the query's final order. Then the
	// scan can stop as soon as the limit is reached, which is what makes
	// "the first 10 of a million" cheap.
	ordered bool
}
```

A `plan` is data, not behaviour. `planQuery` returns one, a single executor
carries out whichever plan it is given, and a test can inspect the plan
without touching a database.

A `span` is a range of index keys. Filters are converted to spans:

From `internal/adapters/sqlite/plan.go`:

```go
func filterSpan(f domain.Filter) (span, bool) {
	key := encodeKey(f.Value)
	tag := kindTag(f.Value.Kind())
	kindOnly := span{lo: []byte{tag}, hi: []byte{tag + 1}, hiOpen: true}
	switch f.Op {
	case domain.OpEq:
		return span{lo: key, hi: key}, true
	case domain.OpGt, domain.OpGe:
		kindOnly.narrow(span{lo: key})
		return kindOnly, true
	case domain.OpLt, domain.OpLe:
		kindOnly.narrow(span{hi: key})
		return kindOnly, true
	}
	return span{}, false
}
```

An equality becomes the span from the key to itself. An ordering comparison
becomes a span that starts or ends at the key, clipped to keys of the same
kind (`kindOnly`), because `age > 5` must not match a string.

Notice what the function does not do. For `>` it uses the same span as for
`>=`. The span includes the key itself, although a strict comparison should
exclude it. That is allowed, and the next section explains why.

`narrow` has a pointer receiver, `(s *span)`, because it modifies the span
it is called on. With a value receiver it would modify a copy and the
caller would see no change.

`planQuery` then picks among a handful of shapes. With an ordering it walks
the index of the first ordered field. Without one, it looks for a filter
that an index can serve, preferring equality:

From `internal/adapters/sqlite/plan.go`:

```go
	// Without an ordering the result is in document ID order. An equality
	// on an index yields exactly that order (all keys are equal, so entries
	// sort by ID), which makes it the best choice when available.
	for _, f := range q.Filters {
		switch f.Op {
		case domain.OpEq:
			key := encodeKey(f.Value)
			return plan{useIndex: true, field: f.Field.String(), kind: kindValue,
				spans: []span{{lo: key, hi: key}}, ordered: true}
```

(The excerpt stops after the first case.) The `ordered` field matters most.
It says the rows will arrive already in the query's final order. All index
entries with the same key are sorted by document ID, and without an
ordering the result is sorted by document ID, so an equality scan is
ordered.

If nothing fits, the plan is to read the collection itself:

From `internal/adapters/sqlite/plan.go`:

```go
	// Nothing usable: read the collection itself, in ID order.
	p := plan{ordered: true}
	if q.Start != nil {
		p.idFrom = string(q.Start.Values[0].(domain.String))
	}
	if q.End != nil {
		p.idTo = string(q.End.Values[0].(domain.String))
	}
	return p
```

## How a query runs: execution and the residual filter

The executor reads the planned spans a page at a time and applies the full
query definition to every row:

From `internal/adapters/sqlite/plan.go`:

```go
scan:
	for _, sp := range spans {
		var after *position
		for {
			docs, last, err := db.page(ctx, q, p, sp, after, size)
			if err != nil {
				return nil, err
			}
			for _, doc := range docs {
				if !q.Matches(doc) {
					continue
				}
				if p.ordered && !q.BeforeEnd(doc) {
					break scan // in an ordered scan nothing later can qualify
```

`q.Matches(doc)` is the same method `Apply` uses. Applying it again to rows
the index already selected is called a *residual filter*. It is what makes
the loose span for `>` harmless: the row with the equal key is read and then
rejected here.

This gives the design its safety property. A plan only has to produce a
superset of the answer. Which documents are in the answer is decided by
`domain.Query`, in one place. If the planner chooses badly, the query reads
more rows than necessary. It still returns the right documents.

The one thing a plan can get wrong is to leave rows out: a span that is too
narrow, or `ordered: true` when the rows are not in final order. The
differential test below exists to catch that.

`scan:` is a *label*, and `break scan` leaves the loop that carries it. A
plain `break` would leave only the innermost `for`, the loop over one page.
Here the intent is to stop the whole scan, through three nested loops.

From `internal/adapters/sqlite/plan.go`:

```go
				if keep != nil && !keep(doc) {
					continue
				}
				out = append(out, doc)
				if p.ordered && q.Limit > 0 && len(out) == q.Limit {
					break scan
```

When the plan is ordered and the limit is reached, the scan stops. That
early exit is what makes "the first 10 of a million" cost ten rows. When
the plan is not ordered, the executor has to collect every match, sort with
`slices.SortFunc(out, q.Compare)`, and cut to the limit afterwards, exactly
as `Apply` does.

**Typical mistake with labels:** using them where a small function with a
`return` would be clearer. A label is justified here because the loop body
shares several variables with the code after it.

## The differential test

How do you test a planner? Hand-written cases cover the situations the
author thought of. The bugs are in the others.

A *differential test* runs two implementations of the same thing on the
same input and requires equal output. Here the memory adapter is the
reference and the SQLite adapter is under test:

From `internal/adapters/sqlite/keyenc_test.go`:

```go
		want, err := reference.Query(ctx, q, keep)
		if err != nil {
			t.Fatal(err)
		}
		got, err := db.Query(ctx, q, keep)
		if err != nil {
			t.Fatalf("query %+v: %v", q, err)
		}
		if w, g := pathsOf(want), pathsOf(got); w != g {
			t.Fatalf("results differ for query %+v\n plan: %+v\n want: %s\n got:  %s", q, planQuery(q), w, g)
```

The test loads the same 250 random documents into both stores and then
generates 3,000 random queries: random filters and operators, up to two
orderings, limits, cursors, and sometimes a `keep` function. The random
generator has a fixed seed, so a failure can be reproduced.

The reference is short enough to be checked by reading it. The planner is
not. The test transfers the confidence from one to the other. The technique
is described further in [chapter 3](03-testing.md).

## Try it

Run the tests for the reference definition and for the planner:

```sh
go test ./internal/domain -run 'TestQueryFilters|TestQueryOrderLimitAndCursors' -v
go test ./internal/adapters/sqlite -run 'TestPlanChoosesOrderedScans|TestQueriesAgreeWithReference' -v
```

The last line of the differential test's output reports how many of the
random queries returned documents, as evidence that it compared something.

Now see the test earn its place. In
`internal/adapters/sqlite/plan.go`, in `filterSpan`, change

```go
		kindOnly.narrow(span{lo: key})
```

to

```go
		kindOnly.narrow(span{lo: key, loOpen: true})
```

This makes the span for `>` and `>=` exclude the key itself. It looks like
a sensible optimisation for `>`. Run both test groups of the adapter:

```sh
go test ./internal/adapters/sqlite -run 'TestStoreContract|TestQueriesAgreeWithReference'
```

The hand-written contract tests still pass. The differential test fails and
prints the query, the plan, and the two results: a `>=` filter that lost
the documents whose value equals the bound. Undo the change.

Finally, measure the early exit:

```sh
go test ./internal/adapters/sqlite -run '^$' -bench QueryTopTen
```

The two lines, for 1,000 and for 50,000 documents, show nearly the same
time per query.

## Exercise

Add a `prefix` operator for strings, so that a client can ask for
`name prefix "Ad"`.

**Goal.** A filter with the operator `"prefix"` matches documents whose
field is a string that starts with the given string. The SQLite adapter
answers it with a range scan.

**Files.**

- `internal/domain/query.go`: a constant `OpPrefix`, a case in
  `Filter.validate` (the value must be a string), a case in `Filter.Matches`.
- `internal/adapters/sqlite/plan.go`: a case in `filterSpan`.
- `internal/adapters/sqlite/keyenc_test.go`: add the operator to the `ops`
  list of the differential test.
- `internal/domain/query_test.go` and
  `internal/app/storetest/storetest.go`: a hand-written case each.

**Hints.**

- Do the domain part first and add the operator to the differential test
  before touching the planner. It passes, because a filter the planner does
  not understand is handled by the residual filter. Correct first, fast
  second.
- For the span you need the smallest and the largest key a string with that
  prefix can have. Look at what `appendEscaped` appends after the string's
  bytes, and at what the key of the prefix itself would be without that
  ending. What is the smallest byte sequence that is greater than every
  key beginning with those bytes?
- A span may be wider than necessary but never narrower. If you are unsure
  about the upper bound, start with one that is obviously too wide and
  tighten it while the differential test stays green.
- Think about a prefix that contains a zero byte, and about the empty
  prefix. The random strings in
  [`domaintest/random.go`](../../internal/domain/domaintest/random.go)
  include both kinds of string.
- The HTTP layer needs no change: it passes the operator string through
  and `Validate` judges it.

**How to know it works.** The differential test passes with the operator in
its list, and `TestPlanChoosesOrderedScans` gains a case showing that a
prefix filter uses the index with one span.

## Further reading

- [slices package documentation](https://pkg.go.dev/slices) and [cmp package documentation](https://pkg.go.dev/cmp)
- [strings package documentation](https://pkg.go.dev/strings)
- [Tutorial: Getting started with generics](https://go.dev/doc/tutorial/generics)
- [When To Use Generics](https://go.dev/blog/when-generics)
- [Range Over Function Types](https://go.dev/blog/range-functions)
- [A Tour of Go: methods and pointer receivers](https://go.dev/tour/methods/4)

Next: [8. An interpreter for the rules language](08-interpreter.md)
