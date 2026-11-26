// Package integration tests the assembled server from the outside: real
// SQLite file, real password hashing and tokens, real HTTP. Nothing is
// replaced by a fake, so these tests exercise the same wiring as
// cmd/candledb and catch mistakes that sit between the components.
package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/bshadmehr98/candledb/internal/adapters/bcrypthash"
	"github.com/bshadmehr98/candledb/internal/adapters/httpapi"
	"github.com/bshadmehr98/candledb/internal/adapters/instrumented"
	"github.com/bshadmehr98/candledb/internal/adapters/jwtauth"
	"github.com/bshadmehr98/candledb/internal/adapters/sqlite"
	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/rules"
)

const (
	adminKey  = "integration-test-admin-key-0123456789"
	jwtSecret = "integration-test-jwt-secret-0123456789abcdef"
)

// options adjusts how a test server is built. The zero value gives a server
// that allows everything.
type options struct {
	rules       string // rules source; empty means allow all
	dbPath      string // reuse a database file; empty means a fresh temporary one
	accessTTL   time.Duration
	authPerMin  float64
	authBurst   int
	maxBody     int64
	corsOrigins []string
	limits      *app.Limits
	// serverTimeout sets http.Server's ReadTimeout and WriteTimeout, as a
	// production server has; zero means none.
	serverTimeout time.Duration
}

type server struct {
	t      *testing.T
	URL    string
	dbPath string
	docs   *app.Service
	stop   func()
}

// start builds a complete server the same way cmd/candledb does and serves
// it on a local port chosen by httptest.
func start(t *testing.T, opt options) *server {
	t.Helper()
	ctx := context.Background()
	if opt.dbPath == "" {
		opt.dbPath = filepath.Join(t.TempDir(), "candledb.db")
	}
	db, err := sqlite.Open(ctx, opt.dbPath)
	if err != nil {
		t.Fatal(err)
	}

	ruleset := rules.AllowAll()
	if opt.rules != "" {
		if ruleset, err = rules.Parse(opt.rules); err != nil {
			t.Fatalf("test rules: %v", err)
		}
	}
	store := instrumented.Wrap(db)
	appOpts := []app.Option{app.WithRules(ruleset)}
	if opt.limits != nil {
		appOpts = append(appOpts, app.WithLimits(*opt.limits))
	}
	docs, err := app.New(ctx, store, appOpts...)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := jwtauth.New([]byte(jwtSecret), "candledb-test", app.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	// MinCost keeps bcrypt fast; the algorithm is otherwise the real one.
	auth, err := app.NewAuthService(db.Users(), bcrypthash.New(bcrypt.MinCost), tokens, app.SystemClock{},
		app.AuthConfig{AdminKey: adminKey, AccessTTL: opt.accessTTL})
	if err != nil {
		t.Fatal(err)
	}

	apiOpts := []httpapi.Option{httpapi.WithStoreStats(store), httpapi.WithHeartbeat(100 * time.Millisecond)}
	if opt.authBurst > 0 {
		apiOpts = append(apiOpts, httpapi.WithAuthRateLimit(opt.authPerMin, opt.authBurst))
	} else {
		apiOpts = append(apiOpts, httpapi.WithAuthRateLimit(60_000, 1000))
	}
	if opt.maxBody > 0 {
		apiOpts = append(apiOpts, httpapi.WithMaxBodyBytes(opt.maxBody))
	}
	if opt.corsOrigins != nil {
		apiOpts = append(apiOpts, httpapi.WithCORSOrigins(opt.corsOrigins))
	}
	ts := httptest.NewUnstartedServer(httpapi.New(docs, auth, apiOpts...).Handler())
	ts.Config.ReadTimeout = opt.serverTimeout
	ts.Config.WriteTimeout = opt.serverTimeout
	ts.Start()

	s := &server{t: t, URL: ts.URL, dbPath: opt.dbPath, docs: docs}
	stopped := false
	s.stop = func() {
		if stopped {
			return
		}
		stopped = true
		docs.Close() // ends listen streams, which would otherwise keep ts.Close waiting
		ts.Close()
		if err := db.Close(); err != nil {
			t.Errorf("closing database: %v", err)
		}
	}
	t.Cleanup(s.stop)
	return s
}

// response is a decoded HTTP response.
type response struct {
	Status int
	Header http.Header
	Body   map[string]any
	Raw    string
}

// errorCode returns the "code" of an error response, or "".
func (r response) errorCode() string {
	e, _ := r.Body["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

// call performs one request. body may be nil, a string of raw JSON, or any
// value to be encoded as JSON. token may be empty.
func (s *server) call(method, path, token string, body any) response {
	s.t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			s.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, s.URL+path, reader)
	if err != nil {
		s.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := response{Status: res.StatusCode, Header: res.Header, Raw: string(raw)}
	_ = json.Unmarshal(raw, &out.Body)
	return out
}

// must performs a request and fails the test unless it returns wantStatus.
func (s *server) must(wantStatus int, method, path, token string, body any) response {
	s.t.Helper()
	res := s.call(method, path, token, body)
	if res.Status != wantStatus {
		s.t.Fatalf("%s %s = %d, want %d\n%s", method, path, res.Status, wantStatus, res.Raw)
	}
	return res
}

// signUp registers a user and returns the access token, refresh token and
// user ID.
func (s *server) signUp(email string) (access, refresh, uid string) {
	s.t.Helper()
	res := s.must(200, "POST", "/v1/auth/signup", "", map[string]string{"email": email, "password": "password123"})
	user := res.Body["user"].(map[string]any)
	return res.Body["accessToken"].(string), res.Body["refreshToken"].(string), user["id"].(string)
}

// queryIDs runs a query and returns the IDs of the results, space separated.
func (s *server) queryIDs(token, queryJSON string) string {
	s.t.Helper()
	res := s.must(200, "POST", "/v1/query", token, queryJSON)
	var ids []string
	for _, d := range res.Body["documents"].([]any) {
		ids = append(ids, d.(map[string]any)["id"].(string))
	}
	return strings.Join(ids, " ")
}

// stream is an open listen connection.
type stream struct {
	t       *testing.T
	s       *server
	token   string
	Session string
	events  chan map[string]any
	cancel  context.CancelFunc
}

// listen opens the realtime stream and waits for its "ready" message.
func (s *server) listen(token string) *stream {
	s.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", s.URL+"/v1/listen", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		s.t.Fatalf("opening listen stream: %v", err)
	}
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		cancel()
		s.t.Fatalf("listen: status %d, content type %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	st := &stream{t: s.t, s: s, token: token, events: make(chan map[string]any, 256), cancel: cancel}
	s.t.Cleanup(cancel)

	// A goroutine reads the never-ending response and forwards each
	// message on a channel, which the test then reads with a timeout.
	go func() {
		defer res.Body.Close()
		defer close(st.events)
		scanner := bufio.NewScanner(res.Body)
		scanner.Buffer(make([]byte, 64*1024), 16<<20)
		for scanner.Scan() {
			payload, isData := strings.CutPrefix(scanner.Text(), "data: ")
			if !isData {
				continue // blank separator lines and ": ping" comments
			}
			var ev map[string]any
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				s.t.Errorf("malformed event %q: %v", payload, err)
				return
			}
			st.events <- ev
		}
	}()

	ready := st.next()
	if ready["type"] != "ready" {
		s.t.Fatalf("first message = %v, want ready", ready)
	}
	st.Session = ready["session"].(string)
	return st
}

// next returns the next message on the stream.
func (st *stream) next() map[string]any {
	st.t.Helper()
	select {
	case ev, open := <-st.events:
		if !open {
			st.t.Fatal("stream ended while waiting for an event")
		}
		return ev
	case <-time.After(5 * time.Second):
		st.t.Fatal("timed out waiting for an event")
	}
	return nil
}

// expect reads the next message and compares it with a compact description
// such as "snapshot t1 [+ada +bob]" or "change t2 [~ada -bob]".
func (st *stream) expect(want string) map[string]any {
	st.t.Helper()
	ev := st.next()
	if got := describe(ev); got != want {
		st.t.Fatalf("event = %s, want %s", got, want)
	}
	return ev
}

func describe(ev map[string]any) string {
	if ev["type"] == "error" {
		return fmt.Sprintf("error %v", ev["code"])
	}
	var parts []string
	changes, _ := ev["changes"].([]any)
	for _, raw := range changes {
		c := raw.(map[string]any)
		switch c["kind"] {
		case "removed":
			path := c["path"].(string)
			parts = append(parts, "-"+path[strings.LastIndex(path, "/")+1:])
		case "added":
			parts = append(parts, "+"+c["doc"].(map[string]any)["id"].(string))
		default:
			parts = append(parts, "~"+c["doc"].(map[string]any)["id"].(string))
		}
	}
	return fmt.Sprintf("%v t%v [%s]", ev["type"], ev["target"], strings.Join(parts, " "))
}

// add registers a listener on the stream. target is the JSON of the
// request without its id, e.g. `"document": "users/ada"`.
func (st *stream) add(id int, target string) response {
	st.t.Helper()
	return st.s.call("POST", "/v1/listen/"+st.Session+"/targets", st.token, fmt.Sprintf(`{"id": %d, %s}`, id, target))
}
