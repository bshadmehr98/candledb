package rules

import "github.com/bshadmehr98/candledb/internal/domain"

// Method is an operation a rule can allow.
type Method string

const (
	MethodGet    Method = "get"    // read one document by path
	MethodList   Method = "list"   // read documents through a query
	MethodCreate Method = "create" // write a document that does not exist
	MethodUpdate Method = "update" // write a document that exists
	MethodDelete Method = "delete"
)

// methodGroups expands the names accepted after "allow". "read" and "write"
// are shorthands for groups of methods.
var methodGroups = map[string][]Method{
	"get":    {MethodGet},
	"list":   {MethodList},
	"create": {MethodCreate},
	"update": {MethodUpdate},
	"delete": {MethodDelete},
	"read":   {MethodGet, MethodList},
	"write":  {MethodCreate, MethodUpdate, MethodDelete},
}

// Ruleset is a parsed rules file, ready to evaluate. It is immutable after
// Parse returns, so one Ruleset can serve any number of goroutines.
type Ruleset struct {
	matches []*match
	funcs   map[string]*function
	// allowAll short-circuits evaluation; see AllowAll.
	allowAll bool
}

// match is one "match /path { ... }" block.
type match struct {
	pattern  []segment
	allows   []*allow
	children []*match
	funcs    map[string]*function
}

// segment is one part of a match path: a literal name, a {wildcard} that
// captures one path segment, or a {wildcard=**} that captures all the rest.
type segment struct {
	literal string
	capture string
	rest    bool
}

// allow is one "allow methods: if condition;" statement.
type allow struct {
	methods []Method
	cond    expr // nil means unconditional
}

// function is a user-defined helper: function name(params) { return expr; }
type function struct {
	name   string
	params []string
	body   expr
	pos    Pos
}

// expr is a node of an expression's syntax tree. Like domain.Value it is a
// closed set of types that the evaluator walks with a type switch.
type expr interface{ position() Pos }

type (
	// literalExpr is a constant: 1, "text", true, null.
	literalExpr struct {
		pos   Pos
		value domain.Value
	}
	// identExpr names a variable: request, resource, a wildcard, a parameter.
	identExpr struct {
		pos  Pos
		name string
	}
	// memberExpr is x.name.
	memberExpr struct {
		pos  Pos
		x    expr
		name string
	}
	// indexExpr is x[i].
	indexExpr struct {
		pos Pos
		x   expr
		i   expr
	}
	// callExpr is f(args) or x.method(args).
	callExpr struct {
		pos  Pos
		fn   expr
		args []expr
	}
	// unaryExpr is !x or -x.
	unaryExpr struct {
		pos Pos
		op  string
		x   expr
	}
	// binaryExpr is "l op r" for every two-sided operator.
	binaryExpr struct {
		pos  Pos
		op   string
		l, r expr
	}
	// listExpr is [a, b, c].
	listExpr struct {
		pos   Pos
		elems []expr
	}
	// isExpr is "x is typename".
	isExpr struct {
		pos      Pos
		x        expr
		typeName string
	}
)

func (e *literalExpr) position() Pos { return e.pos }
func (e *identExpr) position() Pos   { return e.pos }
func (e *memberExpr) position() Pos  { return e.pos }
func (e *indexExpr) position() Pos   { return e.pos }
func (e *callExpr) position() Pos    { return e.pos }
func (e *unaryExpr) position() Pos   { return e.pos }
func (e *binaryExpr) position() Pos  { return e.pos }
func (e *listExpr) position() Pos    { return e.pos }
func (e *isExpr) position() Pos      { return e.pos }
