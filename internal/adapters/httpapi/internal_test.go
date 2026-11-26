package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bshadmehr98/candledb/internal/domain"
)

func TestRateLimiter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newRateLimiter(1, 3, func() time.Time { return now }) // 1 token per second, bucket of 3

	for i := range 3 {
		if !l.allow("a") {
			t.Fatalf("request %d of the burst was refused", i+1)
		}
	}
	if l.allow("a") {
		t.Error("a fourth request passed with an empty bucket")
	}
	if !l.allow("b") {
		t.Error("one client's usage limited another")
	}

	now = now.Add(2 * time.Second) // two tokens come back
	for i := range 2 {
		if !l.allow("a") {
			t.Errorf("refilled token %d was refused", i+1)
		}
	}
	if l.allow("a") {
		t.Error("more tokens were refilled than time allows")
	}

	now = now.Add(time.Hour) // refill stops at the bucket size
	for i := range 3 {
		if !l.allow("a") {
			t.Fatalf("request %d after a long pause was refused", i+1)
		}
	}
	if l.allow("a") {
		t.Error("the bucket grew beyond its size")
	}
}

func TestRateLimiterForgetsIdleClients(t *testing.T) {
	now := time.Now()
	l := newRateLimiter(1, 2, func() time.Time { return now })
	for i := range 10_001 {
		l.allow(fmt.Sprintf("client-%d", i))
	}
	now = now.Add(time.Minute)
	l.allow("one more")
	if len(l.buckets) > 10 {
		t.Errorf("%d buckets kept after every client went idle", len(l.buckets))
	}
}

func TestClientAddr(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.1:5555"
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")

	direct := &Server{}
	if got := direct.clientAddr(req); got != "10.0.0.1" {
		t.Errorf("without trust-proxy: %q; the header must be ignored, or clients could forge it", got)
	}
	proxied := &Server{trustProxy: true}
	if got := proxied.clientAddr(req); got != "203.0.113.9" {
		t.Errorf("with trust-proxy: %q", got)
	}
}

func TestPanicBecomesInternalError(t *testing.T) {
	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := s.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Code != 500 {
		t.Fatalf("status = %d", rec.Code)
	}
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "internal" {
		t.Errorf("body = %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Error("the panic value leaked to the client")
	}
}

func TestInternalErrorsAreNotLeaked(t *testing.T) {
	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rec := httptest.NewRecorder()
	s.writeError(rec, httptest.NewRequest("GET", "/", nil), errors.New("sql: connection to /secret/path failed"))
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}

	// A client error keeps its message, however deeply it is wrapped.
	rec = httptest.NewRecorder()
	wrapped := fmt.Errorf("write 3: %w", fmt.Errorf("%w: document a/b", domain.ErrNotFound))
	s.writeError(rec, httptest.NewRequest("GET", "/", nil), wrapped)
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "document a/b") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

func parseWrite(t *testing.T, raw string) (domain.Write, error) {
	t.Helper()
	var wr writeRequest
	if err := json.Unmarshal([]byte(raw), &wr); err != nil {
		t.Fatalf("bad test JSON: %v", err)
	}
	return wr.toDomain()
}

func TestWriteTranslation(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	existing := &domain.Document{Path: "c/d", Data: domain.Map{
		"keep":   domain.Int(1),
		"nested": domain.Map{"a": domain.Int(1), "b": domain.Int(2)},
		"list":   domain.Array{domain.Int(1)},
	}}

	tests := []struct {
		name  string
		write string
		want  string
	}{
		{
			"set replaces and lifts nested transforms out of the data",
			`{"op": "set", "path": "c/d", "data": {"x": 1, "deep": {"n": {"$increment": 2}, "y": true}}}`,
			`{"x": 1, "deep": {"n": 2, "y": true}}`,
		},
		{
			"merge descends into maps instead of replacing them",
			`{"op": "set", "merge": true, "path": "c/d", "data": {"nested": {"b": 20, "c": 30}}}`,
			`{"keep": 1, "nested": {"a": 1, "b": 20, "c": 30}, "list": [1]}`,
		},
		{
			"merge can delete a field",
			`{"op": "set", "merge": true, "path": "c/d", "data": {"nested": {"a": {"$delete": true}}}}`,
			`{"keep": 1, "nested": {"b": 2}, "list": [1]}`,
		},
		{
			"merge treats an empty map as a value",
			`{"op": "set", "merge": true, "path": "c/d", "data": {"nested": {}}}`,
			`{"keep": 1, "nested": {}, "list": [1]}`,
		},
		{
			"update replaces a map given as a whole",
			`{"op": "update", "path": "c/d", "data": {"nested": {"z": 26}}}`,
			`{"keep": 1, "nested": {"z": 26}, "list": [1]}`,
		},
		{
			"update reaches into a map with a dotted key",
			`{"op": "update", "path": "c/d", "data": {"nested.a": 100, "list": {"$arrayUnion": [2]}}}`,
			`{"keep": 1, "nested": {"a": 100, "b": 2}, "list": [1, 2]}`,
		},
		{
			"a timestamp object is a value, not a nested map",
			`{"op": "set", "merge": true, "path": "c/d", "data": {"at": {"$timestamp": "2026-01-01T00:00:00Z"}}}`,
			`{"keep": 1, "nested": {"a": 1, "b": 2}, "list": [1], "at": {"$timestamp": "2026-01-01T00:00:00Z"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, err := parseWrite(t, tt.write)
			if err != nil {
				t.Fatal(err)
			}
			got, err := w.Apply(existing, now)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := domain.ParseMapJSON([]byte(tt.want))
			if !domain.Equal(got, want) {
				gotJSON, _ := json.Marshal(got)
				t.Errorf("result = %s\nwant     %s", gotJSON, tt.want)
			}
		})
	}
}

func TestWriteTranslationRejects(t *testing.T) {
	for name, raw := range map[string]string{
		"merge with update":         `{"op": "update", "merge": true, "path": "c/d", "data": {}}`,
		"missing data":              `{"op": "set", "path": "c/d"}`,
		"bad path":                  `{"op": "set", "path": "c", "data": {}}`,
		"transform inside an array": `{"op": "set", "path": "c/d", "data": {"l": [{"$increment": 1}]}}`,
		"array union without list":  `{"op": "set", "path": "c/d", "data": {"l": {"$arrayUnion": 1}}}`,
		"update with empty path":    `{"op": "update", "path": "c/d", "data": {"a..b": 1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseWrite(t, raw); !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("error = %v, want ErrInvalidArgument", err)
			}
		})
	}
}
