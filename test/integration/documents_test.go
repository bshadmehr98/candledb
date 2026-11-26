package integration

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDocumentLifecycle(t *testing.T) {
	s := start(t, options{})

	s.must(404, "GET", "/v1/documents/users/ada", "", nil)

	put := s.must(200, "PUT", "/v1/documents/users/ada", "", `{"name": "Ada", "address": {"city": "London", "zip": "N1"}}`)
	if put.Body["version"] != 1.0 {
		t.Errorf("first commit version = %v", put.Body["version"])
	}

	got := s.must(200, "GET", "/v1/documents/users/ada", "", nil)
	if got.Body["path"] != "users/ada" || got.Body["id"] != "ada" || got.Body["version"] != 1.0 {
		t.Errorf("document = %v", got.Body)
	}
	if got.Body["createTime"] != put.Body["commitTime"] {
		t.Errorf("createTime %v != commitTime %v", got.Body["createTime"], put.Body["commitTime"])
	}

	// PATCH changes only what it names; dots reach into nested maps.
	s.must(200, "PATCH", "/v1/documents/users/ada", "", `{"address.city": "Paris", "age": 36}`)
	data := s.must(200, "GET", "/v1/documents/users/ada", "", nil).Body["data"].(map[string]any)
	address := data["address"].(map[string]any)
	if data["name"] != "Ada" || data["age"] != 36.0 || address["city"] != "Paris" || address["zip"] != "N1" {
		t.Errorf("after PATCH: %v", data)
	}
	missing := s.must(404, "PATCH", "/v1/documents/users/nobody", "", `{"a": 1}`)
	if strings.Contains(missing.Raw, "write 0") {
		t.Errorf("a single-document error talks about batches: %s", missing.Raw)
	}

	// PUT with merge keeps the other fields, including nested siblings.
	s.must(200, "PUT", "/v1/documents/users/ada?merge=true", "", `{"address": {"zip": "75001"}, "tags": ["x"]}`)
	data = s.must(200, "GET", "/v1/documents/users/ada", "", nil).Body["data"].(map[string]any)
	address = data["address"].(map[string]any)
	if data["name"] != "Ada" || address["city"] != "Paris" || address["zip"] != "75001" {
		t.Errorf("after merge: %v", data)
	}

	// Plain PUT replaces everything.
	s.must(200, "PUT", "/v1/documents/users/ada", "", `{"only": true}`)
	data = s.must(200, "GET", "/v1/documents/users/ada", "", nil).Body["data"].(map[string]any)
	if len(data) != 1 || data["only"] != true {
		t.Errorf("after replace: %v", data)
	}

	s.must(200, "DELETE", "/v1/documents/users/ada", "", nil)
	s.must(404, "GET", "/v1/documents/users/ada", "", nil)
	s.must(200, "DELETE", "/v1/documents/users/ada", "", nil) // deleting again is fine
}

func TestAddGeneratesID(t *testing.T) {
	s := start(t, options{})
	first := s.must(200, "POST", "/v1/documents/users/ada/posts", "", `{"title": "one"}`)
	second := s.must(200, "POST", "/v1/documents/users/ada/posts", "", `{"title": "two"}`)

	id, _ := first.Body["id"].(string)
	if len(id) < 20 || id == second.Body["id"] {
		t.Errorf("generated IDs: %q and %q", id, second.Body["id"])
	}
	if first.Body["path"] != "users/ada/posts/"+id {
		t.Errorf("path = %v", first.Body["path"])
	}
	got := s.must(200, "GET", "/v1/documents/users/ada/posts/"+id, "", nil)
	if got.Body["data"].(map[string]any)["title"] != "one" {
		t.Errorf("stored = %v", got.Body)
	}
	if ids := s.queryIDs("", `{"collection": "users/ada/posts"}`); len(strings.Fields(ids)) != 2 {
		t.Errorf("subcollection holds %q", ids)
	}
}

func TestInvalidRequests(t *testing.T) {
	s := start(t, options{maxBody: 1024})
	tests := []struct {
		name, method, path, body string
		status                   int
		code                     string
	}{
		{"GET a collection path", "GET", "/v1/documents/users", "", 400, "invalid_argument"},
		{"PUT to a collection path", "PUT", "/v1/documents/users", `{}`, 400, "invalid_argument"},
		{"POST to a document path", "POST", "/v1/documents/users/ada", `{}`, 400, "invalid_argument"},
		{"reserved segment", "GET", "/v1/documents/users/__x__", "", 400, "invalid_argument"},
		{"malformed JSON", "PUT", "/v1/documents/a/b", `{"x": `, 400, "invalid_argument"},
		{"body is not an object", "PUT", "/v1/documents/a/b", `[1, 2]`, 400, "invalid_argument"},
		{"empty body", "PUT", "/v1/documents/a/b", ``, 400, "invalid_argument"},
		{"reserved field name", "PUT", "/v1/documents/a/b", `{"$x": 1}`, 400, "invalid_argument"},
		{"field name with a dot", "PUT", "/v1/documents/a/b", `{"x.y": 1}`, 400, "invalid_argument"},
		{"unknown transform", "PUT", "/v1/documents/a/b", `{"x": {"$explode": 1}}`, 400, "invalid_argument"},
		{"delete field outside update", "PUT", "/v1/documents/a/b", `{"x": {"$delete": true}}`, 400, "invalid_argument"},
		{"increment by a string", "PATCH", "/v1/documents/a/b", `{"x": {"$increment": "1"}}`, 400, "invalid_argument"},
		{"body too large", "PUT", "/v1/documents/a/b", `{"x": "` + strings.Repeat("y", 2000) + `"}`, 413, "request_too_large"},
		{"unknown commit field", "POST", "/v1/commit", `{"writes": [], "extra": 1}`, 400, "invalid_argument"},
		{"empty commit", "POST", "/v1/commit", `{"writes": []}`, 400, "invalid_argument"},
		{"unknown write op", "POST", "/v1/commit", `{"writes": [{"op": "upsert", "path": "a/b", "data": {}}]}`, 400, "invalid_argument"},
		{"query without collection", "POST", "/v1/query", `{}`, 400, "invalid_argument"},
		{"query with bad operator", "POST", "/v1/query", `{"collection": "a", "where": [{"field": "x", "op": "like", "value": 1}]}`, 400, "invalid_argument"},
		{"query with bad direction", "POST", "/v1/query", `{"collection": "a", "orderBy": [{"field": "x", "direction": "up"}]}`, 400, "invalid_argument"},
		{"query with misspelt field", "POST", "/v1/query", `{"collection": "a", "limt": 5}`, 400, "invalid_argument"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body any
			if tt.body != "" || tt.method != "GET" {
				body = tt.body
			}
			res := s.call(tt.method, tt.path, "", body)
			if res.Status != tt.status || res.errorCode() != tt.code {
				t.Errorf("got %d %q, want %d %q\n%s", res.Status, res.errorCode(), tt.status, tt.code, res.Raw)
			}
		})
	}

	// Routing errors come from the standard library's mux.
	if res := s.call("GET", "/v1/nowhere", "", nil); res.Status != 404 {
		t.Errorf("unknown route = %d", res.Status)
	}
	if res := s.call("DELETE", "/v1/query", "", nil); res.Status != 405 {
		t.Errorf("wrong method = %d", res.Status)
	}
}

func TestTransformsAndValueTypes(t *testing.T) {
	s := start(t, options{})
	before := time.Now().Add(-time.Second)

	s.must(200, "PUT", "/v1/documents/c/d", "", `{
		"count": {"$increment": 5},
		"tags": {"$arrayUnion": ["a", "b"]},
		"created": {"$serverTimestamp": true},
		"when": {"$timestamp": "2026-01-02T03:04:05.678901+01:00"},
		"big": 9007199254740993,
		"ratio": 2.0,
		"nested": {"stamp": {"$serverTimestamp": true}, "keep": 1}
	}`)
	s.must(200, "PATCH", "/v1/documents/c/d", "", `{
		"count": {"$increment": -2},
		"tags": {"$arrayUnion": ["b", "c"]},
		"nested.keep": {"$delete": true},
		"ratio": {"$increment": 0.5}
	}`)
	s.must(200, "PATCH", "/v1/documents/c/d", "", `{"tags": {"$arrayRemove": ["a"]}}`)

	res := s.must(200, "GET", "/v1/documents/c/d", "", nil)
	data := res.Body["data"].(map[string]any)
	if data["count"] != 3.0 || data["ratio"] != 2.5 {
		t.Errorf("count = %v, ratio = %v", data["count"], data["ratio"])
	}
	if fmt.Sprint(data["tags"]) != "[b c]" {
		t.Errorf("tags = %v", data["tags"])
	}
	if _, has := data["nested"].(map[string]any)["keep"]; has {
		t.Error("nested.keep was not deleted")
	}
	// Timestamps come back normalised to UTC with microseconds.
	if got := data["when"].(map[string]any)["$timestamp"]; got != "2026-01-02T02:04:05.678901Z" {
		t.Errorf("when = %v", got)
	}
	created, err := time.Parse(time.RFC3339Nano, data["created"].(map[string]any)["$timestamp"].(string))
	if err != nil || created.Before(before) || created.After(time.Now().Add(time.Second)) {
		t.Errorf("server timestamp = %v (%v)", created, err)
	}
	// The raw text shows what a float-based JSON decoder would hide: the
	// large integer is exact and the double kept its decimal point.
	if !strings.Contains(res.Raw, `"big":9007199254740993`) {
		t.Errorf("large integer lost precision: %s", res.Raw)
	}
	if !strings.Contains(res.Raw, `"ratio":2.5`) {
		t.Errorf("ratio: %s", res.Raw)
	}
}

func TestCommitBatch(t *testing.T) {
	s := start(t, options{})
	s.must(200, "POST", "/v1/commit", "", `{"writes": [
		{"op": "create", "path": "accounts/a", "data": {"balance": 100}},
		{"op": "create", "path": "accounts/b", "data": {"balance": 0}}
	]}`)

	// A batch is all or nothing: the second write fails, so the first is
	// not applied either.
	res := s.call("POST", "/v1/commit", "", `{"writes": [
		{"op": "update", "path": "accounts/a", "data": {"balance": {"$increment": -50}}},
		{"op": "update", "path": "accounts/missing", "data": {"balance": {"$increment": 50}}}
	]}`)
	if res.Status != 404 {
		t.Fatalf("batch with a failing write = %d\n%s", res.Status, res.Raw)
	}
	balance := s.must(200, "GET", "/v1/documents/accounts/a", "", nil).Body["data"].(map[string]any)["balance"]
	if balance != 100.0 {
		t.Errorf("balance = %v: half of a failed batch was applied", balance)
	}

	if res := s.call("POST", "/v1/commit", "", `{"writes": [{"op": "create", "path": "accounts/a", "data": {}}]}`); res.Status != 409 || res.errorCode() != "already_exists" {
		t.Errorf("create over an existing document = %d %s", res.Status, res.errorCode())
	}

	// Preconditions turn a blind write into a conditional one.
	version := s.must(200, "GET", "/v1/documents/accounts/a", "", nil).Body["version"]
	ok := fmt.Sprintf(`{"writes": [{"op": "update", "path": "accounts/a", "data": {"balance": 1}, "precondition": {"version": %v}}]}`, version)
	s.must(200, "POST", "/v1/commit", "", ok)
	if res := s.call("POST", "/v1/commit", "", ok); res.Status != 412 || res.errorCode() != "failed_precondition" {
		t.Errorf("stale version = %d %s, want 412 failed_precondition", res.Status, res.errorCode())
	}
	if res := s.call("POST", "/v1/commit", "", `{"writes": [{"op": "set", "path": "accounts/a", "data": {}, "precondition": {"exists": false}}]}`); res.Status != 412 {
		t.Errorf("exists:false on an existing document = %d", res.Status)
	}
	// A verify write checks a document without touching it.
	current := s.must(200, "GET", "/v1/documents/accounts/a", "", nil).Body["version"]
	verify := fmt.Sprintf(`{"writes": [{"op": "verify", "path": "accounts/a", "precondition": {"version": %v}}, {"op": "set", "path": "accounts/c", "data": {}}]}`, current)
	s.must(200, "POST", "/v1/commit", "", verify)
	if after := s.must(200, "GET", "/v1/documents/accounts/a", "", nil).Body["version"]; after != current {
		t.Errorf("verify changed the document's version from %v to %v", current, after)
	}
	s.must(200, "PATCH", "/v1/documents/accounts/a", "", `{"balance": 2}`)
	if res := s.call("POST", "/v1/commit", "", verify); res.Status != 412 {
		t.Errorf("verify of a changed document = %d, want 412", res.Status)
	}

	s.must(200, "POST", "/v1/commit", "", `{"writes": [{"op": "delete", "path": "accounts/b", "precondition": {"exists": true}}]}`)
	if res := s.call("POST", "/v1/commit", "", `{"writes": [{"op": "delete", "path": "accounts/b", "precondition": {"exists": true}}]}`); res.Status != 412 {
		t.Errorf("exists:true on a missing document = %d", res.Status)
	}
}

func TestQueryEndpoint(t *testing.T) {
	s := start(t, options{})
	var writes []string
	for i := range 25 {
		writes = append(writes, fmt.Sprintf(`{"op": "set", "path": "items/i%02d", "data": {"n": %d, "group": %d, "tags": ["t%d"]}}`, i, i, i%3, i%5))
	}
	s.must(200, "POST", "/v1/commit", "", `{"writes": [`+strings.Join(writes, ",")+`]}`)

	if got := s.queryIDs("", `{"collection": "items", "where": [{"field": "n", "op": ">=", "value": 22}]}`); got != "i22 i23 i24" {
		t.Errorf("range = %q", got)
	}
	if got := s.queryIDs("", `{"collection": "items", "where": [{"field": "group", "op": "==", "value": 0}, {"field": "tags", "op": "array-contains", "value": "t0"}]}`); got != "i00 i15" {
		t.Errorf("two filters = %q", got)
	}
	if got := s.queryIDs("", `{"collection": "items", "orderBy": [{"field": "n", "direction": "desc"}], "limit": 3}`); got != "i24 i23 i22" {
		t.Errorf("descending with limit = %q", got)
	}
	if got := s.queryIDs("", `{"collection": "items", "where": [{"field": "n", "op": "in", "value": [3, 1, 99]}]}`); got != "i01 i03" {
		t.Errorf("in = %q", got)
	}
	if got := s.queryIDs("", `{"collection": "empty"}`); got != "" {
		t.Errorf("empty collection = %q", got)
	}

	// Page through everything, ordered by group then by ID, using the last
	// document of each page as the cursor for the next: the standard way a
	// client paginates.
	var all []string
	cursor := ""
	for page := 0; ; page++ {
		if page > 10 {
			t.Fatal("pagination does not terminate")
		}
		q := `{"collection": "items", "orderBy": [{"field": "group"}], "limit": 7` + cursor + `}`
		res := s.must(200, "POST", "/v1/query", "", q)
		docs := res.Body["documents"].([]any)
		if len(docs) == 0 {
			break
		}
		for _, d := range docs {
			all = append(all, d.(map[string]any)["id"].(string))
		}
		last := docs[len(docs)-1].(map[string]any)
		cursor = fmt.Sprintf(`, "startAt": {"values": [%v, %q], "inclusive": false}`,
			last["data"].(map[string]any)["group"], last["id"])
	}
	if len(all) != 25 {
		t.Fatalf("pagination returned %d documents, want 25: %v", len(all), all)
	}
	seen := map[string]bool{}
	for _, id := range all {
		if seen[id] {
			t.Fatalf("pagination returned %s twice", id)
		}
		seen[id] = true
	}
	if all[0] != "i00" || all[8] != "i24" || all[9] != "i01" {
		t.Errorf("page order is wrong: %v", all)
	}
}

// TestConcurrentTransactions runs the read-modify-write loop a client SDK
// uses for transactions, from many goroutines at once: read the document,
// compute a new value, write it on condition that the version is unchanged,
// and start over if it was not. No increment may be lost.
func TestConcurrentTransactions(t *testing.T) {
	s := start(t, options{})
	s.must(200, "PUT", "/v1/documents/counters/c", "", `{"n": 0}`)
	const workers, each = 8, 10

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range each {
				for attempt := 0; ; attempt++ {
					if attempt > 500 {
						t.Error("transaction never succeeded")
						return
					}
					doc := s.call("GET", "/v1/documents/counters/c", "", nil)
					n := doc.Body["data"].(map[string]any)["n"].(float64)
					res := s.call("POST", "/v1/commit", "", fmt.Sprintf(
						`{"writes": [{"op": "update", "path": "counters/c", "data": {"n": %v}, "precondition": {"version": %v}}]}`,
						n+1, doc.Body["version"]))
					if res.Status == 200 {
						break
					}
					if res.Status != 412 {
						t.Errorf("unexpected status %d: %s", res.Status, res.Raw)
						return
					}
				}
			}
		})
	}
	wg.Wait()

	n := s.must(200, "GET", "/v1/documents/counters/c", "", nil).Body["data"].(map[string]any)["n"]
	if n != float64(workers*each) {
		t.Errorf("counter = %v, want %d", n, workers*each)
	}
}

func TestDataSurvivesRestart(t *testing.T) {
	first := start(t, options{})
	access, refresh, _ := first.signUp("ada@example.com")
	first.must(200, "PUT", "/v1/documents/notes/n1", access, `{"text": "remember me", "tags": ["a"]}`)
	first.must(200, "PUT", "/v1/documents/notes/n2", access, `{"text": "and me"}`)
	first.stop()

	second := start(t, options{dbPath: first.dbPath})
	got := second.must(200, "GET", "/v1/documents/notes/n1", "", nil)
	if got.Body["data"].(map[string]any)["text"] != "remember me" {
		t.Errorf("document after restart: %v", got.Body)
	}
	// Indexes are on disk too.
	if ids := second.queryIDs("", `{"collection": "notes", "where": [{"field": "tags", "op": "array-contains", "value": "a"}]}`); ids != "n1" {
		t.Errorf("index query after restart = %q", ids)
	}
	// Versions carry on from where they stopped.
	if v := second.must(200, "PUT", "/v1/documents/notes/n3", "", `{}`).Body["version"]; v != 3.0 {
		t.Errorf("version after restart = %v, want 3", v)
	}
	// Accounts and sessions survive: the old tokens still work.
	second.must(200, "GET", "/v1/auth/me", access, nil)
	second.must(200, "POST", "/v1/auth/refresh", "", map[string]string{"refreshToken": refresh})
	second.must(200, "POST", "/v1/auth/signin", "", map[string]string{"email": "ada@example.com", "password": "password123"})
}
