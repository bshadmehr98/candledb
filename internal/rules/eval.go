package rules

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/bshadmehr98/candledb/internal/domain"
)

// Limits that keep one evaluation cheap and bounded.
const (
	maxLookups   = 10 // get() and exists() calls per request
	maxCallDepth = 16 // nested user-function calls
)

// Request describes the operation being authorised.
type Request struct {
	Method Method
	Path   domain.DocPath
	// Auth is nil when the caller is not signed in.
	Auth *Auth
	// Resource is the stored document, or nil if there is none.
	Resource *domain.Document
	// NewResource is the document as it would be after a create or update.
	NewResource *domain.Document
	Time        time.Time
}

// Auth is the identity of a signed-in caller as rules see it.
type Auth struct {
	UID string
	// Token holds the claims of the caller's access token.
	Token domain.Map
}

// Getter loads a document for the get and exists functions. It returns
// (nil, nil) when the document does not exist.
type Getter func(ctx context.Context, path domain.DocPath) (*domain.Document, error)

// AllowAll returns a ruleset that permits everything. It is meant for local
// development and tests.
func AllowAll() *Ruleset { return &Ruleset{allowAll: true} }

// DenyAll returns a ruleset that permits nothing, the safe default when no
// rules file is configured.
func DenyAll() *Ruleset { return &Ruleset{} }

// Allow reports whether the rules permit the request.
//
// Access is denied unless some allow statement, in a match block whose path
// matches the document, lists the method and has a condition that evaluates
// to true. A condition that fails to evaluate (for example because it reads
// a field of a missing document) counts as false.
func (rs *Ruleset) Allow(ctx context.Context, req Request, get Getter) bool {
	if rs.allowAll {
		return true
	}
	ev := &evaluator{ctx: ctx, get: get}
	vars := map[string]domain.Value{
		"request":  requestValue(req),
		"resource": documentValue(req.Resource),
	}
	funcs := []map[string]*function{rs.funcs}
	return ev.walk(rs.matches, req.Path.Segments(), req.Method, vars, funcs)
}

func requestValue(req Request) domain.Value {
	var auth domain.Value = domain.Null{}
	if req.Auth != nil {
		token := req.Auth.Token
		if token == nil {
			token = domain.Map{}
		}
		auth = domain.Map{"uid": domain.String(req.Auth.UID), "token": token}
	}
	return domain.Map{
		"auth":     auth,
		"method":   domain.String(req.Method),
		"path":     domain.String(req.Path),
		"time":     domain.NewTimestamp(req.Time),
		"resource": documentValue(req.NewResource),
	}
}

// documentValue is how a document appears inside an expression: a map with
// the fields data, id and path, or null when there is no document.
func documentValue(doc *domain.Document) domain.Value {
	if doc == nil {
		return domain.Null{}
	}
	return domain.Map{
		"data": doc.Data,
		"id":   domain.String(doc.Path.ID()),
		"path": domain.String(doc.Path),
	}
}

// evaluator holds the state of one Allow call.
type evaluator struct {
	ctx     context.Context
	get     Getter
	lookups int
	depth   int
}

// scope is the set of variables visible to an expression, plus the chain of
// function tables to resolve calls in.
type scope struct {
	vars  map[string]domain.Value
	funcs []map[string]*function
	// base is the scope of the allow statement being evaluated. A function
	// body sees base plus its own parameters, never its caller's parameters.
	base *scope
}

// walk descends through nested match blocks, consuming path segments, and
// evaluates the allow statements of every block that matches the full path.
func (ev *evaluator) walk(matches []*match, segs []string, method Method, vars map[string]domain.Value, funcs []map[string]*function) bool {
	for _, m := range matches {
		rest, bound, ok := m.bind(segs)
		if !ok {
			continue
		}
		inner := vars
		if len(bound) > 0 {
			inner = make(map[string]domain.Value, len(vars)+len(bound))
			for k, v := range vars {
				inner[k] = v
			}
			for k, v := range bound {
				inner[k] = domain.String(v)
			}
		}
		innerFuncs := append(funcs[:len(funcs):len(funcs)], m.funcs)
		if len(rest) > 0 {
			if ev.walk(m.children, rest, method, inner, innerFuncs) {
				return true
			}
			continue
		}
		sc := &scope{vars: inner, funcs: innerFuncs}
		sc.base = sc
		for _, a := range m.allows {
			if a.covers(method) && ev.holds(a.cond, sc) {
				return true
			}
		}
	}
	return false
}

// bind matches the pattern against the start of segs. It returns the
// segments left over and the values captured by wildcards.
func (m *match) bind(segs []string) (rest []string, bound map[string]string, ok bool) {
	for i, seg := range m.pattern {
		if seg.rest {
			if i >= len(segs) {
				return nil, nil, false
			}
			if bound == nil {
				bound = map[string]string{}
			}
			bound[seg.capture] = joinPath(segs[i:])
			return nil, bound, true
		}
		if i >= len(segs) {
			return nil, nil, false
		}
		switch {
		case seg.capture != "":
			if bound == nil {
				bound = map[string]string{}
			}
			bound[seg.capture] = segs[i]
		case seg.literal != segs[i]:
			return nil, nil, false
		}
	}
	return segs[len(m.pattern):], bound, true
}

func joinPath(segs []string) string {
	out := segs[0]
	for _, s := range segs[1:] {
		out += "/" + s
	}
	return out
}

func (a *allow) covers(method Method) bool {
	for _, m := range a.methods {
		if m == method {
			return true
		}
	}
	return false
}

// holds evaluates a condition to a yes or no. Errors and non-boolean
// results are a no.
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

func evalErr(pos Pos, format string, args ...any) error {
	return fmt.Errorf("rules:%s: %s", pos, fmt.Sprintf(format, args...))
}

// eval computes the value of an expression. It is the heart of the
// interpreter: a type switch with one case per kind of syntax node, each
// evaluating its children by calling eval again.
func (ev *evaluator) eval(e expr, sc *scope) (domain.Value, error) {
	switch e := e.(type) {
	case *literalExpr:
		return e.value, nil

	case *identExpr:
		if v, ok := sc.vars[e.name]; ok {
			return v, nil
		}
		return nil, evalErr(e.pos, "unknown name %s", e.name)

	case *listExpr:
		out := make(domain.Array, len(e.elems))
		for i, el := range e.elems {
			v, err := ev.eval(el, sc)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil

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

	case *indexExpr:
		x, err := ev.eval(e.x, sc)
		if err != nil {
			return nil, err
		}
		i, err := ev.eval(e.i, sc)
		if err != nil {
			return nil, err
		}
		return index(e.pos, x, i)

	case *unaryExpr:
		x, err := ev.eval(e.x, sc)
		if err != nil {
			return nil, err
		}
		switch v := x.(type) {
		case domain.Bool:
			if e.op == "!" {
				return !v, nil
			}
		case domain.Int:
			if e.op == "-" {
				return -v, nil
			}
		case domain.Double:
			if e.op == "-" {
				return -v, nil
			}
		}
		return nil, evalErr(e.pos, "operator %s cannot be applied to a %s", e.op, x.Kind())

	case *isExpr:
		x, err := ev.eval(e.x, sc)
		if err != nil {
			return nil, err
		}
		return domain.Bool(typeNames[e.typeName](x)), nil

	case *binaryExpr:
		if e.op == "&&" || e.op == "||" {
			return ev.logical(e, sc)
		}
		l, err := ev.eval(e.l, sc)
		if err != nil {
			return nil, err
		}
		r, err := ev.eval(e.r, sc)
		if err != nil {
			return nil, err
		}
		return binary(e.pos, e.op, l, r)

	case *callExpr:
		return ev.call(e, sc)
	}
	return nil, errors.New("rules: unknown expression node")
}

// logical evaluates && and ||. Both short-circuit, and both forgive an
// error on one side when the other side alone decides the result:
// "broken || true" is true and "broken && false" is false. That lets a rule
// such as
//
//	resource == null || resource.data.owner == request.auth.uid
//
// be written in either order.
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

func (ev *evaluator) evalBool(e expr, sc *scope) (domain.Bool, error) {
	v, err := ev.eval(e, sc)
	if err != nil {
		return false, err
	}
	b, ok := v.(domain.Bool)
	if !ok {
		return false, evalErr(e.position(), "expected a bool, found a %s", v.Kind())
	}
	return b, nil
}

func index(pos Pos, x, i domain.Value) (domain.Value, error) {
	switch c := x.(type) {
	case domain.Array:
		n, ok := i.(domain.Int)
		if !ok || n < 0 || int(n) >= len(c) {
			return nil, evalErr(pos, "list index is out of range")
		}
		return c[n], nil
	case domain.Map:
		key, ok := i.(domain.String)
		if !ok {
			return nil, evalErr(pos, "map keys are strings")
		}
		v, ok := c[string(key)]
		if !ok {
			return nil, evalErr(pos, "field %s does not exist", key)
		}
		return v, nil
	}
	return nil, evalErr(pos, "a %s cannot be indexed", x.Kind())
}

func binary(pos Pos, op string, l, r domain.Value) (domain.Value, error) {
	switch op {
	case "==":
		return domain.Bool(domain.Equal(l, r)), nil
	case "!=":
		return domain.Bool(!domain.Equal(l, r)), nil
	case "<", "<=", ">", ">=":
		if l.Kind() != r.Kind() {
			return nil, evalErr(pos, "cannot compare a %s with a %s", l.Kind(), r.Kind())
		}
		c := domain.Compare(l, r)
		switch op {
		case "<":
			return domain.Bool(c < 0), nil
		case "<=":
			return domain.Bool(c <= 0), nil
		case ">":
			return domain.Bool(c > 0), nil
		}
		return domain.Bool(c >= 0), nil
	case "in":
		switch c := r.(type) {
		case domain.Array:
			return domain.Bool(c.Contains(l)), nil
		case domain.Map:
			key, ok := l.(domain.String)
			if !ok {
				return domain.Bool(false), nil
			}
			_, has := c[string(key)]
			return domain.Bool(has), nil
		}
		return nil, evalErr(pos, "the right side of in must be a list or a map")
	case "+":
		switch lv := l.(type) {
		case domain.String:
			if rv, ok := r.(domain.String); ok {
				return lv + rv, nil
			}
		case domain.Array:
			if rv, ok := r.(domain.Array); ok {
				return append(append(domain.Array{}, lv...), rv...), nil
			}
		}
	}
	return arithmetic(pos, op, l, r)
}

func arithmetic(pos Pos, op string, l, r domain.Value) (domain.Value, error) {
	if l.Kind() != domain.KindNumber || r.Kind() != domain.KindNumber {
		return nil, evalErr(pos, "operator %s cannot be applied to a %s and a %s", op, l.Kind(), r.Kind())
	}
	li, lInt := l.(domain.Int)
	ri, rInt := r.(domain.Int)
	if lInt && rInt {
		switch op {
		case "+":
			return li + ri, nil
		case "-":
			return li - ri, nil
		case "*":
			return li * ri, nil
		case "/", "%":
			if ri == 0 {
				return nil, evalErr(pos, "division by zero")
			}
			if op == "/" {
				return li / ri, nil
			}
			return li % ri, nil
		}
	}
	lf, _ := domain.NumberKey(l)
	rf, _ := domain.NumberKey(r)
	var out float64
	switch op {
	case "+":
		out = lf + rf
	case "-":
		out = lf - rf
	case "*":
		out = lf * rf
	case "/":
		out = lf / rf
	case "%":
		out = math.Mod(lf, rf)
	}
	if math.IsNaN(out) || math.IsInf(out, 0) {
		return nil, evalErr(pos, "arithmetic result is not a finite number")
	}
	return domain.Double(out), nil
}

func (ev *evaluator) call(e *callExpr, sc *scope) (domain.Value, error) {
	args := make([]domain.Value, len(e.args))
	for i, a := range e.args {
		v, err := ev.eval(a, sc)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	switch fn := e.fn.(type) {
	case *memberExpr:
		recv, err := ev.eval(fn.x, sc)
		if err != nil {
			return nil, err
		}
		v, err := methods[fn.name](recv, args)
		if err != nil {
			return nil, evalErr(e.pos, "%s: %v", fn.name, err)
		}
		return v, nil
	case *identExpr:
		if _, ok := globalFunctions[fn.name]; ok {
			return ev.lookup(e.pos, fn.name, args[0])
		}
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
	}
	return nil, evalErr(e.pos, "this expression cannot be called")
}

// lookup implements get(path) and exists(path).
func (ev *evaluator) lookup(pos Pos, name string, arg domain.Value) (domain.Value, error) {
	raw, ok := arg.(domain.String)
	if !ok {
		return nil, evalErr(pos, "%s needs a path string", name)
	}
	path, err := domain.ParseDocPath(string(raw))
	if err != nil {
		return nil, evalErr(pos, "%s: %v", name, err)
	}
	if ev.get == nil {
		return nil, evalErr(pos, "%s is not available here", name)
	}
	if ev.lookups++; ev.lookups > maxLookups {
		return nil, evalErr(pos, "more than %d document lookups in one request", maxLookups)
	}
	doc, err := ev.get(ev.ctx, path)
	if err != nil {
		return nil, evalErr(pos, "%s: %v", name, err)
	}
	if name == "exists" {
		return domain.Bool(doc != nil), nil
	}
	return documentValue(doc), nil
}
