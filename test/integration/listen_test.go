package integration

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestListenStream(t *testing.T) {
	s := start(t, options{})
	s.must(200, "PUT", "/v1/documents/tasks/a", "", `{"done": false, "rank": 1}`)
	st := s.listen("")

	if res := st.add(1, `"query": {"collection": "tasks", "where": [{"field": "done", "op": "==", "value": false}]}`); res.Status != 204 {
		t.Fatalf("add target = %d %s", res.Status, res.Raw)
	}
	snap := st.expect("snapshot t1 [+a]")
	if snap["version"] != 1.0 {
		t.Errorf("snapshot version = %v", snap["version"])
	}

	s.must(200, "PUT", "/v1/documents/tasks/b", "", `{"done": false, "rank": 2}`)
	added := st.expect("change t1 [+b]")
	doc := added["changes"].([]any)[0].(map[string]any)["doc"].(map[string]any)
	if doc["path"] != "tasks/b" || doc["data"].(map[string]any)["rank"] != 2.0 || added["version"] != 2.0 {
		t.Errorf("added event = %v", added)
	}

	s.must(200, "PATCH", "/v1/documents/tasks/a", "", `{"rank": 5}`)
	st.expect("change t1 [~a]")
	s.must(200, "PATCH", "/v1/documents/tasks/a", "", `{"done": true}`)
	st.expect("change t1 [-a]")

	// A second listener on the same stream, on a single document.
	if res := st.add(2, `"document": "tasks/b"`); res.Status != 204 {
		t.Fatalf("add second target = %d", res.Status)
	}
	st.expect("snapshot t2 [+b]")
	s.must(200, "DELETE", "/v1/documents/tasks/b", "", nil)
	st.expect("change t1 [-b]")
	st.expect("change t2 [-b]")

	// After removing a target its events stop; the other keeps going.
	s.must(204, "DELETE", "/v1/listen/"+st.Session+"/targets/1", "", nil)
	s.must(200, "PUT", "/v1/documents/tasks/b", "", `{"done": false}`)
	st.expect("change t2 [+b]")

	// Bad target requests.
	if res := st.add(2, `"document": "tasks/b"`); res.Status != 409 {
		t.Errorf("duplicate target ID = %d", res.Status)
	}
	if res := st.add(3, `"document": "tasks"`); res.Status != 400 {
		t.Errorf("collection path as document = %d", res.Status)
	}
	if res := st.add(3, `"document": "a/b", "query": {"collection": "a"}`); res.Status != 400 {
		t.Errorf("document and query together = %d", res.Status)
	}
	if res := s.call("POST", "/v1/listen/no-such-session/targets", "", `{"id": 1, "document": "a/b"}`); res.Status != 404 {
		t.Errorf("unknown session = %d", res.Status)
	}
}

func TestListenLimitedQuery(t *testing.T) {
	s := start(t, options{})
	for i, id := range []string{"a", "b", "c"} {
		s.must(200, "PUT", "/v1/documents/scores/"+id, "", fmt.Sprintf(`{"points": %d}`, (i+1)*10))
	}
	st := s.listen("")
	st.add(1, `"query": {"collection": "scores", "orderBy": [{"field": "points", "direction": "desc"}], "limit": 2}`)
	st.expect("snapshot t1 [+c +b]") // the snapshot arrives in query order

	s.must(200, "DELETE", "/v1/documents/scores/c", "", nil)
	ev := st.next()
	if got := describe(ev); got != "change t1 [+a -c]" {
		t.Errorf("after deleting the leader: %s, want the runner-up to move in", got)
	}
}

func TestListenIsolatesClients(t *testing.T) {
	s := start(t, options{rules: appRules})
	ann, _, annID := s.signUp("ann@example.com")
	bob, _, bobID := s.signUp("bob@example.com")
	s.must(200, "PUT", "/v1/documents/rooms/r1", ann, `{"members": ["`+annID+`"]}`)

	annStream := s.listen(ann)
	bobStream := s.listen(bob)

	// A session belongs to whoever opened it. To anyone else it does not
	// exist, so its ID is useless if it leaks.
	res := s.call("POST", "/v1/listen/"+annStream.Session+"/targets", bob, `{"id": 1, "document": "rooms/r1"}`)
	if res.Status != 404 {
		t.Errorf("adding a target to someone else's session = %d, want 404", res.Status)
	}
	if res := s.call("DELETE", "/v1/listen/"+annStream.Session+"/targets/1", "", nil); res.Status != 404 {
		t.Errorf("removing a target from someone else's session = %d, want 404", res.Status)
	}

	annStream.add(1, `"query": {"collection": "rooms"}`)
	bobStream.add(1, `"query": {"collection": "rooms"}`)
	annStream.expect("snapshot t1 [+r1]")
	bobStream.expect("snapshot t1 []")

	// Bob may not listen to a room he is not in.
	if res := bobStream.add(2, `"document": "rooms/r1"`); res.Status != 403 {
		t.Errorf("listening to a forbidden document = %d, want 403", res.Status)
	}

	// Adding bob to the room makes it appear on his stream; ann sees it change.
	s.must(200, "PUT", "/v1/documents/rooms/r1", adminKey, `{"members": ["`+annID+`", "`+bobID+`"]}`)
	annStream.expect("change t1 [~r1]")
	bobStream.expect("change t1 [~r1]")

	// Removing ann takes it away from her and leaves it with bob.
	s.must(200, "PUT", "/v1/documents/rooms/r1", adminKey, `{"members": ["`+bobID+`"]}`)
	annStream.expect("change t1 [-r1]")
	bobStream.expect("change t1 [~r1]")
}

func TestListenEndsWhenTokenExpires(t *testing.T) {
	// Token expiry is whole seconds, so two seconds is the shortest
	// lifetime that cannot already be over when the stream opens.
	s := start(t, options{accessTTL: 2 * time.Second})
	access, _, _ := s.signUp("ada@example.com")
	st := s.listen(access)

	deadline := time.After(6 * time.Second)
	select {
	case ev := <-st.events:
		if describe(ev) != "error unauthenticated" {
			t.Errorf("stream ended with %v, want an unauthenticated error", ev)
		}
	case <-deadline:
		t.Fatal("the stream outlived its access token")
	}
	if _, open := <-st.events; open {
		t.Error("the stream stayed open after the error event")
	}
}

func TestListenEndsOnShutdown(t *testing.T) {
	s := start(t, options{})
	st := s.listen("")
	st.add(1, `"document": "a/b"`)
	st.expect("snapshot t1 []")

	s.docs.Close()
	st.expect("error session_closed")
	if _, open := <-st.events; open {
		t.Error("the stream stayed open after the server closed its sessions")
	}
	if stats := s.docs.Stats(); stats.Sessions != 0 {
		t.Errorf("%d sessions left after Close", stats.Sessions)
	}

	// A client that reconnects during shutdown is turned away at once
	// instead of holding the server open; ordinary requests still work.
	if res := s.call("GET", "/v1/listen", "", nil); res.Status != 503 || res.errorCode() != "unavailable" {
		t.Errorf("listen during shutdown = %d %s, want 503 unavailable", res.Status, res.errorCode())
	}
	s.must(404, "GET", "/v1/documents/a/b", "", nil)
}

func TestListenCleansUpWhenClientDisconnects(t *testing.T) {
	s := start(t, options{})
	st := s.listen("")
	st.add(1, `"query": {"collection": "a"}`)
	st.expect("snapshot t1 []")
	if stats := s.docs.Stats(); stats.Sessions != 1 || stats.Targets != 1 {
		t.Fatalf("stats while connected = %+v", stats)
	}

	st.cancel() // the client goes away

	// The server notices asynchronously, so poll for the expected state
	// instead of sleeping for a fixed time.
	deadline := time.Now().Add(5 * time.Second)
	for s.docs.Stats().Sessions != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("session not released after disconnect: %+v", s.docs.Stats())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if stats := s.docs.Stats(); stats.Targets != 0 {
		t.Errorf("targets left behind: %+v", stats)
	}
}

func TestListenSendsKeepAlives(t *testing.T) {
	s := start(t, options{}) // the harness sets a 100ms heartbeat
	req, _ := http.NewRequest("GET", s.URL+"/v1/listen", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	// Read raw bytes until a comment line shows up.
	found := make(chan bool, 1)
	go func() {
		buf := make([]byte, 4096)
		var seen strings.Builder
		for {
			n, err := res.Body.Read(buf)
			seen.Write(buf[:n])
			if strings.Contains(seen.String(), ": ping\n\n") {
				found <- true
				return
			}
			if err != nil {
				found <- false
				return
			}
		}
	}()
	select {
	case ok := <-found:
		if !ok {
			t.Error("stream ended without a keep-alive")
		}
	case <-time.After(5 * time.Second):
		t.Error("no keep-alive within 5s")
	}
}

// TestListenOutlivesServerTimeouts checks that the read and write timeouts
// a production http.Server is configured with do not end a listen stream.
// The read timeout only covers reading the request. The write timeout would
// cut the response off, which is why the handler replaces it with a
// deadline per message.
func TestListenOutlivesServerTimeouts(t *testing.T) {
	s := start(t, options{serverTimeout: 300 * time.Millisecond})
	st := s.listen("")
	st.add(1, `"document": "a/b"`)
	st.expect("snapshot t1 []")

	time.Sleep(900 * time.Millisecond) // three times the timeouts

	s.must(200, "PUT", "/v1/documents/a/b", "", `{}`)
	st.expect("change t1 [+b]")
}
