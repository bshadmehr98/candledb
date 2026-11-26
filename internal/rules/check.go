package rules

// check walks a freshly parsed ruleset and verifies that every variable and
// function an expression refers to exists in its scope.
func check(rs *Ruleset) error {
	c := &checker{}
	base := scopeNames{"request": true, "resource": true}
	funcs := []map[string]*function{rs.funcs}
	for _, fn := range rs.funcs {
		if err := c.function(fn, base, funcs); err != nil {
			return err
		}
	}
	for _, m := range rs.matches {
		if err := c.match(m, base, funcs); err != nil {
			return err
		}
	}
	return nil
}

type scopeNames map[string]bool

func (s scopeNames) with(names ...string) scopeNames {
	out := make(scopeNames, len(s)+len(names))
	for k := range s {
		out[k] = true
	}
	for _, n := range names {
		out[n] = true
	}
	return out
}

type checker struct{}

func (c *checker) match(m *match, vars scopeNames, funcs []map[string]*function) error {
	for _, seg := range m.pattern {
		if seg.capture != "" {
			vars = vars.with(seg.capture)
		}
	}
	// Build a new slice so sibling matches do not see each other's functions.
	funcs = append(funcs[:len(funcs):len(funcs)], m.funcs)
	for _, fn := range m.funcs {
		if err := c.function(fn, vars, funcs); err != nil {
			return err
		}
	}
	for _, a := range m.allows {
		if a.cond != nil {
			if err := c.expr(a.cond, vars, funcs); err != nil {
				return err
			}
		}
	}
	for _, child := range m.children {
		if err := c.match(child, vars, funcs); err != nil {
			return err
		}
	}
	return nil
}

func (c *checker) function(fn *function, vars scopeNames, funcs []map[string]*function) error {
	return c.expr(fn.body, vars.with(fn.params...), funcs)
}

func (c *checker) expr(e expr, vars scopeNames, funcs []map[string]*function) error {
	switch e := e.(type) {
	case *literalExpr:
		return nil
	case *identExpr:
		if !vars[e.name] {
			return &SyntaxError{e.pos, "unknown name " + e.name}
		}
	case *memberExpr:
		return c.expr(e.x, vars, funcs)
	case *indexExpr:
		if err := c.expr(e.x, vars, funcs); err != nil {
			return err
		}
		return c.expr(e.i, vars, funcs)
	case *unaryExpr:
		return c.expr(e.x, vars, funcs)
	case *isExpr:
		return c.expr(e.x, vars, funcs)
	case *binaryExpr:
		if err := c.expr(e.l, vars, funcs); err != nil {
			return err
		}
		return c.expr(e.r, vars, funcs)
	case *listExpr:
		for _, el := range e.elems {
			if err := c.expr(el, vars, funcs); err != nil {
				return err
			}
		}
	case *callExpr:
		for _, arg := range e.args {
			if err := c.expr(arg, vars, funcs); err != nil {
				return err
			}
		}
		switch fn := e.fn.(type) {
		case *identExpr:
			if arity, ok := globalFunctions[fn.name]; ok {
				if len(e.args) != arity {
					return &SyntaxError{e.pos, fn.name + " takes exactly one argument"}
				}
				return nil
			}
			def := lookupFunction(funcs, fn.name)
			if def == nil {
				return &SyntaxError{fn.pos, "unknown function " + fn.name}
			}
			if len(def.params) != len(e.args) {
				return &SyntaxError{e.pos, "wrong number of arguments in call to " + fn.name}
			}
		case *memberExpr:
			if _, ok := methods[fn.name]; !ok {
				return &SyntaxError{fn.pos, "unknown method " + fn.name}
			}
			return c.expr(fn.x, vars, funcs)
		default:
			return &SyntaxError{e.pos, "this expression cannot be called"}
		}
	}
	return nil
}

// lookupFunction searches from the innermost scope outwards.
func lookupFunction(funcs []map[string]*function, name string) *function {
	for i := len(funcs) - 1; i >= 0; i-- {
		if fn, ok := funcs[i][name]; ok {
			return fn
		}
	}
	return nil
}
