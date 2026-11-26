# 8. An interpreter for the rules language

The security rules are written in a small language of their own. This
chapter walks through the program that reads and runs that language, which
is a compact tour of strings, recursion, and functions as values.

## What you will learn

- The difference between bytes and runes, and how to walk a string character by character.
- How a hand-written lexer and a recursive-descent parser are organised.
- Precedence climbing: parsing `a || b && c == 1` correctly with one loop and one table.
- A syntax tree as a closed set of node types, walked with a type switch.
- Maps of functions as dispatch tables, and closures that build functions.
- Why recursion needs a depth limit when the input comes from outside.
- `regexp`, and why Go's implementation is safe to expose to user input.
- A mutex around a package-level cache, and `atomic.Pointer` for replacing a value that many goroutines read.

## The feature

An operator describes who may read and write which documents in a rules
file ([tour: security rules](../tour/07-security-rules.md)):

```text
match /notes/{id} {
  allow read: if resource.data.owner == request.auth.uid;
}
```

The server parses the file once at start-up, and again on reload. For every
request it evaluates the conditions of the matching `allow` statements
against that request. The package that does both is `internal/rules`. It
has no dependency on HTTP or storage. It works in three stages:

    source text --lexer--> tokens --parser--> syntax tree --evaluator--> decision

## Where the code is

- [`internal/rules/lexer.go`](../../internal/rules/lexer.go): text to tokens.
- [`internal/rules/ast.go`](../../internal/rules/ast.go): the types of the syntax tree.
- [`internal/rules/parser.go`](../../internal/rules/parser.go): tokens to tree.
- [`internal/rules/check.go`](../../internal/rules/check.go): a pass over the finished tree that rejects unknown names.
- [`internal/rules/eval.go`](../../internal/rules/eval.go): matching a path against the rules and evaluating conditions.
- [`internal/rules/builtins.go`](../../internal/rules/builtins.go): built-in functions, methods and type names.
- [`internal/app/service.go`](../../internal/app/service.go): where the parsed rules are held and replaced.

## Bytes and runes

A Go string is a sequence of bytes, by convention the UTF-8 encoding of
text. In UTF-8 a character takes between one and four bytes: `a` is one
byte, `é` is two, `日` is three. Go calls a character a *rune*; the type
`rune` is another name for `int32` and holds a Unicode code point.

So `len(s)` is a number of bytes and `s[i]` is a byte. To read characters
you have to decode. The lexer does it explicitly:

From `internal/rules/lexer.go`:

```go
// peek returns the next rune without consuming it, or 0 at the end.
func (lx *lexer) peek() rune {
	if lx.off >= len(lx.src) {
		return 0
	}
	r, _ := utf8.DecodeRuneInString(lx.src[lx.off:])
	return r
}
```

`utf8.DecodeRuneInString` reads one character from the start of a string
and also returns how many bytes it occupied. `peek` discards that size
because it only looks. `advance` uses it to move forward:

From `internal/rules/lexer.go`:

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

`lx.off` is a byte offset, and it moves by `size` bytes, not by one. That
is the detail that makes the lexer correct for any text.

`'\n'` in single quotes is a rune constant. Double quotes make a string.

The `lexer` struct holds the source and a position in it. `advance` keeps
the line and column up to date, so every token can carry the position that
error messages report.

**Typical mistake:** `for i := 0; i < len(s); i++ { c := s[i] ... }` on
text. It works for ASCII and cuts other characters into pieces. A
`for _, r := range s` loop decodes runes for you; the lexer uses the
explicit calls because it needs to stop and resume in the middle.

## Tokens

A lexer groups characters into *tokens*, the words of the language:
identifiers, numbers, strings, punctuation.

From `internal/rules/lexer.go`:

```go
type token struct {
	kind tokenKind
	text string // for tokString this is the decoded value
	pos  Pos
}
```

The kinds are a list of constants declared with `iota`, as in
[chapter 1](01-types-and-values.md). The parser works on tokens and never
looks at characters or whitespace again.

From `internal/rules/lexer.go`:

```go
func (lx *lexer) next() (token, error) {
	lx.skipSpaceAndComments()
	pos := lx.pos()
	if lx.off >= len(lx.src) {
		return token{kind: tokEOF, pos: pos}, nil
	}
	r := lx.peek()
	switch {
	case r == '_' || unicode.IsLetter(r):
		start := lx.off
		for r := lx.peek(); r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r); r = lx.peek() {
			lx.advance()
		}
		return token{tokIdent, lx.src[start:lx.off], pos}, nil
	case r >= '0' && r <= '9':
		return lx.number(pos), nil
	case r == '"' || r == '\'':
		return lx.str(pos)
```

`next` skips what does not matter, looks at one character, and decides
which kind of token starts there. A `switch` with no value after the
keyword is Go's form of an if/else-if chain: the first `case` whose
condition is true runs.

`lx.src[start:lx.off]` slices the source string. Slicing a string does not
copy; the token's text shares the bytes of the source.

Punctuation is tried from a list in which longer operators come first, so
`==` is recognised before `=`:

From `internal/rules/lexer.go`:

```go
	for _, p := range punctuation {
		if strings.HasPrefix(lx.src[lx.off:], p) {
			for range p {
				lx.advance()
			}
			return token{tokPunct, p, pos}, nil
		}
	}
	return token{}, lx.errorf(pos, "unexpected character %q", r)
```

`for range p` runs once per character of `p` without using the character.
The verb `%q` prints a value in quotes, with unprintable characters escaped.

## Recursive descent: one method per grammar rule

The grammar of the rules language is written in comments above the parser's
methods, for example `allow := "allow" NAME ("," NAME)* (":" "if" expr)? ";"`.
A *recursive-descent* parser turns each such rule into a method that
consumes exactly the tokens of that rule and returns its tree. Rules that
refer to other rules become calls to other methods.

Two small helpers do most of the token handling:

From `internal/rules/parser.go`:

```go
// accept consumes the next token if it is the given punctuation.
func (p *parser) accept(punct string) bool {
	if p.peekIs(tokPunct, punct) {
		p.i++
		return true
	}
	return false
}
```

From `internal/rules/parser.go`:

```go
func (p *parser) expect(punct string) error {
	if !p.accept(punct) {
		return p.errorf(p.peek().pos, "expected %q, found %s", punct, p.peek())
	}
	return nil
}
```

`accept` is "if the next token is this, take it and say so". `expect` is
"the next token must be this". With them, a rule for a comma-separated list
reads almost like its grammar:

From `internal/rules/parser.go`:

```go
// exprList parses comma-separated expressions up to the closing bracket.
func (p *parser) exprList(closing string) ([]expr, error) {
	var list []expr
	for !p.accept(closing) {
		if len(list) > 0 {
			if err := p.expect(","); err != nil {
				return nil, err
			}
		}
		e, err := p.expr()
		if err != nil {
			return nil, err
		}
		list = append(list, e)
	}
	return list, nil
}
```

Errors are returned, never thrown. Every call that can fail is followed by
`if err != nil { return nil, err }`, and the error travels up through the
methods to `Parse`. The error type is `*SyntaxError`, which carries the
position; custom error types are covered in [chapter 2](02-errors.md).

## Precedence climbing

Binary operators need one more idea. `a || b && c` must group as
`a || (b && c)`, because `&&` binds tighter. The usual textbook answer is a
grammar rule and a method per precedence level. This parser uses a table
and a single method instead:

From `internal/rules/parser.go`:

```go
var precedence = map[string]int{
	"||": 1,
	"&&": 2,
	"==": 3, "!=": 3,
	"<": 4, "<=": 4, ">": 4, ">=": 4, "in": 4, "is": 4,
	"+": 5, "-": 5,
	"*": 6, "/": 6, "%": 6,
}
```

From `internal/rules/parser.go`:

```go
func (p *parser) binary(minPrec int) (expr, error) {
	left, err := p.unary()
	if err != nil {
		return nil, err
	}
	for {
		op := p.peek()
		prec, isOperator := precedence[op.text]
		// "in" and "is" are words, the rest punctuation; a string literal
		// that happens to spell an operator is neither.
		if !isOperator || prec < minPrec || (op.kind != tokPunct && op.kind != tokIdent) {
			return left, nil
		}
		p.advance()
```

The rest of the loop, for an ordinary operator:

From `internal/rules/parser.go`:

```go
		// prec+1 makes operators of equal precedence group to the left:
		// a - b - c is (a - b) - c.
		right, err := p.binary(prec + 1)
		if err != nil {
			return nil, err
		}
		left = &binaryExpr{pos: op.pos, op: op.text, l: left, r: right}
```

`binary(minPrec)` parses one operand, then keeps absorbing
"operator, operand" pairs as long as the operator's precedence is at least
`minPrec`. The right-hand operand is parsed by a recursive call with a
higher minimum, so that call will take any tighter-binding operators that
follow and leave the rest.

Follow `a || b && c == 1`. Parsing starts with `binary(1)`.

1. `binary(1)` parses the operand `a`. The next token is `||`, precedence
   1. That is at least 1, so it is consumed, and the right side is parsed
   by `binary(2)`.
2. `binary(2)` parses `b`. Next is `&&`, precedence 2, at least 2: consumed.
   The right side is `binary(3)`.
3. `binary(3)` parses `c`. Next is `==`, precedence 3, at least 3: consumed.
   The right side is `binary(4)`.
4. `binary(4)` parses `1`. The next token is not an operator, so it returns
   `1`.
5. Back in `binary(3)`: `left` becomes `c == 1`. No operator follows; it
   returns.
6. Back in `binary(2)`: `left` becomes `b && (c == 1)`; it returns.
7. Back in `binary(1)`: `left` becomes `a || (b && (c == 1))`.

Now the opposite order, `a && b || c`. `binary(1)` parses `a`, consumes
`&&` and calls `binary(3)`. That call parses `b` and sees `||` with
precedence 1, which is below 3, so it returns just `b`. `binary(1)` builds
`a && b`, loops, now sees `||` with precedence 1, consumes it, and ends
with `(a && b) || c`. The low-precedence operator was refused by the inner
call and picked up by the outer one.

Looking up `precedence[op.text]` uses the two-result form of a map read.
The second result says whether the key was present, which here means "is
this token an operator at all".

## The syntax tree: a closed set of node types

The parser's output is a tree. Each node is a struct, and all node types
share one small interface:

From `internal/rules/ast.go`:

```go
// expr is a node of an expression's syntax tree. Like domain.Value it is a
// closed set of types that the evaluator walks with a type switch.
type expr interface{ position() Pos }
```

From `internal/rules/ast.go`:

```go
	// binaryExpr is "l op r" for every two-sided operator.
	binaryExpr struct {
		pos  Pos
		op   string
		l, r expr
	}
```

A `binaryExpr` holds two more `expr` values, which may be binary
expressions themselves. That self-reference through an interface is what
makes the structure a tree. The parser stores pointers (`&binaryExpr{...}`),
so the nodes are shared, not copied, as they are passed around.

This is the same design as `domain.Value` in
[chapter 1](01-types-and-values.md): a fixed list of types behind an
interface, processed with a type switch. The interface method `position`
is unexported, so no other package can add a node type.

## Recursion needs a limit

Parsing `((((1))))` calls `unary`, `postfix`, `primary`, `expr` and
`binary` once per bracket. Each call uses stack space. A rules file with a
hundred thousand brackets would grow the stack until the program hits its
limit and dies, and a Go program cannot recover from that.

From `internal/rules/parser.go`:

```go
func (p *parser) unary() (expr, error) {
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > maxNesting {
		return nil, p.errorf(p.peek().pos, "expression is nested too deeply")
```

A counter goes up on the way in and down on the way out. `defer` runs the
decrement whenever `unary` returns, on every path, without it being
repeated before each `return`.

The evaluator has the same kind of guard for user-defined functions that
call themselves (`maxCallDepth`), and a cap on document lookups per request
(`maxLookups`).

**Typical mistake:** assuming input is reasonable. Any recursive function
whose depth is controlled by outside input needs a bound.

## A checking pass

After parsing, `check` walks the whole tree once and verifies that every
variable and function an expression mentions exists where it is used.

From `internal/rules/check.go`:

```go
	case *identExpr:
		if !vars[e.name] {
			return &SyntaxError{e.pos, "unknown name " + e.name}
		}
	case *memberExpr:
		return c.expr(e.x, vars, funcs)
```

`vars` is the set of names in scope: `request`, `resource`, the wildcards
of the enclosing `match` blocks, and inside a function its parameters.
The benefit is in when the error appears. Without this pass a misspelt
name would be an evaluation error, an evaluation error counts as "deny",
and the operator would only see requests failing. With it, the server
refuses to load the file and names the line and column.

## The evaluator is a type switch

From `internal/rules/eval.go`:

```go
func (ev *evaluator) eval(e expr, sc *scope) (domain.Value, error) {
	switch e := e.(type) {
	case *literalExpr:
		return e.value, nil

	case *identExpr:
		if v, ok := sc.vars[e.name]; ok {
			return v, nil
		}
		return nil, evalErr(e.pos, "unknown name %s", e.name)
```

`eval` takes a node and returns its value. `switch e := e.(type)` selects
on the node's concrete type, and inside each case `e` has that type, so
`e.value` and `e.name` are available without further conversion.

Cases for nodes with children call `eval` on the children first:

From `internal/rules/eval.go`:

```go
	case *memberExpr:
		x, err := ev.eval(e.x, sc)
		if err != nil {
			return nil, err
		}
		m, ok := x.(domain.Map)
		if !ok {
			return nil, evalErr(e.pos, "cannot read field %s of a %s", e.name, x.Kind())
		}
		v, ok := m[e.name]
		if !ok {
			return nil, evalErr(e.pos, "field %s does not exist", e.name)
		}
		return v, nil
```

To evaluate `x.name`: evaluate `x`, require a map, look up the field. Each
failure is an error with a position. The values are `domain.Value`s; the
rules engine reuses the database's own value types and its `Compare`, so
`==` in a rule means exactly what `==` means in a query.

The result of a condition is turned into a decision here:

From `internal/rules/eval.go`:

```go
func (ev *evaluator) holds(cond expr, sc *scope) bool {
	if cond == nil {
		return true
	}
	v, err := ev.eval(cond, sc)
	if err != nil {
		return false
	}
	b, ok := v.(domain.Bool)
	return ok && bool(b)
}
```

An error is a "no". A result that is not a boolean is a "no". Only `true`
allows. Security code should fail closed, and this function is where that
happens.

## Logical operators that forgive

`&&` and `||` get their own function because they must not evaluate both
sides blindly:

From `internal/rules/eval.go`:

```go
func (ev *evaluator) logical(e *binaryExpr, sc *scope) (domain.Value, error) {
	decisive := domain.Bool(e.op == "||") // true decides ||, false decides &&
	l, lerr := ev.evalBool(e.l, sc)
	if lerr == nil && l == decisive {
		return decisive, nil
	}
	r, rerr := ev.evalBool(e.r, sc)
	if rerr == nil && r == decisive {
		return decisive, nil
	}
	if lerr != nil {
		return nil, lerr
	}
	if rerr != nil {
		return nil, rerr
	}
	return !decisive, nil
}
```

`decisive` is the value that settles the operator on its own: `true` for
`||`, `false` for `&&`. If the left side evaluates cleanly to that value,
the right side is never evaluated (*short-circuiting*).

The function goes one step further. If the left side fails, the right side
is still tried, and if it is decisive the error is dropped. That is what
lets a rule say `resource == null || resource.data.owner == request.auth.uid`
with the two halves in either order. If neither side is decisive and one of
them failed, the error is returned, and `holds` turns it into a denial.

`domain.Bool(e.op == "||")` converts a Go `bool` to the domain's `Bool`
type. `l == decisive` then compares two values of that type.

## Dispatch tables: maps of functions

Functions are values in Go. They can be stored in variables, passed as
arguments, and put in maps. The built-in methods of the rules language
(`size`, `matches`, `hasAll`, ...) live in a map from name to function:

From `internal/rules/builtins.go`:

```go
// method is the signature of a built-in method such as "text".size().
type method func(recv domain.Value, args []domain.Value) (domain.Value, error)
```

From `internal/rules/builtins.go`:

```go
var methods = map[string]method{
	"size":    size,
	"keys":    keys,
	"values":  values,
	"get":     mapGet,
	"hasAll":  listTest(func(list, want domain.Array) bool { return all(want, list.Contains) }),
	"hasAny":  listTest(func(list, want domain.Array) bool { return anyOf(want, list.Contains) }),
	"hasOnly": listTest(func(list, want domain.Array) bool { return all(list, want.Contains) }),
	"matches": matches,
	"lower":   stringMap(strings.ToLower),
	"upper":   stringMap(strings.ToUpper),
	"trim":    stringMap(strings.TrimSpace),
}
```

The evaluator calls a method with `methods[fn.name](recv, args)`: look up
the function, call it. It contains no list of method names. Adding a method
to the language is adding a line to this map, and both the checker and the
evaluator pick it up.

`type method func(...)` gives the function signature a name. Function
types can be named like any other type.

Several entries are not function names but calls: `listTest(...)` and
`stringMap(...)`. Those calls run once, when the map is initialised, and
each returns a function:

From `internal/rules/builtins.go`:

```go
// listTest builds a method that takes one list argument and answers a
// yes/no question about the receiver list and that argument.
func listTest(test func(recv, arg domain.Array) bool) method {
	return func(recv domain.Value, args []domain.Value) (domain.Value, error) {
		list, ok := recv.(domain.Array)
		if !ok {
			return nil, errReceiver
		}
		if len(args) != 1 {
			return nil, errArguments
		}
		arg, ok := args[0].(domain.Array)
		if !ok {
			return nil, errArguments
		}
		return domain.Bool(test(list, arg)), nil
	}
}
```

`hasAll`, `hasAny` and `hasOnly` all need the same preamble: the receiver
must be a list, there must be exactly one argument, and it must be a list.
`listTest` writes the preamble once and takes the part that differs,
`test`, as a parameter. The function it returns is a closure: it keeps
using `test` after `listTest` has returned.

The types accepted by the `is` operator are a second table,
`typeNames`, from a name to a function that tests a value.

**Typical mistake:** a long `switch name { case "size": ... }` in the
evaluator. It works, but each new method means editing the evaluator, and
the checker needs a second copy of the list.

## Scopes and user functions

A rules file can define functions. Calling one means evaluating its body
with its parameters bound to the argument values:

From `internal/rules/eval.go`:

```go
		def := lookupFunction(sc.funcs, fn.name)
		if def == nil || len(def.params) != len(args) {
			return nil, evalErr(e.pos, "cannot call %s", fn.name)
		}
		if ev.depth >= maxCallDepth {
			return nil, evalErr(e.pos, "function calls are nested too deeply")
		}
		ev.depth++
		defer func() { ev.depth-- }()
		vars := make(map[string]domain.Value, len(sc.base.vars)+len(args))
		for k, v := range sc.base.vars {
			vars[k] = v
		}
		for i, name := range def.params {
			vars[name] = args[i]
		}
		return ev.eval(def.body, &scope{vars: vars, funcs: sc.funcs, base: sc.base})
```

A `scope` holds the variables an expression can see. For a call, a new map
is built from the variables of the `allow` statement being evaluated
(`sc.base.vars`) plus the parameters. It deliberately starts from `base`
and not from the caller's own scope, so a function sees the request, the
resource and the wildcards, but not the parameters of whichever function
called it.

`lookupFunction` searches a slice of maps from the last to the first: the
innermost `match` block's functions shadow the outer ones.

## Regular expressions

The `matches` method tests a string against a pattern. Patterns can come
from document data, so in principle a client can choose them.

Many regular expression engines work by backtracking and can take time
exponential in the input length for certain patterns. That has been the
cause of real outages. Go's `regexp` package uses a different algorithm and
guarantees running time linear in the size of the input. The price is that
some features, such as backreferences, are not supported. For a server that
runs patterns it did not write, that is the right trade.

From `internal/rules/builtins.go`:

```go
func compileCached(pattern string) (*regexp.Regexp, error) {
	regexCache.Lock()
	re, ok := regexCache.m[pattern]
	regexCache.Unlock()
	if ok {
		return re, nil
	}
	if len(pattern) > 1000 {
		return nil, errors.New("pattern is too long")
	}
	// Anchor the pattern so that it must match the whole string.
	re, err := regexp.Compile(`^(?:` + pattern + `)$`)
	if err != nil {
		return nil, errors.New("invalid regular expression")
	}
	regexCache.Lock()
	if len(regexCache.m) < maxCachedPatterns {
		regexCache.m[pattern] = re
	}
	regexCache.Unlock()
	return re, nil
}
```

`regexp.Compile` returns an error for an invalid pattern.
(`regexp.MustCompile` panics instead and is meant for patterns written in
the source code.) The pattern is wrapped in `^(?:...)$` so it must match
the whole string, and not merely occur somewhere in it.

## A mutex around a shared map

Compiling a pattern is much slower than using it, so compiled patterns are
cached in a package-level variable. Rules are evaluated by many goroutines
at once, and a Go map must not be read and written concurrently.

From `internal/rules/builtins.go`:

```go
var regexCache = struct {
	sync.Mutex
	m map[string]*regexp.Regexp
}{m: map[string]*regexp.Regexp{}}
```

This declares a variable of an anonymous struct type. `sync.Mutex` is
*embedded*: listed without a field name, so its methods become methods of
the struct, and the code can write `regexCache.Lock()`. Keeping the mutex
and the data it protects in one struct documents what the lock is for.

In `compileCached` the lock is held only around the two map operations and
released during the compilation. Two goroutines may therefore compile the
same new pattern at the same time. Both get a correct result and one
overwrites the other's cache entry, which is harmless and cheaper than
making every evaluation wait.

The cache stops growing at 256 entries, so a rule that builds patterns from
data cannot be used to consume memory without bound.

## Replacing the rules while requests run

The parsed `*Ruleset` is immutable: once `Parse` returns, nothing changes
it, so any number of goroutines can evaluate it without locks. But a reload
replaces it while requests are in flight.

From `internal/app/service.go`:

```go
	// rules is swapped atomically so that a reload never blocks or tears a
	// request that is being evaluated.
	rules atomic.Pointer[rules.Ruleset]
```

`atomic.Pointer[T]` is a pointer that can be read and replaced safely from
several goroutines without a mutex. It is a generic type from the standard
library, here instantiated for `rules.Ruleset`.

From `internal/app/service.go`:

```go
func (s *Service) SetRules(rs *rules.Ruleset) { s.rules.Store(rs) }
```

A request calls `s.rules.Load()` once and uses the ruleset it got for the
whole evaluation. A reload calls `Store` with a new one. A request sees the
old rules or the new rules, never a mixture, and neither waits for the
other. The pattern is: immutable data, replaced as a whole through an
atomic pointer.

## Try it

Check a valid rules file and two broken ones with the server binary:

```sh
./bin/candledb check-rules examples/todo/candledb.rules

printf 'match /notes/{id} {\n  allow read: if request.auth.uid == owner;\n}\n' > /tmp/bad.rules
./bin/candledb check-rules /tmp/bad.rules

printf 'match /notes/{id} {\n  allow read: if (1 + 2;\n}\n' > /tmp/bad2.rules
./bin/candledb check-rules /tmp/bad2.rules
```

The first prints `ok`. The second reports `rules:2:38: unknown name owner`:
the file parsed, and the checking pass rejected it. The third reports
`rules:2:24: expected ")", found ";"`, an error from `expect` in the parser.

Read the specification of the expression language as a test table, then run
it. Each line of the table is one expression and its expected result:

```sh
go test ./internal/rules -run TestExpressions -v
go test ./internal/rules -run 'TestParseErrors|TestSyntaxErrorPosition' -v
```

Let Go search for inputs that crash the parser or the evaluator:

```sh
go test ./internal/rules -run '^$' -fuzz FuzzParse -fuzztime 10s
```

The `execs` figure in the output is the number of generated inputs tried.
Fuzzing is introduced in [chapter 3](03-testing.md).

## Exercise

Add a string method `startsWith` to the rules language, so a rule can say
`request.resource.data.sku.startsWith("A-")`.

**Goal.** `"hello".startsWith("he")` is `true`, `"hello".startsWith("lo")`
is `false`, and calling it on a non-string or with a non-string argument is
an error (which a rule experiences as a denial).

**Files.** `internal/rules/builtins.go` and `internal/rules/rules_test.go`.
Nothing else.

**Hints.**

- Write the cases in the `TestExpressions` table first and watch them fail.
  Before your change, what kind of error does the new method name produce,
  and from which stage?
- `matches` is the closest existing method: one string receiver, one string
  argument, a boolean result.
- The standard library already has the function you need.
- If you find yourself editing `eval.go` or `check.go`, stop and reread the
  section on dispatch tables.

**How to know it works.** The new table cases pass, including one for a
non-string receiver, and `go test ./internal/rules` is green.

**Stretch.** Add a conditional operator, `cond ? a : b`. This one touches
every stage: the lexer needs `?` in `punctuation`; the parser needs to
decide where it sits relative to `||` (lower than everything, and note that
it is not a binary operator, so it does not belong in the `precedence`
table); `ast.go` needs a node type; `check.go` and `eval.go` each need a
case. Only the chosen branch may be evaluated. Extend `FuzzParse`'s seeds
with an example and run the fuzzer.

## Further reading

- [Strings, bytes, runes and characters in Go](https://go.dev/blog/strings)
- [unicode/utf8 package documentation](https://pkg.go.dev/unicode/utf8)
- [Effective Go: Functions](https://go.dev/doc/effective_go#functions) and [Defer](https://go.dev/doc/effective_go#defer)
- [regexp package documentation](https://pkg.go.dev/regexp) and the [syntax it accepts](https://pkg.go.dev/regexp/syntax)
- [sync/atomic package documentation](https://pkg.go.dev/sync/atomic)
- [Tutorial: Getting started with fuzzing](https://go.dev/doc/tutorial/fuzz)
- The standard library's own recursive-descent parser for Go: [go/parser](https://pkg.go.dev/go/parser)

Next: [9. Concurrency](09-concurrency.md)
