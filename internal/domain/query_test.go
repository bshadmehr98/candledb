package domain_test

import (
	"strings"
	"testing"

	"github.com/bshadmehr98/candledb/internal/domain"
)

// people is the fixture most query tests run against.
func people(t *testing.T) []domain.Document {
	rows := map[string]string{
		"ada":   `{"name": "Ada",   "age": 36, "langs": ["en", "fr"], "city": "London"}`,
		"bob":   `{"name": "Bob",   "age": 25, "langs": ["en"],       "city": "Paris"}`,
		"cy":    `{"name": "Cy",    "age": 25, "langs": [],           "city": "London"}`,
		"dee":   `{"name": "Dee",   "age": 41.5, "langs": ["de"],     "city": null}`,
		"eve":   `{"name": "Eve",   "age": "unknown"}`,
		"frank": `{"name": "Frank"}`,
	}
	var docs []domain.Document
	for id, data := range rows {
		docs = append(docs, domain.Document{Path: domain.DocPath("people/" + id), Data: mustMap(t, data)})
	}
	// A document of another collection must never match.
	docs = append(docs, domain.Document{Path: "pets/ada", Data: mustMap(t, `{"name": "Ada", "age": 36}`)})
	return docs
}

func ids(docs []domain.Document) string {
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i] = d.Path.ID()
	}
	return strings.Join(out, " ")
}

func val(t *testing.T, s string) domain.Value {
	t.Helper()
	v, err := domain.ParseJSON([]byte(s))
	if err != nil {
		t.Fatalf("bad test value %s: %v", s, err)
	}
	return v
}

func TestQueryFilters(t *testing.T) {
	tests := []struct {
		field string
		op    domain.Operator
		value string
		want  string
	}{
		{"age", domain.OpEq, `25`, "bob cy"},
		{"age", domain.OpEq, `25.0`, "bob cy"},
		{"age", domain.OpNe, `25`, "ada dee eve"}, // frank has no age: != needs the field
		{"age", domain.OpGt, `25`, "ada dee"},     // eve's string age is another kind
		{"age", domain.OpGe, `25`, "ada bob cy dee"},
		{"age", domain.OpLt, `36`, "bob cy"},
		{"age", domain.OpLe, `36`, "ada bob cy"},
		{"age", domain.OpGt, `"a"`, "eve"},
		{"city", domain.OpEq, `null`, "dee"},
		{"city", domain.OpIn, `["Paris", null]`, "bob dee"},
		{"city", domain.OpNotIn, `["Paris", null]`, "ada cy"},
		{"langs", domain.OpArrayContains, `"en"`, "ada bob"},
		{"langs", domain.OpArrayContainsAny, `["fr", "de"]`, "ada dee"},
		{"langs", domain.OpEq, `[]`, "cy"},
		{"name", domain.OpArrayContains, `"A"`, ""}, // not an array
	}
	for _, tt := range tests {
		t.Run(tt.field+" "+string(tt.op)+" "+tt.value, func(t *testing.T) {
			q := domain.Query{
				Collection: "people",
				Filters:    []domain.Filter{{Field: fp(tt.field), Op: tt.op, Value: val(t, tt.value)}},
			}
			if err := q.Validate(); err != nil {
				t.Fatal(err)
			}
			if got := ids(q.Apply(people(t), nil)); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestQueryOrderLimitAndCursors(t *testing.T) {
	asc := []domain.Order{{Field: fp("age")}}
	desc := []domain.Order{{Field: fp("age"), Desc: true}}
	cursor := func(inclusive bool, values ...string) *domain.Cursor {
		c := &domain.Cursor{Inclusive: inclusive}
		for _, v := range values {
			c.Values = append(c.Values, val(t, v))
		}
		return c
	}

	tests := []struct {
		name string
		q    domain.Query
		want string
	}{
		// Ordering by age drops frank (no age) and puts eve's string last,
		// since strings sort after numbers. Ties (bob, cy) are broken by ID.
		{"ascending", domain.Query{Orders: asc}, "bob cy ada dee eve"},
		{"descending reverses the tie-break too", domain.Query{Orders: desc}, "eve dee ada cy bob"},
		{"no ordering means by ID", domain.Query{}, "ada bob cy dee eve frank"},
		{"limit", domain.Query{Orders: asc, Limit: 2}, "bob cy"},
		{"two orderings", domain.Query{Orders: []domain.Order{{Field: fp("age")}, {Field: fp("name"), Desc: true}}}, "cy bob ada dee eve"},
		{"start at value", domain.Query{Orders: asc, Start: cursor(true, `25`)}, "bob cy ada dee eve"},
		{"start after value skips all ties", domain.Query{Orders: asc, Start: cursor(false, `25`)}, "ada dee eve"},
		{"start after a document", domain.Query{Orders: asc, Start: cursor(false, `25`, `"bob"`)}, "cy ada dee eve"},
		{"end before value", domain.Query{Orders: asc, End: cursor(false, `36`)}, "bob cy"},
		{"end at value", domain.Query{Orders: asc, End: cursor(true, `36`)}, "bob cy ada"},
		{"window", domain.Query{Orders: asc, Start: cursor(false, `25`, `"bob"`), End: cursor(true, `41.5`)}, "cy ada dee"},
		{"descending start after a document", domain.Query{Orders: desc, Start: cursor(false, `25`, `"cy"`)}, "bob"},
		{"cursor on ID only", domain.Query{Start: cursor(false, `"bob"`), End: cursor(true, `"dee"`)}, "cy dee"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.q.Collection = "people"
			if err := tt.q.Validate(); err != nil {
				t.Fatal(err)
			}
			if got := ids(tt.q.Apply(people(t), nil)); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestQueryApplyKeepRunsBeforeLimit(t *testing.T) {
	q := domain.Query{Collection: "people", Limit: 2}
	notAda := func(d domain.Document) bool { return d.Path.ID() != "ada" }
	if got := ids(q.Apply(people(t), notAda)); got != "bob cy" {
		t.Errorf("got %q: the limit must be filled after filtering", got)
	}
}

func TestQueryValidate(t *testing.T) {
	one := val(t, `1`)
	bad := map[string]domain.Query{
		"document path":         {Collection: "people/ada"},
		"negative limit":        {Collection: "people", Limit: -1},
		"unknown operator":      {Collection: "people", Filters: []domain.Filter{{Field: fp("a"), Op: "~", Value: one}}},
		"in without a list":     {Collection: "people", Filters: []domain.Filter{{Field: fp("a"), Op: domain.OpIn, Value: one}}},
		"in with an empty list": {Collection: "people", Filters: []domain.Filter{{Field: fp("a"), Op: domain.OpIn, Value: domain.Array{}}}},
		"filter without value":  {Collection: "people", Filters: []domain.Filter{{Field: fp("a"), Op: domain.OpEq}}},
		"same field twice":      {Collection: "people", Orders: []domain.Order{{Field: fp("a")}, {Field: fp("a")}}},
		"cursor too long":       {Collection: "people", Start: &domain.Cursor{Values: []domain.Value{one, one}}},
		"cursor empty":          {Collection: "people", Start: &domain.Cursor{}},
		"cursor ID not string":  {Collection: "people", Orders: []domain.Order{{Field: fp("a")}}, End: &domain.Cursor{Values: []domain.Value{one, one}}},
	}
	for name, q := range bad {
		t.Run(name, func(t *testing.T) {
			if err := q.Validate(); err == nil {
				t.Error("Validate accepted the query")
			}
		})
	}
}
