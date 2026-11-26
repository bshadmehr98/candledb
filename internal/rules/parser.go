package rules

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/bshadmehr98/candledb/internal/domain"
)

// maxNesting bounds how deeply expressions may nest, so a hostile or
// mistaken rules file cannot overflow the stack of the parser or evaluator.
const maxNesting = 64

// Parse compiles rules source into a Ruleset. Besides syntax it checks that
// every name an expression uses is defined, so a typo is reported when the
// rules are loaded instead of silently denying requests later.
func Parse(src string) (*Ruleset, error) {
	tokens, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{tokens: tokens}
	rs := &Ruleset{funcs: map[string]*function{}}
	for p.peek().kind != tokEOF {
		switch {
		case p.peekIs(tokIdent, "match"):
			m, err := p.match()
			if err != nil {
				return nil, err
			}
			rs.matches = append(rs.matches, m)
		case p.peekIs(tokIdent, "function"):
			if err := p.function(rs.funcs); err != nil {
				return nil, err
			}
		default:
			return nil, p.errorf(p.peek().pos, "expected match or function, found %s", p.peek())
		}
	}
	if err := check(rs); err != nil {
		return nil, err
	}
	return rs, nil
}

// parser is a recursive-descent parser: one method per grammar rule, each
// consuming the tokens of that rule and returning its syntax tree.
type parser struct {
	tokens []token
	i      int
	depth  int
}

func (p *parser) errorf(pos Pos, format string, args ...any) error {
	return &SyntaxError{Pos: pos, Msg: fmt.Sprintf(format, args...)}
}

func (p *parser) peek() token { return p.tokens[p.i] }

func (p *parser) peekIs(kind tokenKind, text string) bool {
	t := p.peek()
	return t.kind == kind && t.text == text
}

func (p *parser) advance() token {
	t := p.tokens[p.i]
	if t.kind != tokEOF {
		p.i++
	}
	return t
}

// accept consumes the next token if it is the given punctuation.
func (p *parser) accept(punct string) bool {
	if p.peekIs(tokPunct, punct) {
		p.i++
		return true
	}
	return false
}

func (p *parser) expect(punct string) error {
	if !p.accept(punct) {
		return p.errorf(p.peek().pos, "expected %q, found %s", punct, p.peek())
	}
	return nil
}

func (p *parser) expectIdent() (token, error) {
	if t := p.peek(); t.kind != tokIdent {
		return t, p.errorf(t.pos, "expected a name, found %s", t)
	}
	return p.advance(), nil
}

// match := "match" PATH "{" (allow | match | function)* "}"
func (p *parser) match() (*match, error) {
	p.advance() // match
	pathTok := p.advance()
	pattern, err := p.pattern(pathTok)
	if err != nil {
		return nil, err
	}
	m := &match{pattern: pattern, funcs: map[string]*function{}}
	if err := p.expect("{"); err != nil {
		return nil, err
	}
	for !p.accept("}") {
		switch {
		case p.peekIs(tokIdent, "allow"):
			a, err := p.allow()
			if err != nil {
				return nil, err
			}
			m.allows = append(m.allows, a)
		case p.peekIs(tokIdent, "match"):
			if pattern[len(pattern)-1].rest {
				return nil, p.errorf(p.peek().pos, "a match with a ** wildcard cannot contain another match")
			}
			child, err := p.match()
			if err != nil {
				return nil, err
			}
			m.children = append(m.children, child)
		case p.peekIs(tokIdent, "function"):
			if err := p.function(m.funcs); err != nil {
				return nil, err
			}
		default:
			return nil, p.errorf(p.peek().pos, "expected allow, match, function or }, found %s", p.peek())
		}
	}
	return m, nil
}

func (p *parser) pattern(tok token) ([]segment, error) {
	if tok.kind != tokPath {
		return nil, p.errorf(tok.pos, "expected a path after match")
	}
	parts := strings.Split(strings.TrimPrefix(tok.text, "/"), "/")
	segments := make([]segment, 0, len(parts))
	for i, part := range parts {
		switch {
		case part == "":
			return nil, p.errorf(tok.pos, "path %s has an empty segment", tok.text)
		case strings.HasPrefix(part, "{"):
			name := strings.TrimSuffix(strings.TrimPrefix(part, "{"), "}")
			name, rest := strings.CutSuffix(name, "=**")
			if !isIdentifier(name) {
				return nil, p.errorf(tok.pos, "%q is not a valid wildcard name", name)
			}
			if rest && i != len(parts)-1 {
				return nil, p.errorf(tok.pos, "a ** wildcard must be the last segment of the path")
			}
			segments = append(segments, segment{capture: name, rest: rest})
		default:
			segments = append(segments, segment{literal: part})
		}
	}
	return segments, nil
}

func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		letter := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !letter && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// allow := "allow" NAME ("," NAME)* (":" "if" expr)? ";"
func (p *parser) allow() (*allow, error) {
	p.advance() // allow
	a := &allow{}
	for {
		name, err := p.expectIdent()
		if err != nil {
			return nil, err
		}
		methods, ok := methodGroups[name.text]
		if !ok {
			return nil, p.errorf(name.pos, "unknown method %q (use read, write, get, list, create, update or delete)", name.text)
		}
		a.methods = append(a.methods, methods...)
		if !p.accept(",") {
			break
		}
	}
	if p.accept(":") {
		if !p.peekIs(tokIdent, "if") {
			return nil, p.errorf(p.peek().pos, "expected if, found %s", p.peek())
		}
		p.advance()
		cond, err := p.expr()
		if err != nil {
			return nil, err
		}
		a.cond = cond
	}
	return a, p.expect(";")
}

// function := "function" NAME "(" params ")" "{" "return" expr ";" "}"
func (p *parser) function(into map[string]*function) error {
	p.advance() // function
	name, err := p.expectIdent()
	if err != nil {
		return err
	}
	if _, dup := into[name.text]; dup {
		return p.errorf(name.pos, "function %s is defined twice", name.text)
	}
	if _, builtin := globalFunctions[name.text]; builtin {
		return p.errorf(name.pos, "%s is a built-in function and cannot be redefined", name.text)
	}
	fn := &function{name: name.text, pos: name.pos}
	if err := p.expect("("); err != nil {
		return err
	}
	for !p.accept(")") {
		if len(fn.params) > 0 {
			if err := p.expect(","); err != nil {
				return err
			}
		}
		param, err := p.expectIdent()
		if err != nil {
			return err
		}
		fn.params = append(fn.params, param.text)
	}
	if err := p.expect("{"); err != nil {
		return err
	}
	if !p.peekIs(tokIdent, "return") {
		return p.errorf(p.peek().pos, "expected return, found %s", p.peek())
	}
	p.advance()
	if fn.body, err = p.expr(); err != nil {
		return err
	}
	if err := p.expect(";"); err != nil {
		return err
	}
	into[fn.name] = fn
	return p.expect("}")
}

// precedence gives each binary operator its binding strength; a higher
// number binds tighter, so a || b && c parses as a || (b && c).
var precedence = map[string]int{
	"||": 1,
	"&&": 2,
	"==": 3, "!=": 3,
	"<": 4, "<=": 4, ">": 4, ">=": 4, "in": 4, "is": 4,
	"+": 5, "-": 5,
	"*": 6, "/": 6, "%": 6,
}

func (p *parser) expr() (expr, error) { return p.binary(1) }

// binary parses a chain of binary operators whose precedence is at least
// minPrec. This technique is called precedence climbing: one loop and one
// table replace a separate grammar rule per precedence level.
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
		if op.text == "is" {
			typeName, err := p.expectIdent()
			if err != nil {
				return nil, err
			}
			if _, known := typeNames[typeName.text]; !known {
				return nil, p.errorf(typeName.pos, "unknown type %q", typeName.text)
			}
			left = &isExpr{pos: op.pos, x: left, typeName: typeName.text}
			continue
		}
		// prec+1 makes operators of equal precedence group to the left:
		// a - b - c is (a - b) - c.
		right, err := p.binary(prec + 1)
		if err != nil {
			return nil, err
		}
		left = &binaryExpr{pos: op.pos, op: op.text, l: left, r: right}
	}
}

func (p *parser) unary() (expr, error) {
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > maxNesting {
		return nil, p.errorf(p.peek().pos, "expression is nested too deeply")
	}
	if t := p.peek(); t.kind == tokPunct && (t.text == "!" || t.text == "-") {
		p.advance()
		x, err := p.unary()
		if err != nil {
			return nil, err
		}
		return &unaryExpr{pos: t.pos, op: t.text, x: x}, nil
	}
	return p.postfix()
}

// postfix := primary ( "." NAME | "[" expr "]" | "(" args ")" )*
func (p *parser) postfix() (expr, error) {
	x, err := p.primary()
	if err != nil {
		return nil, err
	}
	for {
		pos := p.peek().pos
		switch {
		case p.accept("."):
			name, err := p.expectIdent()
			if err != nil {
				return nil, err
			}
			x = &memberExpr{pos: pos, x: x, name: name.text}
		case p.accept("["):
			index, err := p.expr()
			if err != nil {
				return nil, err
			}
			if err := p.expect("]"); err != nil {
				return nil, err
			}
			x = &indexExpr{pos: pos, x: x, i: index}
		case p.accept("("):
			args, err := p.exprList(")")
			if err != nil {
				return nil, err
			}
			x = &callExpr{pos: pos, fn: x, args: args}
		default:
			return x, nil
		}
	}
}

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

func (p *parser) primary() (expr, error) {
	t := p.advance()
	switch t.kind {
	case tokInt:
		n, err := strconv.ParseInt(t.text, 10, 64)
		if err != nil {
			return nil, p.errorf(t.pos, "integer %s is out of range", t.text)
		}
		return &literalExpr{t.pos, domain.Int(n)}, nil
	case tokFloat:
		f, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, p.errorf(t.pos, "number %s is out of range", t.text)
		}
		return &literalExpr{t.pos, domain.Double(f)}, nil
	case tokString:
		return &literalExpr{t.pos, domain.String(t.text)}, nil
	case tokIdent:
		switch t.text {
		case "true":
			return &literalExpr{t.pos, domain.Bool(true)}, nil
		case "false":
			return &literalExpr{t.pos, domain.Bool(false)}, nil
		case "null":
			return &literalExpr{t.pos, domain.Null{}}, nil
		}
		return &identExpr{t.pos, t.text}, nil
	case tokPunct:
		switch t.text {
		case "(":
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			return e, p.expect(")")
		case "[":
			elems, err := p.exprList("]")
			if err != nil {
				return nil, err
			}
			return &listExpr{t.pos, elems}, nil
		}
	}
	return nil, p.errorf(t.pos, "expected an expression, found %s", t)
}
