# 01 · Types and values

## What you will learn

- How to declare variables, and what a zero value is
- Structs, and named types built on existing types
- Methods, and the difference between value and pointer receivers
- Constants and `iota`
- Slices and maps, including what `nil` means for each
- Interfaces as a closed set of types, type switches and type assertions
- Functions with several results
- How this codebase keeps values immutable without language support
- Closures

## The feature

A candledb database holds **documents**. A document lives at a **path** such
as `users/alice`, and its content is a set of named fields. A field holds a
**value**: null, a boolean, a number, a timestamp, a string, a list or a
nested map. The tour describes this model in
[Data model](../tour/02-data-model.md).

All of that vocabulary is defined in one package, `internal/domain`. The
package does no input or output. It only defines the types and the pure
rules about them: how two values compare, what a valid path looks like, what
a write does to a document. That makes it the right place to start: every
other package uses these types, and nothing here depends on anything else in
the project.

## Where the code is

Read the files in this order:

1. [`internal/domain/doc.go`](../../internal/domain/doc.go): the package
   comment. Six lines that say what the package is for.
2. [`internal/domain/value.go`](../../internal/domain/value.go): the `Value`
   types and how values compare.
3. [`internal/domain/path.go`](../../internal/domain/path.go): document
   paths, collection paths and field paths.
4. [`internal/domain/document.go`](../../internal/domain/document.go): the
   `Document` struct.
5. [`internal/domain/valuejson.go`](../../internal/domain/valuejson.go):
   conversion between values and JSON. Skim it now; chapter 05 returns to it.
6. [`internal/domain/write.go`](../../internal/domain/write.go): writes and
   the transforms (`increment`, `arrayUnion` and so on).

## Declarations and the shape of a file

Every Go file has the same order: the `package` clause, the imports, then
declarations. A declaration starts with one of four keywords: `var`, `const`,
`type` or `func`. Here is the whole of
[`internal/domain/document.go`](../../internal/domain/document.go) except its
last function:

```go
package domain

import (
	"fmt"
	"time"
)

// Document is one stored record: its data plus the metadata the server
// maintains for it.
type Document struct {
	Path       DocPath
	Data       Map
	CreateTime time.Time
	UpdateTime time.Time
	// Version is the commit version of the write that last changed the
	// document. Versions increase by one with every committed batch, across
	// the whole database, so they order all writes.
	Version int64
}
```

Things to notice:

- The type comes **after** the name: `Version int64`, not `int64 Version`.
  This holds everywhere in Go: variables, fields, parameters, results.
- `import` takes a list in parentheses. An unused import is a compile error,
  not a warning. Your editor adds and removes imports for you.
- `Document` and all its fields start with an upper-case letter, so other
  packages can use them.
- The comment above `Document` starts with the word `Document`. That is the
  convention `go doc` relies on.

## Structs

A **struct** is a fixed set of named fields, like a record or a plain data
class. `Document` above is one. You create a struct value with a *composite
literal* that names the fields. This line is from `stage` in
[`internal/app/write.go`](../../internal/app/write.go):

```go
updated = &domain.Document{Path: w.Path, Data: data, CreateTime: now, UpdateTime: now, Version: version}
```

Fields you leave out get their zero value (explained below). The `&` in front
takes the address of the new struct, so `updated` is a pointer to a
`Document`, written `*domain.Document`.

A struct is a **value**. Assigning it or passing it to a function copies all
its fields. If a function should see the caller's struct instead of a copy,
or if the struct is large, you pass a pointer. There are no hidden references
as in Java or Python: you can always see from the type whether you hold the
thing itself (`Document`) or a pointer to it (`*Document`).

A common mistake coming from other languages is to expect a struct copy to be
"deep". It is not: the copy has its own `Version`, but its `Data` field is a
map, and a map value refers to shared storage. Both copies see the same map
contents. This matters later in this chapter.

## Named types

`type` can also give a new name to an existing type. The new type has the
same representation but is a **different type** to the compiler. From
[`internal/domain/path.go`](../../internal/domain/path.go):

```go
// DocPath is the validated path of a document, such as "users/alice" or
// "users/alice/posts/p1". It always has an even number of segments:
// collection, document, collection, document...
//
// It is a named string type, so it is cheap to copy, comparable and usable
// as a map key, yet the compiler stops a raw string or a CollectionPath from
// being passed where a document path is expected.
type DocPath string

// CollectionPath is the validated path of a collection, such as "users" or
// "users/alice/posts". It always has an odd number of segments.
type CollectionPath string
```

A `DocPath` is stored exactly like a string. It can be compared with `==`,
used as a map key and converted to and from `string` at no cost. But a
function that takes a `DocPath` will not accept a `string` variable or a
`CollectionPath`. You must convert explicitly, with `DocPath(s)`.

That small friction is the point. The only place that converts an untrusted
string is `ParseDocPath`, which validates it first. Everywhere else, holding
a `DocPath` means "this has been checked". The compiler now enforces a rule
that would otherwise be a comment.

The exception worth knowing: an *untyped constant* such as the literal
`"users/alice"` is accepted wherever a `DocPath` is expected, because a
literal has no fixed type yet. The tests use this (`Path: "c/d"`). It is
convenient in tests and a reason to route real input through the parser.

## Functions with several results

A Go function can return more than one value. The parser for document paths
returns the path and an error:

```go
func ParseDocPath(s string) (DocPath, error) {
	n, err := validatePath(s)
	if err != nil {
		return "", err
	}
	if n%2 != 0 {
		return "", fmt.Errorf("%w: %q is a collection path, not a document path", ErrInvalidArgument, s)
	}
	return DocPath(s), nil
}
```

- `n, err := validatePath(s)` declares two variables and assigns both. `:=`
  is the *short variable declaration*: it declares and infers the types from
  the right-hand side. It only works inside functions.
- The result list `(DocPath, error)` is the usual shape of a function that
  can fail: the answer, and an `error` that is `nil` on success. Chapter 02
  is about that pattern.
- On failure the function returns `""`, the zero value of `DocPath`, with
  the error. Callers must not use the first result when the error is not nil.

Results can be named, as in `validatePath` right below it in the same file:
`(segments int, err error)`. Named results document what the values mean and
are ordinary variables inside the function.

## Methods and receivers

A **method** is a function attached to a type. The type appears in
parentheses before the method name and is called the **receiver**. Any named
type you declare in your package can have methods, not only structs:

```go
// Parent returns the collection the document lives in.
func (p DocPath) Parent() CollectionPath {
	return CollectionPath(p[:strings.LastIndexByte(string(p), '/')])
}

// ID returns the last segment of the path, the document's ID within its
// collection.
func (p DocPath) ID() string {
	return string(p[strings.LastIndexByte(string(p), '/')+1:])
}
```

`p` is the receiver: the path the method was called on, as in
`path.Parent()`. There is no `this` or `self` keyword; you choose the name,
and by convention it is one or two letters derived from the type.

`p[:i]` is a *slice expression*: the part of the string from the start up to
index `i`. Slicing a string gives a string that shares the same bytes, so no
copying happens. The result is converted to `CollectionPath`, because the
parent of a document is a collection.

### Value receivers and pointer receivers

A receiver is either a value (`p DocPath`) or a pointer (`lx *lexer`). With a
value receiver the method gets a **copy** and cannot change the original.
With a pointer receiver it gets the address and can.

The domain types use value receivers, because their methods only read. The
rules lexer, which keeps a position while it walks through text, uses pointer
receivers. From [`internal/rules/lexer.go`](../../internal/rules/lexer.go):

```go
func (lx *lexer) advance() rune {
	r, size := utf8.DecodeRuneInString(lx.src[lx.off:])
	lx.off += size
	if r == '\n' {
		lx.line++
		lx.col = 1
	} else {
		lx.col++
	}
	return r
}
```

`lx.off += size` changes the lexer the caller holds. If the receiver were
`lx lexer`, the method would advance a copy, return, and the caller's lexer
would not have moved. That is the classic mistake: a method with a value
receiver that assigns to a field compiles without complaint and does nothing
useful.

The rule of thumb used in this codebase: if any method of a type needs a
pointer receiver, or the type contains a lock, give all its methods pointer
receivers. Small immutable values (`DocPath`, `Timestamp`, `Filter`) use
value receivers.

Note also `r, size := utf8.DecodeRuneInString(...)`. A **rune** is Go's name
for a Unicode code point (an `int32`). Strings are sequences of bytes in
UTF-8; this call decodes the next code point and says how many bytes it took.

## Constants and iota

A `const` block can number its entries automatically with `iota`, which is 0
on the first line and increases by one per line. This is how Go spells an
enumeration. From [`internal/domain/value.go`](../../internal/domain/value.go):

```go
// Kind identifies the type class of a Value. The numeric order of the
// constants is the order values of different kinds sort in, so a query that
// orders by a field holding mixed types still has one well-defined order.
type Kind int

const (
	KindNull Kind = iota
	KindBool
	KindNumber
	KindTimestamp
	KindString
	KindArray
	KindMap
)
```

`Kind` is a named type based on `int`. `KindNull` is `Kind(0)`, `KindBool` is
`Kind(1)`, and so on; a line that gives no value repeats the expression of
the line above with the next `iota`.

Here the numbers are not arbitrary. The comment says the order of the
constants is the order in which values of different kinds sort, and `Compare`
uses the numbers directly. When the order of an enumeration carries meaning,
say so in a comment, as this one does, because inserting a constant in the
middle changes the value of every later one.

Go enumerations are plain integers. Nothing stops `Kind(42)` from existing,
and a `switch` over a `Kind` is not checked for completeness. That is why
`Kind.String`, just below the constants, ends with `return "unknown"`.

## Zero values

Every type has a **zero value**, and a variable that is declared without a
value holds it: `0` for numbers, `""` for strings, `false` for booleans,
`nil` for pointers, slices, maps, interfaces and functions, and for a struct
the struct whose fields are all zero. There is no "uninitialised" state.

Good Go types are designed so that the zero value means something sensible.
From [`internal/domain/write.go`](../../internal/domain/write.go):

```go
// Precondition makes a write conditional. The zero value imposes nothing.
type Precondition struct {
	// Exists, when set, requires the document to exist (true) or not (false).
	Exists *bool
	// Version, when set, requires the stored document to be at exactly this
	// version. It is how clients build optimistic transactions: read, then
	// write only if nothing changed in between.
	Version *int64
}
```

A `Precondition{}` with nothing set imposes no condition, so a `Write` that
does not mention its `Pre` field is unconditional without any extra code.

The fields are pointers for a reason. A plain `bool` cannot tell "must not
exist" (`false`) from "no requirement" (also `false`, the zero value). A
`*bool` has three states: `nil`, pointing at `true`, pointing at `false`. Use
a pointer when "absent" must differ from the zero value.

## Slices

A **slice** is a growable sequence: `[]string` is a slice of strings. It is
the list type you use daily. A slice value is a small header (a pointer to an
array, a length and a capacity), so passing a slice does not copy the
elements.

```go
// Keys returns the map's field names in sorted order.
func (m Map) Keys() []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
```

- `make([]string, 0, len(m))` creates a slice of length 0 with room for
  `len(m)` elements. Stating the capacity up front avoids repeated growth.
- `append` adds an element and returns the resulting slice. You must assign
  the result (`keys = append(keys, k)`), because when the capacity is used up
  `append` allocates a larger array and returns a slice that points to it.
  Writing `append(keys, k)` alone is a compile error, which protects you from
  the most common slice mistake.
- `for k := range m` iterates over the keys of a map. With two variables,
  `for k, v := range m`, you get the values as well. Over a slice, `range`
  yields the index and the element.
- `slices.Sort` comes from the standard `slices` package, which holds the
  general-purpose slice functions (sort, search, contains, reverse).

Because slice headers share their array, two slices can alias the same
elements. Look at how `ArrayUnion` avoids changing its input:

```go
func (t ArrayUnion) Apply(prev Value, _ time.Time) (Value, bool) {
	old, _ := prev.(Array)
	out := make(Array, len(old), len(old)+len(t.Elements))
	copy(out, old)
	for _, e := range t.Elements {
		if !out.Contains(e) {
			out = append(out, e)
		}
	}
	return out, false
}
```

`old, _ := prev.(Array)` is explained in the section on type assertions; it
yields the previous list, or `nil` if the field was not a list. The function
then makes a **new** slice and copies the old elements into it before
appending. Appending straight to `old` could write into the array of the
stored document. The whole design of this package rests on never doing that.

A `nil` slice is a valid empty slice: `len` is 0, `range` runs zero times and
`append` works on it. You rarely need to check a slice for `nil`.

## Maps

A **map** is a hash table: `map[string]Value` maps strings to `Value`s. The
`Map` type of a document's data is exactly that with a name, declared in the
type group shown in the next section.

Three facts about maps cover most of what you need:

- Reading a missing key returns the zero value of the element type, not an
  error. To tell "missing" from "present with a zero value", use the two-result
  form `v, ok := m[key]`.
- Iteration order is random, deliberately, and differs from run to run. Code
  that needs a stable order must sort the keys, which is what `Keys` above is
  for.
- A `nil` map can be read (every key is missing, `len` is 0, `range` runs
  zero times) but **writing to a nil map panics**. Create maps with `make` or
  a literal before storing into them.

The field-path lookup shows the two-result read:

```go
// Get follows the field path into the map. The boolean reports whether the
// field exists; a field that exists and holds null returns (Null{}, true).
func (m Map) Get(fp FieldPath) (Value, bool) {
	var cur Value = m
	for _, name := range fp {
		asMap, ok := cur.(Map)
		if !ok {
			return nil, false
		}
		if cur, ok = asMap[name]; !ok {
			return nil, false
		}
	}
	return cur, true
}
```

`if cur, ok = asMap[name]; !ok` assigns and tests in one statement: the part
before the semicolon runs first, and the condition after it decides. This
"statement; condition" form of `if` appears on almost every page of Go code.

## Interfaces as a closed set of types

A document value can be one of eight things. In a language with tagged unions
you would declare one. Go has no such construct; the idiom is an **interface**
that only a fixed set of types implements.

An interface is a list of methods. Any type that has those methods satisfies
the interface automatically; there is no `implements` keyword. (Chapter 04
covers interfaces in depth.) Here is `Value`:

```go
// Value is one field value inside a document. It is a closed set of types:
// the unexported method means no other package can add an implementation,
// so a type switch over the types below is exhaustive.
//
// Values are treated as immutable. Code that needs a changed value builds a
// new one; it never modifies an Array or Map it was handed.
type Value interface {
	Kind() Kind
	sealed()
}
```

`Kind()` is an ordinary method. `sealed()` is the trick: its name is
lower-case, so it is unexported, so **no type outside this package can have
it**, so no type outside this package can be a `Value`. The set is closed.

The eight types are declared together, each on top of a suitable basic type:

```go
type (
	// Null is the JSON null value.
	Null struct{}
	// Bool is a boolean.
	Bool bool
	// Int is a 64-bit integer.
	Int int64
	// Double is a finite 64-bit float. NaN and infinities are rejected at
	// the edges of the system because JSON cannot carry them.
	Double float64
	// Timestamp is an instant in UTC with microsecond precision.
	Timestamp struct{ t time.Time }
	// String is UTF-8 text.
	String string
	// Array is an ordered list of values.
	Array []Value
	// Map is a set of named fields.
	Map map[string]Value
)
```

and each gets the two methods, one line apiece. Here are the first four
`Kind` methods; the `sealed` methods below them in the file look the same:

```go
func (Null) Kind() Kind      { return KindNull }
func (Bool) Kind() Kind      { return KindBool }
func (Int) Kind() Kind       { return KindNumber }
func (Double) Kind() Kind    { return KindNumber }
```

A receiver with no name, `(Null)`, is allowed when the method does not use
it. `Bool(true)`, `Int(42)` and `String("x")` are conversions from basic
values to the named types, and `Array{Int(1), Int(2)}` and
`Map{"a": Int(1)}` are composite literals, the same notation as for a plain
slice or map.

`Timestamp` is a struct with one unexported field, `t`. Code outside the
package cannot build one directly, so it must go through the constructor,
which normalises the time:

```go
// NewTimestamp converts t to a Timestamp, normalising it to UTC and
// truncating it to microseconds so that it survives a round trip through
// storage and JSON unchanged.
func NewTimestamp(t time.Time) Timestamp {
	return Timestamp{t: t.UTC().Truncate(time.Microsecond)}
}

// Time returns the instant as a time.Time in UTC.
func (ts Timestamp) Time() time.Time { return ts.t }
```

Go has no constructors as a language feature. A function named `New...` that
returns the type is the convention, and an unexported field is how you make
the constructor the only way in.

## Type switches

Given a `Value`, how does code find out which of the eight types it holds? A
**type switch** branches on the dynamic type of an interface value:

```go
func Size(v Value) int {
	switch x := v.(type) {
	case Null, Bool:
		return 1
	case Int, Double, Timestamp:
		return 8
	case String:
		return len(x) + 1
	case Array:
		n := 1
		for _, e := range x {
			n += Size(e)
		}
		return n
	case Map:
		n := 1
		for k, e := range x {
			n += len(k) + 1 + Size(e)
		}
		return n
	}
	return 0
}
```

`switch x := v.(type)` declares `x`, and in each `case` it has that case's
type: in `case String`, `x` is a `String`, so `len(x)` compiles. In a case
that lists several types, such as `case Null, Bool`, `x` keeps the interface
type `Value`, because no single concrete type fits.

`Size` calls itself for the elements of arrays and maps. Recursion over a
tree of values with a type switch at each node is the main pattern of this
package (`Compare`, `Depth` and the JSON conversion have the same shape) and
of the rules interpreter in chapter 08.

The compiler does not check that a type switch covers every type. `Size`
ends with `return 0` for that reason, and `Compare` ends with
`panic("domain: unknown value type")`, because reaching that line means a new
value type was added without teaching `Compare` about it: a programming
error that should be loud. Because `Value` is sealed, you can find every
place to update by searching this module for `.(type)`.

## Type assertions

When you expect one particular type, a **type assertion** extracts it:
`v.(Map)`. The one-result form panics if `v` holds something else. The
two-result form reports it instead:

```go
asMap, ok := cur.(Map)
```

That line is from `Map.Get` above: if the current value is not a map, the
path cannot continue and the field does not exist.

`Map.With` uses the two-result form in a way that is easy to miss:

```go
// With returns a copy of the map in which the field at fp holds v. Only the
// maps along the path are copied; everything else is shared with the
// original, which is safe because values are never modified in place.
// Intermediate fields that are missing, or are not maps, become maps.
func (m Map) With(fp FieldPath, v Value) Map {
	out := make(Map, len(m)+1)
	for k, e := range m {
		out[k] = e
	}
	if len(fp) == 1 {
		out[fp[0]] = v
		return out
	}
	child, _ := m[fp[0]].(Map)
	out[fp[0]] = child.With(fp[1:], v)
	return out
}
```

`child, _ := m[fp[0]].(Map)` does three things. It reads the field, which may
be missing. It asserts that the value is a `Map`. And the blank identifier
`_` discards the `ok` result. If the field is missing or is not a map, the
assertion fails quietly and `child` is the zero value of `Map`: a nil map.
The recursive call `child.With(...)` is still fine, because calling a method
on a nil map is allowed and `With` only reads its receiver. The result is
that missing intermediate maps are created, in one line, with no special
case. Using the one-result form `m[fp[0]].(Map)` here would be the mistake:
it panics as soon as the field holds a number.

## Immutability and copy-on-write

Go has no `const` or `readonly` for data structures. This package adopts a
rule instead, stated in the comment on `Value`: **values are never modified
after they are built**. Code that needs a changed value builds a new one.

The rule exists because values are shared. The in-memory store hands the same
`Map` to every reader, and the realtime system passes one document to many
listeners in different goroutines. If any of them wrote into it, the others
would see a document change under their hands.

`Map.With`, shown above, is how a change is made under that rule. It copies
the top-level map (`out := make(Map, len(m)+1)` and the loop), then replaces
one entry. For a nested path it recurses, so exactly the maps *along the
path* are copied. Every other entry of `out` is the same value as in `m`,
shared, which is safe because nobody modifies it. This technique is called
**copy-on-write**: the cost of an update is proportional to the depth of the
path, not to the size of the document.

`Write.Apply` in `write.go` builds on this. It starts from the stored data
and calls `With` or `Without` once per field operation; the stored document
is never touched. The test `TestWriteApply` checks that explicitly by
comparing the stored document before and after.

Since the compiler cannot enforce the rule, three things keep it true: the
comment, small helper methods that are the only code that "changes" maps,
and tests that would notice.

## Transforms: several types, one method

`write.go` has a second small interface, with ordinary exported methods:

```go
// Transform computes the new value of a field from its previous value. Each
// implementation is a small command object: the data it needs travels with
// it and Apply carries it out.
type Transform interface {
	// Apply returns the new value. prev is nil when the field does not
	// exist. Returning remove=true deletes the field.
	Apply(prev Value, now time.Time) (v Value, remove bool)
}
```

Six types implement it. Three need one line each:

```go
func (t SetValue) Apply(Value, time.Time) (Value, bool)            { return t.Value, false }
func (DeleteField) Apply(Value, time.Time) (Value, bool)           { return nil, true }
func (ServerTimestamp) Apply(_ Value, now time.Time) (Value, bool) { return NewTimestamp(now), false }
```

An unused parameter can be left unnamed (`Value, time.Time`) or named `_`.
`SetValue` ignores both inputs and returns the value it carries.
`DeleteField` returns `remove = true`. You will add a seventh type in the
exercise.

## Closures

A function can be written inline and stored in a variable. Such a function
can use the variables of the function around it; it is then called a
**closure**. From
[`internal/adapters/sqlite/store.go`](../../internal/adapters/sqlite/store.go):

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

`visit` appends to `entries`, a variable of the enclosing function. It does
not get a copy; it refers to the same variable, so when the outer loop is
done `entries` holds everything `visit` collected.

The declaration is split in two (`var visit func(...)`, then `visit = ...`)
because `visit` calls itself, and with `visit := func...` the name would not
yet exist inside the function body.

Closures are everywhere in Go: as callbacks, as HTTP middleware (chapter 05)
and as the body of goroutines (chapter 09).

## Small things you will see in this package

- **`min` and `max`** are built-in functions for any ordered type:
  `max(deepest, Depth(e)+1)` in `Depth`.
- **Ranging over an integer.** `for i := range n` runs with `i` from 0 to
  `n-1`. `Compare` uses it to walk two arrays up to the shorter length:

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

  `b.(Array)` is a one-result assertion. It cannot fail here: the function
  has already established that `a` and `b` are of the same kind, and `Array`
  is the only type of that kind.
- **`if` with an initialiser**, as in `if c := Compare(av[i], bv[i]); c != 0`,
  limits the variable `c` to the `if` statement.
- **Conversions are always explicit.** `float64(n)` converts an `Int` to a
  float. Go never converts between numeric types on its own, not even from
  `int32` to `int64`.

## Try it

From the repository root:

```sh
go doc ./internal/domain Value
go test ./internal/domain -run TestCompareOrder -v
go test ./internal/domain -run TestFieldPathGetWithWithout -v
go test ./internal/domain -run 'TestWriteApply/increment' -v
```

Then open
[`internal/domain/path_test.go`](../../internal/domain/path_test.go) and find
`TestFieldPathGetWithWithout`. In `Map.With` in `path.go`, change the first
lines so that the method modifies its receiver instead of a copy: replace
`out := make(Map, len(m)+1)` and the loop that follows with `out := m`. Run
the test again. Two things happen. The test reports "With modified its
receiver: Paris", which is what the immutability rule looks like when it is
broken. Then the test panics with "assignment to entry in nil map", because
without `make` the method now writes into the nil map described in the
section on type assertions. Undo the change.

## Exercise: a "maximum" transform

**Goal.** Add a transform that raises a numeric field to a given value but
never lowers it. If the field holds 3 and the transform says 5, the field
becomes 5; if the field holds 8, it stays 8. A typical use is a high score.

**What to do.**

1. In `internal/domain/write.go`, declare `type Maximum struct{ To Value }`
   next to `Increment`, with a doc comment.
2. Give it an `Apply(prev Value, _ time.Time) (Value, bool)` method so that
   it satisfies `Transform`.
3. Decide and document what happens when the field is missing or is not a
   number. `Increment` treats that case as "start from the operand"; being
   consistent with it is a good choice.
4. In `internal/domain/write_test.go`, add cases to the table in
   `TestWriteApply`: field lower than `To`, field higher, field missing,
   field holding a string, an `Int` field with a `Double` operand.

**Hints.**

- `Compare` already orders `Int` and `Double` against each other. You do not
  need arithmetic.
- `prev` is `nil` when the field does not exist. Calling a method on a nil
  interface value panics, so test for it first. A type switch with a
  `case Int, Double:` handles "is it a number" and nil in one construct,
  because nil matches no case.
- Return the larger of the two values **unchanged**. Do not convert an `Int`
  to a `Double`.

**How you know it works.** `go test ./internal/domain` passes with your new
cases, and `go vet ./...` is silent. Chapter 05's exercise makes the
transform reachable over HTTP, so keep the code.

## Further reading

- [A Tour of Go: Basics and "More types"](https://go.dev/tour/): variables,
  structs, slices and maps, interactively.
- [Go Slices: usage and internals](https://go.dev/blog/slices-intro): what a
  slice header is and why `append` returns a slice.
- [Effective Go: Data](https://go.dev/doc/effective_go#data): `new`, `make`,
  composite literals, slices and maps.
- [The Go Programming Language Specification: Type switches](https://go.dev/ref/spec#Type_switches)
- [Package slices](https://pkg.go.dev/slices)

**Next:** [02 · Errors](02-errors.md)
