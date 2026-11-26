package httpapi

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/domain"
)

// middleware is a function that wraps a handler to add behaviour around it.
// This is the decorator pattern applied to http.Handler: each middleware
// returns something with the same interface as what it was given.
type middleware func(http.Handler) http.Handler

// chain applies middleware so that the first one listed is closest to the
// handler and the last one is outermost.
func chain(h http.Handler, mws ...middleware) http.Handler {
	for _, mw := range mws {
		h = mw(h)
	}
	return h
}

// contextKey is a private type for context keys. Because no other package
// can create a value of this type, no other package can collide with, read
// or overwrite what is stored under it.
type contextKey int

const (
	keyPrincipal contextKey = iota
	keyRequestID
)

func principal(ctx context.Context) app.Principal {
	p, _ := ctx.Value(keyPrincipal).(app.Principal)
	return p
}

func requestID(ctx context.Context) string {
	id, _ := ctx.Value(keyRequestID).(string)
	return id
}

// recoverPanics turns a panic in a handler into a 500 response. Without it
// net/http would only close the connection, and the client would see a
// network error with no hint of what happened.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil || v == http.ErrAbortHandler {
				return
			}
			s.logger.Error("panic in handler", "panic", v, "stack", string(debug.Stack()),
				"method", r.Method, "path", r.URL.Path)
			writeJSON(w, http.StatusInternalServerError,
				errorBody{errorDetail{Code: "internal", Message: "internal error"}})
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder remembers the status code a handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the real writer, which is how
// the listen handler can still flush and set deadlines through this wrapper.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// observe assigns a request ID, logs the request and counts it.
func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" || len(id) > 64 {
			id = rand.Text()[:12]
		}
		w.Header().Set("X-Request-Id", id)
		r = r.WithContext(context.WithValue(r.Context(), keyRequestID, id))

		rec := &statusRecorder{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(rec, r)

		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		s.metrics.record(rec.status)
		s.logger.Info("request",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(), "request_id", id)
	})
}

// cors answers cross-origin requests from browsers, including the OPTIONS
// "preflight" a browser sends before any request carrying an Authorization
// header.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Add("Vary", "Origin")
			// Without this a page on another origin cannot read these
			// response headers, even when the request itself is allowed.
			w.Header().Set("Access-Control-Expose-Headers", "Retry-After, X-Request-Id")
			switch {
			case slices.Contains(s.corsOrigins, "*"):
				w.Header().Set("Access-Control-Allow-Origin", "*")
			case slices.Contains(s.corsOrigins, origin):
				w.Header().Set("Access-Control-Allow-Origin", origin)
			}
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-Id")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// limitBody caps the size of request bodies. Reading past the cap fails
// with *http.MaxBytesError, which classify maps to 413.
func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// authenticate resolves the bearer token into a Principal and stores it in
// the request context. A request without a token continues as anonymous; a
// request with a bad token is rejected, so an expired session is noticed
// instead of silently degrading to anonymous access.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var bearer string
		if h := r.Header.Get("Authorization"); h != "" {
			scheme, token, ok := strings.Cut(h, " ")
			if !ok || !strings.EqualFold(scheme, "Bearer") {
				s.writeError(w, r, fmt.Errorf("%w: Authorization header must be \"Bearer <token>\"", domain.ErrUnauthenticated))
				return
			}
			bearer = strings.TrimSpace(token)
		}
		p, err := s.auth.Authenticate(bearer)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), keyPrincipal, p)))
	})
}

// clientAddr returns the address used to identify a client for rate limits.
func (s *Server) clientAddr(r *http.Request) string {
	if s.trustProxy {
		if first, _, _ := strings.Cut(r.Header.Get("X-Forwarded-For"), ","); strings.TrimSpace(first) != "" {
			return strings.TrimSpace(first)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimit returns middleware that rejects requests from a client address
// that has used up its allowance.
func (s *Server) rateLimit(l *rateLimiter) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !l.allow(s.clientAddr(r)) {
				w.Header().Set("Retry-After", "10")
				s.writeError(w, r, fmt.Errorf("%w: too many attempts, try again shortly", domain.ErrResourceExhausted))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// rateLimiter is a token bucket per key. A bucket holds up to burst tokens
// and gains rate tokens per second; each request spends one. Short bursts
// pass, a sustained flood does not.
type rateLimiter struct {
	rate  float64
	burst float64
	now   func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	seen   time.Time
}

func newRateLimiter(perSecond float64, burst int, now func() time.Time) *rateLimiter {
	return &rateLimiter{rate: perSecond, burst: float64(burst), now: now, buckets: map[string]*bucket{}}
}

func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()

	// Forget clients whose bucket would be full again anyway; this keeps
	// the map from growing with every address ever seen.
	if len(l.buckets) > 10_000 {
		idle := time.Duration(l.burst / l.rate * float64(time.Second))
		for k, b := range l.buckets {
			if now.Sub(b.seen) > idle {
				delete(l.buckets, k)
			}
		}
	}

	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, seen: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.seen).Seconds()*l.rate)
	b.seen = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
