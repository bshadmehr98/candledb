package rules

import (
	"errors"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/bshadmehr98/candledb/internal/domain"
)

// globalFunctions maps the built-in functions to their argument count.
var globalFunctions = map[string]int{
	"get":    1, // get("users/alice") -> the document, or null
	"exists": 1, // exists("users/alice") -> bool
}

// typeNames are the types accepted on the right of the "is" operator.
var typeNames = map[string]func(domain.Value) bool{
	"null":      func(v domain.Value) bool { return v.Kind() == domain.KindNull },
	"bool":      func(v domain.Value) bool { return v.Kind() == domain.KindBool },
	"int":       func(v domain.Value) bool { _, ok := v.(domain.Int); return ok },
	"float":     func(v domain.Value) bool { _, ok := v.(domain.Double); return ok },
	"number":    func(v domain.Value) bool { return v.Kind() == domain.KindNumber },
	"timestamp": func(v domain.Value) bool { return v.Kind() == domain.KindTimestamp },
	"string":    func(v domain.Value) bool { return v.Kind() == domain.KindString },
	"list":      func(v domain.Value) bool { return v.Kind() == domain.KindArray },
	"map":       func(v domain.Value) bool { return v.Kind() == domain.KindMap },
}

// method is the signature of a built-in method such as "text".size().
type method func(recv domain.Value, args []domain.Value) (domain.Value, error)

var (
	errReceiver  = errors.New("not available on this type")
	errArguments = errors.New("wrong arguments")
)

// methods is the table of built-in methods. Dispatching through a map of
// functions keeps the evaluator itself free of any knowledge of them: a new
// method is one new entry here.
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

func all(list domain.Array, pred func(domain.Value) bool) bool {
	for _, v := range list {
		if !pred(v) {
			return false
		}
	}
	return true
}

func anyOf(list domain.Array, pred func(domain.Value) bool) bool {
	for _, v := range list {
		if pred(v) {
			return true
		}
	}
	return false
}

func size(recv domain.Value, args []domain.Value) (domain.Value, error) {
	if len(args) != 0 {
		return nil, errArguments
	}
	switch v := recv.(type) {
	case domain.String:
		return domain.Int(utf8.RuneCountInString(string(v))), nil
	case domain.Array:
		return domain.Int(len(v)), nil
	case domain.Map:
		return domain.Int(len(v)), nil
	}
	return nil, errReceiver
}

func keys(recv domain.Value, args []domain.Value) (domain.Value, error) {
	m, ok := recv.(domain.Map)
	if !ok {
		return nil, errReceiver
	}
	if len(args) != 0 {
		return nil, errArguments
	}
	out := make(domain.Array, 0, len(m))
	for _, k := range m.Keys() {
		out = append(out, domain.String(k))
	}
	return out, nil
}

func values(recv domain.Value, args []domain.Value) (domain.Value, error) {
	m, ok := recv.(domain.Map)
	if !ok {
		return nil, errReceiver
	}
	if len(args) != 0 {
		return nil, errArguments
	}
	out := make(domain.Array, 0, len(m))
	for _, k := range m.Keys() {
		out = append(out, m[k])
	}
	return out, nil
}

// mapGet is map.get(key, fallback): the field, or fallback if it is absent.
func mapGet(recv domain.Value, args []domain.Value) (domain.Value, error) {
	m, ok := recv.(domain.Map)
	if !ok {
		return nil, errReceiver
	}
	if len(args) != 2 {
		return nil, errArguments
	}
	key, ok := args[0].(domain.String)
	if !ok {
		return nil, errArguments
	}
	if v, ok := m[string(key)]; ok {
		return v, nil
	}
	return args[1], nil
}

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

func stringMap(f func(string) string) method {
	return func(recv domain.Value, args []domain.Value) (domain.Value, error) {
		s, ok := recv.(domain.String)
		if !ok {
			return nil, errReceiver
		}
		if len(args) != 0 {
			return nil, errArguments
		}
		return domain.String(f(string(s))), nil
	}
}

// matches is "text".matches(pattern): whether the whole string matches the
// regular expression. Go's regexp package runs in time linear in the input,
// so a pattern cannot be crafted to stall the server.
func matches(recv domain.Value, args []domain.Value) (domain.Value, error) {
	s, ok := recv.(domain.String)
	if !ok {
		return nil, errReceiver
	}
	if len(args) != 1 {
		return nil, errArguments
	}
	pattern, ok := args[0].(domain.String)
	if !ok {
		return nil, errArguments
	}
	re, err := compileCached(string(pattern))
	if err != nil {
		return nil, err
	}
	return domain.Bool(re.MatchString(string(s))), nil
}

// regexCache remembers compiled patterns. Patterns almost always come from
// the rules file, so the set is tiny; the cap guards against a rule that
// builds patterns from document data.
var regexCache = struct {
	sync.Mutex
	m map[string]*regexp.Regexp
}{m: map[string]*regexp.Regexp{}}

const maxCachedPatterns = 256

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
