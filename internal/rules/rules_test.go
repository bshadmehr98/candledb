package rules_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bshadmehr98/candledb/internal/domain"
	"github.com/bshadmehr98/candledb/internal/rules"
)

func mustParse(t *testing.T, src string) *rules.Ruleset {
	t.Helper()
	rs, err := rules.Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, src)
	}
	return rs
}

func data(t *testing.T, s string) domain.Map {
	t.Helper()
	m, err := domain.ParseMapJSON([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestExpressions evaluates single conditions. Each case becomes the rule
// "allow get: if <expr>;" and is checked against one fixed request, so the
// table reads as a specification of the expression language.
func TestExpressions(t *testing.T) {
	req := rules.Request{
		Method: rules.MethodGet,
		Path:   "things/t1",
		Auth:   &rules.Auth{UID: "alice", Token: domain.Map{"email": domain.String("alice@example.com"), "admin": domain.Bool(true)}},
		Resource: &domain.Document{Path: "things/t1", Data: data(t, `{
			"owner": "alice", "count": 3, "price": 2.5, "tags": ["a", "b"],
			"title": "Hello", "nested": {"deep": {"n": 1}}, "nothing": null,
			"at": {"$timestamp": "2026-01-01T00:00:00Z"}
		}`)},
		Time: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}

	tests := []struct {
		expr string
		want bool
	}{
		// literals and comparison
		{`true`, true},
		{`false`, false},
		{`1 == 1.0`, true},
		{`1 != 2`, true},
		{`"a" < "b"`, true},
		{`2 >= 2`, true},
		{`"1" == 1`, false},
		{`1 < "a"`, false}, // comparing different kinds is an error, so: denied
		{`null == null`, true},

		// arithmetic and precedence
		{`1 + 2 * 3 == 7`, true},
		{`(1 + 2) * 3 == 9`, true},
		{`10 - 2 - 3 == 5`, true}, // left associative
		{`7 / 2 == 3`, true},      // integer division
		{`7 / 2.0 == 3.5`, true},
		{`7 % 4 == 3`, true},
		{`-3 + 5 == 2`, true},
		{`1 / 0 == 0`, false}, // error
		{`"a" + "b" == "ab"`, true},
		{`[1] + [2] == [1, 2]`, true},

		// logic
		{`true && false`, false},
		{`true || false`, true},
		{`!false`, true},
		{`!(1 == 2) && 2 > 1`, true},
		{`false && true || true`, true}, // && binds tighter than ||
		{`1 && true`, false},            // not a bool: error

		// errors are forgiven when the other side decides
		{`resource.data.missing == 1 || true`, true},
		{`true || resource.data.missing == 1`, true},
		{`resource.data.missing == 1 && false`, false},
		{`resource.data.missing == 1 || false`, false},
		{`resource.data.missing == 1 && true`, false},

		// request and resource
		{`request.auth != null`, true},
		{`request.auth.uid == "alice"`, true},
		{`request.auth.uid == resource.data.owner`, true},
		{`request.auth.token.email == "alice@example.com"`, true},
		{`request.auth.token.admin`, true},
		{`request.method == "get"`, true},
		{`request.path == "things/t1"`, true},
		{`request.time > resource.data.at`, true},
		{`request.resource == null`, true},
		{`resource.id == "t1" && resource.path == "things/t1"`, true},
		{`id == "t1"`, true}, // the wildcard of the match path
		{`resource.data.nested.deep.n == 1`, true},
		{`resource.data["owner"] == "alice"`, true},
		{`resource.data.tags[1] == "b"`, true},
		{`resource.data.tags[5] == "b"`, false},
		{`resource.data.nothing == null`, true},
		{`resource.data.get("match", 1) == 1 && request.auth.token.get("allow", 2) == 2`, true},
		{`resource.data.missing == null`, false}, // a missing field is an error, not null

		// in and is
		{`"a" in resource.data.tags`, true},
		{`"z" in resource.data.tags`, false},
		{`"owner" in resource.data`, true},
		{`"missing" in resource.data`, false},
		{`resource.data.count in [1, 2, 3]`, true},
		{`resource.data.count is int`, true},
		{`resource.data.count is number`, true},
		{`resource.data.price is float`, true},
		{`resource.data.price is int`, false},
		{`resource.data.title is string`, true},
		{`resource.data.tags is list`, true},
		{`resource.data.nested is map`, true},
		{`resource.data.at is timestamp`, true},
		{`resource.data.nothing is null`, true},
		{`resource.data.count is bool`, false},

		// methods
		{`resource.data.title.size() == 5`, true},
		{`"héllo".size() == 5`, true}, // characters, not bytes
		{`resource.data.tags.size() == 2`, true},
		{`resource.data.size() == 8`, true},
		{`resource.data.title.lower() == "hello"`, true},
		{`resource.data.title.upper() == "HELLO"`, true},
		{`"  x ".trim() == "x"`, true},
		{`resource.data.title.matches("H.*o")`, true},
		{`resource.data.title.matches("H")`, false}, // must match the whole string
		{`request.auth.token.email.matches(".*@example\.com")`, true},
		{`resource.data.title.matches("(")`, false}, // invalid pattern: error
		{`resource.data.tags.hasAll(["a", "b"])`, true},
		{`resource.data.tags.hasAll(["a", "z"])`, false},
		{`resource.data.tags.hasAny(["z", "b"])`, true},
		{`resource.data.tags.hasOnly(["a", "b", "c"])`, true},
		{`resource.data.tags.hasOnly(["a"])`, false},
		{`resource.data.nested.keys() == ["deep"]`, true},
		{`resource.data.nested.deep.values() == [1]`, true},
		{`resource.data.get("count", 0) == 3`, true},
		{`resource.data.get("missing", 7) == 7`, true},
		{`resource.data.count.size() == 1`, false}, // no size on a number
	}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			rs := mustParse(t, "match /things/{id} { allow get: if "+tt.expr+"; }")
			if got := rs.Allow(context.Background(), req, nil); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMatchPaths(t *testing.T) {
	rs := mustParse(t, `
		match /public/{doc} { allow read; }
		match /users/{uid} {
			allow get: if uid == "u1";
			match /posts/{post} {
				allow get: if uid == "u1" && post == "p1";
			}
			match /files/{file=**} {
				allow get: if file == "a/b/c";
			}
		}
		match /logs/{all=**} { allow create; }
	`)
	tests := []struct {
		method rules.Method
		path   domain.DocPath
		want   bool
	}{
		{rules.MethodGet, "public/x", true},
		{rules.MethodList, "public/x", true}, // read covers get and list
		{rules.MethodCreate, "public/x", false},
		{rules.MethodGet, "public/x/sub/y", false}, // a plain wildcard is one segment
		{rules.MethodGet, "users/u1", true},
		{rules.MethodGet, "users/u2", false},
		{rules.MethodGet, "users/u1/posts/p1", true},
		{rules.MethodGet, "users/u1/posts/p2", false},
		{rules.MethodGet, "users/u1/files/a/b/c", true},
		{rules.MethodGet, "users/u1/files/a", false},
		{rules.MethodCreate, "logs/a", true},
		{rules.MethodCreate, "logs/a/b/c", true},
		{rules.MethodDelete, "logs/a", false},
		{rules.MethodGet, "elsewhere/x", false}, // no match block: denied
	}
	for _, tt := range tests {
		if got := rs.Allow(context.Background(), rules.Request{Method: tt.method, Path: tt.path}, nil); got != tt.want {
			t.Errorf("%s %s = %v, want %v", tt.method, tt.path, got, tt.want)
		}
	}
}

func TestWriteMethodsAndRequestResource(t *testing.T) {
	rs := mustParse(t, `
		match /notes/{id} {
			allow create: if request.resource.data.owner == request.auth.uid;
			allow update: if resource.data.owner == request.auth.uid
			              && request.resource.data.owner == resource.data.owner;
			allow delete: if resource.data.owner == request.auth.uid;
		}`)
	mine := &domain.Document{Path: "notes/n", Data: data(t, `{"owner": "me"}`)}
	theirs := &domain.Document{Path: "notes/n", Data: data(t, `{"owner": "them"}`)}
	me := &rules.Auth{UID: "me"}

	tests := []struct {
		name string
		req  rules.Request
		want bool
	}{
		{"create own", rules.Request{Method: rules.MethodCreate, Auth: me, NewResource: mine}, true},
		{"create for someone else", rules.Request{Method: rules.MethodCreate, Auth: me, NewResource: theirs}, false},
		{"create anonymously", rules.Request{Method: rules.MethodCreate, NewResource: mine}, false},
		{"update own", rules.Request{Method: rules.MethodUpdate, Auth: me, Resource: mine, NewResource: mine}, true},
		{"give away ownership", rules.Request{Method: rules.MethodUpdate, Auth: me, Resource: mine, NewResource: theirs}, false},
		{"update theirs", rules.Request{Method: rules.MethodUpdate, Auth: me, Resource: theirs, NewResource: theirs}, false},
		{"delete own", rules.Request{Method: rules.MethodDelete, Auth: me, Resource: mine}, true},
		{"delete theirs", rules.Request{Method: rules.MethodDelete, Auth: me, Resource: theirs}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.req.Path = "notes/n"
			if got := rs.Allow(context.Background(), tt.req, nil); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFunctionsAndLookups(t *testing.T) {
	rs := mustParse(t, `
		function signedIn() { return request.auth != null; }
		function role(uid) { return get("roles/" + uid).data.role; }

		match /orgs/{org} {
			function isMember() { return exists("orgs/" + org + "/members/" + request.auth.uid); }
			allow get: if signedIn() && isMember();
			allow delete: if signedIn() && role(request.auth.uid) == "admin";
			allow update: if get("roles/nobody") == null;
		}`)

	db := map[domain.DocPath]*domain.Document{
		"roles/ann":             {Path: "roles/ann", Data: data(t, `{"role": "admin"}`)},
		"roles/ben":             {Path: "roles/ben", Data: data(t, `{"role": "user"}`)},
		"orgs/acme/members/ben": {Path: "orgs/acme/members/ben", Data: domain.Map{}},
	}
	var lookups int
	get := func(_ context.Context, p domain.DocPath) (*domain.Document, error) {
		lookups++
		return db[p], nil
	}
	allow := func(method rules.Method, uid string) bool {
		req := rules.Request{Method: method, Path: "orgs/acme"}
		if uid != "" {
			req.Auth = &rules.Auth{UID: uid}
		}
		return rs.Allow(context.Background(), req, get)
	}

	if !allow(rules.MethodGet, "ben") {
		t.Error("member denied")
	}
	if allow(rules.MethodGet, "ann") {
		t.Error("non-member allowed")
	}
	lookups = 0
	if allow(rules.MethodGet, "") {
		t.Error("anonymous allowed")
	}
	if lookups != 0 {
		t.Errorf("made %d lookups after signedIn() already failed; && must short-circuit", lookups)
	}
	if !allow(rules.MethodDelete, "ann") || allow(rules.MethodDelete, "ben") {
		t.Error("role check is wrong")
	}
	if allow(rules.MethodDelete, "zed") {
		t.Error("user without a role document allowed") // get() gives null; .data on null is an error
	}
	if !allow(rules.MethodUpdate, "ben") {
		t.Error("get of a missing document should be null")
	}

	failing := func(context.Context, domain.DocPath) (*domain.Document, error) {
		return nil, errors.New("disk on fire")
	}
	if rs.Allow(context.Background(), rules.Request{Method: rules.MethodUpdate, Path: "orgs/acme"}, failing) {
		t.Error("a failing lookup must deny")
	}
}

func TestEvaluationLimits(t *testing.T) {
	// Eleven lookups is one more than a single request may make.
	many := strings.Repeat(`exists("a/b") && `, 11) + "true"
	rs := mustParse(t, "match /x/{id} { allow get: if "+many+"; }")
	found := func(context.Context, domain.DocPath) (*domain.Document, error) {
		return &domain.Document{Path: "a/b"}, nil
	}
	if rs.Allow(context.Background(), rules.Request{Method: rules.MethodGet, Path: "x/1"}, found) {
		t.Error("lookup limit not enforced")
	}

	// Unbounded recursion must end in a denial, not a crash.
	loop := mustParse(t, `
		function forever(n) { return forever(n + 1); }
		match /x/{id} { allow get: if forever(0); }`)
	if loop.Allow(context.Background(), rules.Request{Method: rules.MethodGet, Path: "x/1"}, nil) {
		t.Error("infinite recursion allowed the request")
	}
}

func TestAllowAllAndDenyAll(t *testing.T) {
	req := rules.Request{Method: rules.MethodDelete, Path: "any/thing"}
	if !rules.AllowAll().Allow(context.Background(), req, nil) {
		t.Error("AllowAll denied")
	}
	if rules.DenyAll().Allow(context.Background(), req, nil) {
		t.Error("DenyAll allowed")
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name, src, wantMsg string
	}{
		{"unclosed block", `match /a/{b} { allow read;`, "expected allow"},
		{"missing semicolon", `match /a/{b} { allow read }`, `expected ";"`},
		{"unknown method", `match /a/{b} { allow fly; }`, "unknown method"},
		{"bad path", `match a/b { }`, "expected a path"},
		{"wildcard in the middle", `match /a/{b=**}/c { }`, "must be the last"},
		{"match inside recursive", `match /a/{b=**} { match /c/{d} { } }`, "cannot contain"},
		{"unknown variable", `match /a/{b} { allow read: if c == 1; }`, "unknown name c"},
		{"unknown function", `match /a/{b} { allow read: if nope(); }`, "unknown function nope"},
		{"unknown method call", `match /a/{b} { allow read: if b.explode(); }`, "unknown method explode"},
		{"wrong argument count", `function f(x) { return x; } match /a/{b} { allow read: if f(); }`, "wrong number of arguments"},
		{"unknown type", `match /a/{b} { allow read: if b is blob; }`, "unknown type"},
		{"unclosed string", `match /a/{b} { allow read: if b == "x; }`, "string is not closed"},
		{"stray character", `match /a/{b} { allow read: if b == #; }`, "unexpected character"},
		{"dangling operator", `match /a/{b} { allow read: if b == ; }`, "expected an expression"},
		{"duplicate function", `function f() { return 1; } function f() { return 2; }`, "defined twice"},
		{"redefined builtin", `function get(x) { return x; }`, "built-in"},
		{"function out of scope", `match /a/{b} { function f() { return true; } } match /c/{d} { allow read: if f(); }`, "unknown function f"},
		{"wildcard out of scope", `match /a/{b} { allow read; } match /c/{d} { allow read: if b == d; }`, "unknown name b"},
		{"top-level junk", `allow read;`, "expected match or function"},
		{"nested too deep", `match /a/{b} { allow read: if ` + strings.Repeat("(", 200) + `1` + strings.Repeat(")", 200) + `; }`, "nested too deeply"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := rules.Parse(tt.src)
			if err == nil {
				t.Fatal("Parse succeeded")
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error %q does not mention %q", err, tt.wantMsg)
			}
			// errors.As finds an error of a specific type anywhere in the
			// chain and gives access to its fields.
			var syntaxErr *rules.SyntaxError
			if !errors.As(err, &syntaxErr) {
				t.Errorf("error is %T, want *rules.SyntaxError", err)
			}
		})
	}
}

func TestSyntaxErrorPosition(t *testing.T) {
	_, err := rules.Parse("match /a/{b} {\n  allow read: if oops;\n}")
	var syntaxErr *rules.SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Fatalf("got %v", err)
	}
	if syntaxErr.Pos.Line != 2 || syntaxErr.Pos.Col != 18 {
		t.Errorf("position = %v, want 2:18", syntaxErr.Pos)
	}
}

// Words that are keywords at the start of a statement are ordinary field
// names after a dot.
func TestKeywordsAsFieldNames(t *testing.T) {
	rs := mustParse(t, `
		match /a/{b} {
			allow get: if resource.data.match == 1 && resource.data.allow == 2
			           && resource.data.function == 3 && resource.data.if == 4;
		}`)
	doc := &domain.Document{Path: "a/1", Data: data(t, `{"match": 1, "allow": 2, "function": 3, "if": 4}`)}
	if !rs.Allow(context.Background(), rules.Request{Method: rules.MethodGet, Path: "a/1", Resource: doc}, nil) {
		t.Error("denied")
	}
}

func TestCommentsAndStrings(t *testing.T) {
	rs := mustParse(t, `
		// a comment
		match /a/{b} { // trailing comment
			allow get: if 'single' == "single" && "tab\there" != "tab here" && "q\"q".size() == 3;
		}`)
	if !rs.Allow(context.Background(), rules.Request{Method: rules.MethodGet, Path: "a/1"}, nil) {
		t.Error("denied")
	}
}

// FuzzParse feeds the parser arbitrary input. Run it with
//
//	go test ./internal/rules -fuzz FuzzParse
//
// and Go generates inputs, guided by code coverage, looking for one that
// makes the function panic or hang. As a plain test it runs the seeds below.
func FuzzParse(f *testing.F) {
	f.Add(`match /users/{uid} { allow read: if request.auth.uid == uid; }`)
	f.Add(`function f(a, b) { return a + b * 2 in [1, 2] || !a.size() > 3; } match /a/{b=**} { allow write: if f(1, 2); }`)
	f.Add(`match /a/{b} { allow get, list: if resource.data.x[0].y is string && "x".matches('a\\.b'); }`)
	f.Add(`match`)
	f.Add(`match /{`)
	f.Add("match /a/{b} { allow read: if \"")
	f.Fuzz(func(t *testing.T, src string) {
		rs, err := rules.Parse(src)
		if err != nil {
			return
		}
		// Whatever parses must also evaluate without panicking.
		for _, method := range []rules.Method{rules.MethodGet, rules.MethodCreate} {
			rs.Allow(context.Background(), rules.Request{Method: method, Path: "a/b", Auth: &rules.Auth{UID: "u"}}, nil)
		}
	})
}
