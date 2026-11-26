package integration

import (
	"net/http"
	"strings"
	"testing"
)

func TestHealthAndReadiness(t *testing.T) {
	s := start(t, options{})
	if res := s.must(200, "GET", "/healthz", "", nil); res.Body["status"] != "ok" {
		t.Errorf("healthz = %v", res.Body)
	}
	if res := s.must(200, "GET", "/readyz", "", nil); res.Body["status"] != "ok" {
		t.Errorf("readyz = %v", res.Body)
	}
}

func TestMetrics(t *testing.T) {
	s := start(t, options{})
	s.must(403, "GET", "/metrics", "", nil)
	access, _, _ := s.signUp("ada@example.com")
	s.must(403, "GET", "/metrics", access, nil)

	s.must(200, "PUT", "/v1/documents/a/b", "", `{}`)
	s.must(404, "GET", "/v1/documents/a/missing", "", nil)
	s.queryIDs("", `{"collection": "a"}`)

	res := s.must(200, "GET", "/metrics", adminKey, nil)
	for _, want := range []string{
		`candledb_http_requests_total{class="2xx_3xx"}`,
		`candledb_http_requests_total{class="4xx"} 3`,
		`candledb_database_version 1`,
		`candledb_listen_sessions 0`,
		`candledb_store_operations_total{op="apply"} 1`,
		`candledb_store_operations_total{op="query"} 1`,
		`candledb_store_errors_total{op="get"} 0`, // "not found" is not a store error
	} {
		if !strings.Contains(res.Raw, want) {
			t.Errorf("metrics lack %q\n%s", want, res.Raw)
		}
	}
}

func TestRequestID(t *testing.T) {
	s := start(t, options{})
	generated := s.call("GET", "/healthz", "", nil).Header.Get("X-Request-Id")
	if generated == "" {
		t.Error("no request ID was generated")
	}

	req, _ := http.NewRequest("GET", s.URL+"/healthz", nil)
	req.Header.Set("X-Request-Id", "trace-123")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if got := res.Header.Get("X-Request-Id"); got != "trace-123" {
		t.Errorf("request ID = %q, want the caller's", got)
	}
}

func TestCORS(t *testing.T) {
	preflight := func(s *server, origin string) *http.Response {
		req, _ := http.NewRequest("OPTIONS", s.URL+"/v1/documents/a/b", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "PUT")
		req.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}

	open := start(t, options{})
	res := preflight(open, "https://app.example")
	if res.StatusCode != 204 || res.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("preflight = %d, allow-origin %q", res.StatusCode, res.Header.Get("Access-Control-Allow-Origin"))
	}
	if !strings.Contains(res.Header.Get("Access-Control-Allow-Headers"), "Authorization") ||
		!strings.Contains(res.Header.Get("Access-Control-Allow-Methods"), "PATCH") {
		t.Errorf("preflight headers = %v", res.Header)
	}

	restricted := start(t, options{corsOrigins: []string{"https://app.example"}})
	if got := preflight(restricted, "https://app.example").Header.Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Errorf("allowed origin got %q", got)
	}
	if got := preflight(restricted, "https://evil.example").Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("foreign origin got %q", got)
	}

	// Error responses need the header too, or the browser hides the error
	// from the page's JavaScript.
	req, _ := http.NewRequest("GET", restricted.URL+"/v1/documents/a/missing", nil)
	req.Header.Set("Origin", "https://app.example")
	errRes, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	errRes.Body.Close()
	if !strings.Contains(errRes.Header.Get("Access-Control-Expose-Headers"), "Retry-After") {
		t.Errorf("Retry-After is not exposed to browsers: %v", errRes.Header)
	}
	if errRes.StatusCode != 404 || errRes.Header.Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Errorf("error response: %d, allow-origin %q", errRes.StatusCode, errRes.Header.Get("Access-Control-Allow-Origin"))
	}
}
