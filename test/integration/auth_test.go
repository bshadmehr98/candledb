package integration

import (
	"strings"
	"testing"
)

func TestAuthFlow(t *testing.T) {
	s := start(t, options{})

	signup := s.must(200, "POST", "/v1/auth/signup", "", `{"email": "Ada@Example.com", "password": "password123"}`)
	access := signup.Body["accessToken"].(string)
	refresh := signup.Body["refreshToken"].(string)
	user := signup.Body["user"].(map[string]any)
	if user["email"] != "ada@example.com" || user["id"] == "" {
		t.Errorf("user = %v", user)
	}
	if signup.Header.Get("Cache-Control") != "no-store" {
		t.Error("token response may be cached")
	}
	if _, leaked := user["passwordHash"]; leaked || containsAny(signup.Raw, "password123", "$2a$") {
		t.Errorf("the response leaks the password or its hash: %s", signup.Raw)
	}

	me := s.must(200, "GET", "/v1/auth/me", access, nil)
	if me.Body["id"] != user["id"] {
		t.Errorf("me = %v", me.Body)
	}
	s.must(401, "GET", "/v1/auth/me", "", nil)

	if res := s.call("POST", "/v1/auth/signup", "", `{"email": "ada@example.com", "password": "password123"}`); res.Status != 409 {
		t.Errorf("duplicate sign-up = %d", res.Status)
	}
	if res := s.call("POST", "/v1/auth/signup", "", `{"email": "bob@example.com", "password": "short"}`); res.Status != 400 {
		t.Errorf("weak password = %d", res.Status)
	}

	s.must(200, "POST", "/v1/auth/signin", "", `{"email": "ada@example.com", "password": "password123"}`)
	wrong := s.must(401, "POST", "/v1/auth/signin", "", `{"email": "ada@example.com", "password": "wrong-password"}`)
	unknown := s.must(401, "POST", "/v1/auth/signin", "", `{"email": "nobody@example.com", "password": "wrong-password"}`)
	if wrong.Raw != unknown.Raw {
		t.Errorf("sign-in failures differ:\n%s\n%s", wrong.Raw, unknown.Raw)
	}

	// Refresh gives a new pair and retires the old refresh token.
	refreshed := s.must(200, "POST", "/v1/auth/refresh", "", map[string]string{"refreshToken": refresh})
	newRefresh := refreshed.Body["refreshToken"].(string)
	s.must(200, "GET", "/v1/auth/me", refreshed.Body["accessToken"].(string), nil)
	s.must(401, "POST", "/v1/auth/refresh", "", map[string]string{"refreshToken": refresh})

	s.must(204, "POST", "/v1/auth/signout", "", map[string]string{"refreshToken": newRefresh})
	s.must(401, "POST", "/v1/auth/refresh", "", map[string]string{"refreshToken": newRefresh})
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestBadCredentialsAreRejectedEverywhere(t *testing.T) {
	s := start(t, options{})
	access, _, _ := s.signUp("ada@example.com")
	tampered := access[:len(access)-2] + "xx"

	for _, token := range []string{"not-a-token", tampered} {
		// A request with a bad token fails even on endpoints that allow
		// anonymous access; it is never quietly treated as anonymous.
		if res := s.call("GET", "/v1/documents/a/b", token, nil); res.Status != 401 || res.errorCode() != "unauthenticated" {
			t.Errorf("GET with token %q = %d %s", token, res.Status, res.errorCode())
		}
		if res := s.call("PUT", "/v1/documents/a/b", token, `{}`); res.Status != 401 {
			t.Errorf("PUT with token %q = %d", token, res.Status)
		}
	}

	req := s.call("GET", "/v1/documents/a/b", "", nil)
	if req.Status != 404 {
		t.Errorf("anonymous GET under open rules = %d, want 404", req.Status)
	}
}

func TestAuthRateLimit(t *testing.T) {
	s := start(t, options{authPerMin: 1, authBurst: 3})
	body := `{"email": "ada@example.com", "password": "wrong-password"}`
	for i := range 3 {
		if res := s.call("POST", "/v1/auth/signin", "", body); res.Status != 401 {
			t.Fatalf("attempt %d = %d, want 401", i+1, res.Status)
		}
	}
	res := s.call("POST", "/v1/auth/signin", "", body)
	if res.Status != 429 || res.errorCode() != "resource_exhausted" || res.Header.Get("Retry-After") == "" {
		t.Errorf("fourth attempt = %d %s (Retry-After %q), want 429", res.Status, res.errorCode(), res.Header.Get("Retry-After"))
	}
	// Other endpoints are not affected.
	s.must(404, "GET", "/v1/documents/a/b", "", nil)
}

const appRules = `
function signedIn() { return request.auth != null; }

match /profiles/{uid} {
	allow read: if true;
	allow write: if signedIn() && request.auth.uid == uid
	             && request.resource.data.keys().hasOnly(["name", "bio"]);
}
match /users/{uid}/todos/{todo} {
	allow read, write: if signedIn() && request.auth.uid == uid;
}
match /rooms/{room} {
	allow read: if signedIn() && request.auth.uid in resource.data.members;
	allow create: if signedIn() && request.auth.uid in request.resource.data.members;
	match /messages/{msg} {
		allow read: if signedIn() && request.auth.uid in get("rooms/" + room).data.members;
		allow create: if signedIn()
		              && request.auth.uid in get("rooms/" + room).data.members
		              && request.resource.data.from == request.auth.uid
		              && request.resource.data.text is string
		              && request.resource.data.text.size() <= 200;
	}
}
`

func TestRulesEndToEnd(t *testing.T) {
	s := start(t, options{rules: appRules})
	ann, _, annID := s.signUp("ann@example.com")
	bob, _, bobID := s.signUp("bob@example.com")
	cat, _, _ := s.signUp("cat@example.com")

	// Profiles: world readable, writable by their owner, and only with the
	// allowed fields.
	s.must(200, "PUT", "/v1/documents/profiles/"+annID, ann, `{"name": "Ann"}`)
	s.must(200, "GET", "/v1/documents/profiles/"+annID, "", nil)
	s.must(403, "PUT", "/v1/documents/profiles/"+annID, bob, `{"name": "Mallory"}`)
	s.must(403, "PUT", "/v1/documents/profiles/"+annID, "", `{"name": "Anon"}`)
	s.must(403, "PUT", "/v1/documents/profiles/"+annID, ann, `{"name": "Ann", "admin": true}`)
	s.must(403, "DELETE", "/v1/documents/profiles/"+annID, bob, nil)

	// Private subcollection per user.
	s.must(200, "PUT", "/v1/documents/users/"+annID+"/todos/t1", ann, `{"title": "write tests"}`)
	s.must(403, "GET", "/v1/documents/users/"+annID+"/todos/t1", bob, nil)
	s.must(403, "GET", "/v1/documents/users/"+annID+"/todos/none", bob, nil) // no hint that it is missing
	s.must(404, "GET", "/v1/documents/users/"+annID+"/todos/none", ann, nil)
	if ids := s.queryIDs(bob, `{"collection": "users/`+annID+`/todos"}`); ids != "" {
		t.Errorf("bob's query sees ann's todos: %q", ids)
	}
	if ids := s.queryIDs(ann, `{"collection": "users/`+annID+`/todos"}`); ids != "t1" {
		t.Errorf("ann's query = %q", ids)
	}

	// Rooms: membership stored in the room, checked from messages with get().
	room := `{"members": ["` + annID + `", "` + bobID + `"]}`
	s.must(403, "PUT", "/v1/documents/rooms/r1", cat, room) // cannot create a room without being in it
	s.must(200, "PUT", "/v1/documents/rooms/r1", ann, room)
	s.must(200, "PUT", "/v1/documents/rooms/r2", cat, roomOf(t, s, cat))

	s.must(200, "POST", "/v1/documents/rooms/r1/messages", bob, `{"from": "`+bobID+`", "text": "hi"}`)
	s.must(403, "POST", "/v1/documents/rooms/r1/messages", bob, `{"from": "`+annID+`", "text": "forged"}`)
	s.must(403, "POST", "/v1/documents/rooms/r1/messages", cat, `{"from": "x", "text": "intruder"}`)
	s.must(403, "POST", "/v1/documents/rooms/r1/messages", bob, `{"from": "`+bobID+`", "text": 42}`)
	if ids := s.queryIDs(cat, `{"collection": "rooms/r1/messages"}`); ids != "" {
		t.Errorf("a non-member reads the room's messages: %q", ids)
	}
	res := s.must(200, "POST", "/v1/query", ann, `{"collection": "rooms/r1/messages"}`)
	if n := len(res.Body["documents"].([]any)); n != 1 {
		t.Errorf("a member sees %d messages, want 1", n)
	}
	// Listing rooms returns only the rooms the caller belongs to.
	if ids := s.queryIDs(bob, `{"collection": "rooms"}`); ids != "r1" {
		t.Errorf("bob's rooms = %q, want r1", ids)
	}

	// Anything not mentioned in the rules is closed...
	s.must(403, "PUT", "/v1/documents/elsewhere/x", ann, `{}`)
	s.must(403, "GET", "/v1/documents/elsewhere/x", ann, nil)
	// ...except to the admin key, which bypasses the rules.
	s.must(200, "PUT", "/v1/documents/elsewhere/x", adminKey, `{}`)
	if ids := s.queryIDs(adminKey, `{"collection": "rooms"}`); ids != "r1 r2" {
		t.Errorf("admin's rooms = %q, want r1 r2", ids)
	}
}

// roomOf returns room data whose only member is the caller of the token.
func roomOf(t *testing.T, s *server, token string) string {
	t.Helper()
	me := s.must(200, "GET", "/v1/auth/me", token, nil)
	return `{"members": ["` + me.Body["id"].(string) + `"]}`
}
