// Package storetest is the contract every storage adapter must honour,
// written as a reusable test suite.
//
// An interface such as app.Store only fixes method signatures. What the
// methods must do ("Apply is atomic", "Query applies keep before the limit")
// lives in comments, and comments are not checked. This package turns those
// promises into tests that take the adapter as a parameter, so the memory
// and SQLite adapters are held to exactly the same behaviour, and a future
// adapter can prove itself by calling Run.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/domain"
)

// Run executes the document store contract. open must return a new, empty
// store each time it is called.
func Run(t *testing.T, open func(t *testing.T) app.Store) {
	tests := map[string]func(*testing.T, app.Store){
		"get missing document":          testGetMissing,
		"apply then get":                testApplyThenGet,
		"overwrite and delete":          testOverwriteAndDelete,
		"batch and version":             testBatchAndVersion,
		"all value types survive":       testValueRoundTrip,
		"collections are isolated":      testCollectionIsolation,
		"query filters orders limits":   testQuery,
		"query keep runs before limit":  testQueryKeep,
		"query sees rewritten indexes":  testQueryAfterRewrite,
		"query pages through many rows": testQueryManyRows,
		"concurrent use":                testConcurrentUse,
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			test(t, store)
		})
	}
}

var ctx = context.Background()

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func doc(t *testing.T, path, jsonData string, version int64) *domain.Document {
	t.Helper()
	data, err := domain.ParseMapJSON([]byte(jsonData))
	if err != nil {
		t.Fatalf("bad test data: %v", err)
	}
	return &domain.Document{
		Path:       domain.DocPath(path),
		Data:       data,
		CreateTime: epoch,
		UpdateTime: epoch.Add(time.Duration(version) * time.Second),
		Version:    version,
	}
}

func put(t *testing.T, s app.Store, version int64, docs ...*domain.Document) {
	t.Helper()
	batch := make([]app.Mutation, len(docs))
	for i, d := range docs {
		batch[i] = app.Mutation{Path: d.Path, Doc: d}
	}
	if err := s.Apply(ctx, version, batch); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

func queryIDs(t *testing.T, s app.Store, q domain.Query, keep func(domain.Document) bool) string {
	t.Helper()
	if err := q.Validate(); err != nil {
		t.Fatalf("invalid test query: %v", err)
	}
	docs, err := s.Query(ctx, q, keep)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	ids := make([]string, len(docs))
	for i, d := range docs {
		ids[i] = d.Path.ID()
	}
	return strings.Join(ids, " ")
}

func field(s string) domain.FieldPath {
	fp, err := domain.ParseFieldPath(s)
	if err != nil {
		panic(err)
	}
	return fp
}

func value(s string) domain.Value {
	v, err := domain.ParseJSON([]byte(s))
	if err != nil {
		panic(err)
	}
	return v
}

func testGetMissing(t *testing.T, s app.Store) {
	_, err := s.Get(ctx, "users/nobody")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get of a missing document: error = %v, want ErrNotFound", err)
	}
	if v, err := s.Version(ctx); err != nil || v != 0 {
		t.Errorf("Version of an empty store = %d, %v; want 0", v, err)
	}
}

func testApplyThenGet(t *testing.T, s app.Store) {
	want := doc(t, "users/ada", `{"name": "Ada", "age": 36}`, 1)
	put(t, s, 1, want)

	got, err := s.Get(ctx, "users/ada")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != want.Path || got.Version != 1 || !domain.Equal(got.Data, want.Data) {
		t.Errorf("got %+v", got)
	}
	if !got.CreateTime.Equal(want.CreateTime) || !got.UpdateTime.Equal(want.UpdateTime) {
		t.Errorf("times = %v / %v, want %v / %v", got.CreateTime, got.UpdateTime, want.CreateTime, want.UpdateTime)
	}
}

func testOverwriteAndDelete(t *testing.T, s app.Store) {
	put(t, s, 1, doc(t, "users/ada", `{"v": 1, "old": true}`, 1))
	put(t, s, 2, doc(t, "users/ada", `{"v": 2}`, 2))

	got, err := s.Get(ctx, "users/ada")
	if err != nil {
		t.Fatal(err)
	}
	if !domain.Equal(got.Data, value(`{"v": 2}`)) || got.Version != 2 {
		t.Errorf("after overwrite: %+v", got)
	}

	if err := s.Apply(ctx, 3, []app.Mutation{{Path: "users/ada"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "users/ada"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("after delete: error = %v, want ErrNotFound", err)
	}
	// Deleting what is not there is not an error.
	if err := s.Apply(ctx, 4, []app.Mutation{{Path: "users/ghost"}}); err != nil {
		t.Errorf("deleting a missing document: %v", err)
	}
}

func testBatchAndVersion(t *testing.T, s app.Store) {
	put(t, s, 1, doc(t, "a/1", `{}`, 1), doc(t, "a/2", `{}`, 1), doc(t, "b/1", `{}`, 1))
	err := s.Apply(ctx, 2, []app.Mutation{
		{Path: "a/1"}, // delete
		{Path: "a/3", Doc: doc(t, "a/3", `{}`, 2)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := queryIDs(t, s, domain.Query{Collection: "a"}, nil); got != "2 3" {
		t.Errorf("collection a = %q, want \"2 3\"", got)
	}
	if v, _ := s.Version(ctx); v != 2 {
		t.Errorf("Version = %d, want 2", v)
	}
}

func testValueRoundTrip(t *testing.T, s app.Store) {
	const everything = `{
		"null": null, "yes": true, "no": false,
		"int": 9007199254740993, "negative": -5, "double": 1.5, "whole": 2.0,
		"at": {"$timestamp": "2026-02-03T04:05:06.123456Z"},
		"text": "héllo \u0000 world", "empty": "",
		"list": [1, "two", [3], {"four": 4}, null],
		"map": {"nested": {"deep": [true]}}, "emptyMap": {}, "emptyList": []
	}`
	want := doc(t, "things/all", everything, 1)
	put(t, s, 1, want)
	got, err := s.Get(ctx, "things/all")
	if err != nil {
		t.Fatal(err)
	}
	if !domain.Equal(got.Data, want.Data) {
		t.Errorf("data changed in storage:\n got  %#v\n want %#v", got.Data, want.Data)
	}
	// Equal treats 2 and 2.0 alike, so check the Go types separately.
	if _, ok := got.Data["int"].(domain.Int); !ok {
		t.Errorf("int came back as %T", got.Data["int"])
	}
	if _, ok := got.Data["whole"].(domain.Double); !ok {
		t.Errorf("double 2.0 came back as %T", got.Data["whole"])
	}
}

func testCollectionIsolation(t *testing.T, s app.Store) {
	put(t, s, 1,
		doc(t, "users/a", `{"n": 1}`, 1),
		doc(t, "users2/b", `{"n": 1}`, 1),               // shares a name prefix
		doc(t, "users/a/posts/c", `{"n": 1}`, 1),        // subcollection
		doc(t, "groups/users/members/d", `{"n": 1}`, 1), // "users" as a document ID
	)
	for collection, want := range map[domain.CollectionPath]string{
		"users":                "a",
		"users2":               "b",
		"users/a/posts":        "c",
		"groups/users/members": "d",
		"nothing":              "",
	} {
		if got := queryIDs(t, s, domain.Query{Collection: collection}, nil); got != want {
			t.Errorf("collection %s = %q, want %q", collection, got, want)
		}
		byField := domain.Query{Collection: collection, Filters: []domain.Filter{{Field: field("n"), Op: domain.OpEq, Value: value("1")}}}
		if got := queryIDs(t, s, byField, nil); got != want {
			t.Errorf("collection %s filtered = %q, want %q", collection, got, want)
		}
	}
}

func seedPeople(t *testing.T, s app.Store) {
	put(t, s, 1,
		doc(t, "people/ada", `{"name": "Ada", "age": 36, "langs": ["en", "fr"], "address": {"city": "London"}}`, 1),
		doc(t, "people/bob", `{"name": "Bob", "age": 25, "langs": ["en"], "address": {"city": "Paris"}}`, 1),
		doc(t, "people/cy", `{"name": "Cy", "age": 25, "langs": [], "address": {"city": "London"}}`, 1),
		doc(t, "people/dee", `{"name": "Dee", "age": 41.5, "langs": ["de", "en"]}`, 1),
		doc(t, "people/eve", `{"name": "Eve", "age": "unknown"}`, 1),
		doc(t, "people/frank", `{"name": "Frank"}`, 1),
	)
}

func testQuery(t *testing.T, s app.Store) {
	seedPeople(t, s)
	where := func(f string, op domain.Operator, v string) domain.Filter {
		return domain.Filter{Field: field(f), Op: op, Value: value(v)}
	}
	by := func(f string, desc bool) domain.Order { return domain.Order{Field: field(f), Desc: desc} }
	at := func(inclusive bool, values ...string) *domain.Cursor {
		c := &domain.Cursor{Inclusive: inclusive}
		for _, v := range values {
			c.Values = append(c.Values, value(v))
		}
		return c
	}

	tests := []struct {
		name string
		q    domain.Query
		want string
	}{
		{"everything, by ID", domain.Query{}, "ada bob cy dee eve frank"},
		{"limit by ID", domain.Query{Limit: 2}, "ada bob"},
		{"ID cursor", domain.Query{Start: at(false, `"bob"`), Limit: 2}, "cy dee"},
		{"equality", domain.Query{Filters: []domain.Filter{where("age", domain.OpEq, "25")}}, "bob cy"},
		{"equality on nested field", domain.Query{Filters: []domain.Filter{where("address.city", domain.OpEq, `"London"`)}}, "ada cy"},
		{"equality on a whole map", domain.Query{Filters: []domain.Filter{where("address", domain.OpEq, `{"city": "Paris"}`)}}, "bob"},
		{"range", domain.Query{Filters: []domain.Filter{where("age", domain.OpGt, "25"), where("age", domain.OpLt, "41")}}, "ada"},
		{"range ignores other kinds", domain.Query{Filters: []domain.Filter{where("age", domain.OpGe, "0")}}, "ada bob cy dee"},
		{"not equal", domain.Query{Filters: []domain.Filter{where("age", domain.OpNe, "25")}}, "ada dee eve"},
		{"in", domain.Query{Filters: []domain.Filter{where("name", domain.OpIn, `["Cy", "Ada", "Zed"]`)}}, "ada cy"},
		{"not in", domain.Query{Filters: []domain.Filter{where("name", domain.OpNotIn, `["Cy", "Ada"]`)}}, "bob dee eve frank"},
		{"array contains", domain.Query{Filters: []domain.Filter{where("langs", domain.OpArrayContains, `"en"`)}}, "ada bob dee"},
		{"array contains any", domain.Query{Filters: []domain.Filter{where("langs", domain.OpArrayContainsAny, `["en", "de"]`)}}, "ada bob dee"},
		{"two filters on different fields", domain.Query{Filters: []domain.Filter{where("age", domain.OpEq, "25"), where("address.city", domain.OpEq, `"London"`)}}, "cy"},
		{"order ascending", domain.Query{Orders: []domain.Order{by("age", false)}}, "bob cy ada dee eve"},
		{"order descending", domain.Query{Orders: []domain.Order{by("age", true)}}, "eve dee ada cy bob"},
		{"order with limit", domain.Query{Orders: []domain.Order{by("age", true)}, Limit: 2}, "eve dee"},
		{"order with filter on another field", domain.Query{Orders: []domain.Order{by("name", true)}, Filters: []domain.Filter{where("langs", domain.OpArrayContains, `"en"`)}, Limit: 2}, "dee bob"},
		{"order with range on the same field", domain.Query{Orders: []domain.Order{by("age", false)}, Filters: []domain.Filter{where("age", domain.OpGt, "25")}}, "ada dee"},
		{"two orderings", domain.Query{Orders: []domain.Order{by("age", false), by("name", true)}}, "cy bob ada dee eve"},
		{"start after a document", domain.Query{Orders: []domain.Order{by("age", false)}, Start: at(false, "25", `"bob"`)}, "cy ada dee eve"},
		{"start after, descending", domain.Query{Orders: []domain.Order{by("age", true)}, Start: at(false, "36", `"ada"`), Limit: 2}, "cy bob"},
		{"end before", domain.Query{Orders: []domain.Order{by("age", false)}, End: at(false, "36")}, "bob cy"},
		{"window", domain.Query{Orders: []domain.Order{by("age", false)}, Start: at(true, "25", `"cy"`), End: at(true, "41.5")}, "cy ada dee"},
		{"nothing matches", domain.Query{Filters: []domain.Filter{where("age", domain.OpEq, "99")}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.q.Collection = "people"
			if got := queryIDs(t, s, tt.q, nil); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func testQueryKeep(t *testing.T, s app.Store) {
	seedPeople(t, s)
	// keep may read from the store while the query is running; a security
	// rule that calls get() does exactly that.
	keep := func(d domain.Document) bool {
		if _, err := s.Get(ctx, d.Path); err != nil {
			t.Errorf("Get inside keep: %v", err)
		}
		return d.Path.ID() != "ada" && d.Path.ID() != "bob"
	}
	if got := queryIDs(t, s, domain.Query{Collection: "people", Limit: 2}, keep); got != "cy dee" {
		t.Errorf("by ID: got %q, want \"cy dee\"", got)
	}
	ordered := domain.Query{Collection: "people", Orders: []domain.Order{{Field: field("age")}}, Limit: 2}
	if got := queryIDs(t, s, ordered, keep); got != "cy dee" {
		t.Errorf("ordered: got %q, want \"cy dee\"", got)
	}
}

func testQueryAfterRewrite(t *testing.T, s app.Store) {
	byColor := func(color string) domain.Query {
		return domain.Query{Collection: "items", Filters: []domain.Filter{
			{Field: field("color"), Op: domain.OpEq, Value: domain.String(color)}}}
	}
	put(t, s, 1, doc(t, "items/x", `{"color": "red", "tags": ["a"]}`, 1))
	put(t, s, 2, doc(t, "items/x", `{"color": "blue", "tags": ["b"]}`, 2))

	if got := queryIDs(t, s, byColor("red"), nil); got != "" {
		t.Errorf("stale index entry: old value still finds %q", got)
	}
	if got := queryIDs(t, s, byColor("blue"), nil); got != "x" {
		t.Errorf("new value finds %q, want x", got)
	}
	hasTag := domain.Query{Collection: "items", Filters: []domain.Filter{
		{Field: field("tags"), Op: domain.OpArrayContains, Value: domain.String("a")}}}
	if got := queryIDs(t, s, hasTag, nil); got != "" {
		t.Errorf("stale array index entry finds %q", got)
	}

	if err := s.Apply(ctx, 3, []app.Mutation{{Path: "items/x"}}); err != nil {
		t.Fatal(err)
	}
	if got := queryIDs(t, s, byColor("blue"), nil); got != "" {
		t.Errorf("deleted document still found: %q", got)
	}
}

func testQueryManyRows(t *testing.T, s app.Store) {
	// More rows than any adapter is likely to read in one page.
	const n = 1300
	docs := make([]*domain.Document, n)
	for i := range docs {
		docs[i] = doc(t, fmt.Sprintf("rows/r%04d", i), fmt.Sprintf(`{"i": %d, "even": %t, "bucket": %d}`, i, i%2 == 0, i%7), 1)
	}
	put(t, s, 1, docs...)

	count := func(q domain.Query) int {
		t.Helper()
		q.Collection = "rows"
		got, err := s.Query(ctx, q, nil)
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}
	if got := count(domain.Query{}); got != n {
		t.Errorf("full scan returned %d rows, want %d", got, n)
	}
	if got := count(domain.Query{Orders: []domain.Order{{Field: field("i"), Desc: true}}}); got != n {
		t.Errorf("ordered scan returned %d rows, want %d", got, n)
	}
	even := domain.Filter{Field: field("even"), Op: domain.OpEq, Value: domain.Bool(true)}
	if got := count(domain.Query{Filters: []domain.Filter{even}}); got != n/2 {
		t.Errorf("equality scan returned %d rows, want %d", got, n/2)
	}

	// A selective filter on one field, ordered by another, with a small
	// limit: the scan must keep reading past rows that do not match.
	last := domain.Query{
		Collection: "rows",
		Filters:    []domain.Filter{{Field: field("i"), Op: domain.OpGe, Value: domain.Int(n - 3)}},
		Orders:     []domain.Order{{Field: field("bucket")}},
		Limit:      5,
	}
	got, err := s.Query(ctx, last, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("selective query returned %d rows, want 3", len(got))
	}
}

func testConcurrentUse(t *testing.T, s app.Store) {
	// Run with "go test -race": the race detector reports any unsynchronised
	// access the adapter makes to its own state.
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Go(func() {
			for i := range 50 {
				path := fmt.Sprintf("load/w%d-%d", w, i)
				d := doc(t, path, fmt.Sprintf(`{"w": %d, "i": %d}`, w, i), 1)
				if err := s.Apply(ctx, int64(w*100+i+1), []app.Mutation{{Path: d.Path, Doc: d}}); err != nil {
					t.Errorf("Apply: %v", err)
					return
				}
			}
		})
		wg.Go(func() {
			for range 50 {
				if _, err := s.Query(ctx, domain.Query{Collection: "load", Limit: 10}, nil); err != nil {
					t.Errorf("Query: %v", err)
					return
				}
				if _, err := s.Get(ctx, "load/w0-0"); err != nil && !errors.Is(err, domain.ErrNotFound) {
					t.Errorf("Get: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()

	all, err := s.Query(ctx, domain.Query{Collection: "load"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 200 {
		t.Errorf("%d documents after concurrent writes, want 200", len(all))
	}
}

// RunUsers executes the account store contract.
func RunUsers(t *testing.T, open func(t *testing.T) app.UserStore) {
	t.Run("users", func(t *testing.T) {
		s := open(t)
		ada := app.User{ID: "u1", Email: "ada@example.com", PasswordHash: "hash", CreatedAt: epoch}
		if err := s.CreateUser(ctx, ada); err != nil {
			t.Fatal(err)
		}
		dup := app.User{ID: "u2", Email: "ada@example.com", PasswordHash: "x", CreatedAt: epoch}
		if err := s.CreateUser(ctx, dup); !errors.Is(err, domain.ErrAlreadyExists) {
			t.Errorf("duplicate email: error = %v, want ErrAlreadyExists", err)
		}
		for name, lookup := range map[string]func() (app.User, error){
			"by email": func() (app.User, error) { return s.UserByEmail(ctx, "ada@example.com") },
			"by ID":    func() (app.User, error) { return s.UserByID(ctx, "u1") },
		} {
			got, err := lookup()
			if err != nil || got.ID != ada.ID || got.Email != ada.Email || got.PasswordHash != "hash" || !got.CreatedAt.Equal(epoch) {
				t.Errorf("%s: got %+v, %v", name, got, err)
			}
		}
		if _, err := s.UserByEmail(ctx, "nobody@example.com"); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("unknown email: error = %v, want ErrNotFound", err)
		}
		if _, err := s.UserByID(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("unknown ID: error = %v, want ErrNotFound", err)
		}
	})

	t.Run("refresh tokens are single use", func(t *testing.T) {
		s := open(t)
		expires := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
		if err := s.PutRefreshToken(ctx, app.RefreshToken{Hash: "h1", UserID: "u1", ExpiresAt: expires}); err != nil {
			t.Fatal(err)
		}

		// Many goroutines race for the same token; exactly one may win.
		var wg sync.WaitGroup
		var mu sync.Mutex
		winners := 0
		for range 8 {
			wg.Go(func() {
				tok, err := s.TakeRefreshToken(ctx, "h1")
				if err != nil {
					if !errors.Is(err, domain.ErrNotFound) {
						t.Errorf("TakeRefreshToken: %v", err)
					}
					return
				}
				if tok.UserID != "u1" || !tok.ExpiresAt.Equal(expires) {
					t.Errorf("token = %+v", tok)
				}
				mu.Lock()
				winners++
				mu.Unlock()
			})
		}
		wg.Wait()
		if winners != 1 {
			t.Errorf("%d callers obtained the same refresh token, want exactly 1", winners)
		}
	})
}
