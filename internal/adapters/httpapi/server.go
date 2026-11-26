// Package httpapi is the driving adapter: it exposes the application's use
// cases over HTTP and JSON, with Server-Sent Events for realtime listeners.
//
// "Driving" means the outside world calls in through it. The handlers do
// three things only: translate the request into domain types (wire.go),
// call one use case in package app, and translate the result or error back.
// There is no business logic here.
package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/bshadmehr98/candledb/internal/adapters/instrumented"
	"github.com/bshadmehr98/candledb/internal/app"
)

// Server holds what the handlers need. Build one with New and mount
// Handler() on an http.Server.
type Server struct {
	docs   *app.Service
	auth   *app.AuthService
	logger *slog.Logger

	corsOrigins  []string
	maxBodyBytes int64
	trustProxy   bool
	heartbeat    time.Duration
	authLimiter  *rateLimiter
	storeStats   *instrumented.Store
	metrics      httpMetrics

	handler http.Handler
}

// Option configures a Server (the functional options pattern; see
// app.Option for the idea).
type Option func(*Server)

// WithLogger sets the logger for requests and errors.
func WithLogger(l *slog.Logger) Option { return func(s *Server) { s.logger = l } }

// WithCORSOrigins sets the browser origins allowed to call the API. A
// single "*" allows every origin, which is safe here because the API
// authenticates with bearer tokens, never with cookies.
func WithCORSOrigins(origins []string) Option { return func(s *Server) { s.corsOrigins = origins } }

// WithMaxBodyBytes limits the size of a request body.
func WithMaxBodyBytes(n int64) Option { return func(s *Server) { s.maxBodyBytes = n } }

// WithTrustProxy makes the server take the client's address from the
// X-Forwarded-For header. Enable it only behind a reverse proxy that sets
// the header; otherwise clients could forge their address.
func WithTrustProxy(trust bool) Option { return func(s *Server) { s.trustProxy = trust } }

// WithAuthRateLimit limits sign-up, sign-in and refresh attempts per client
// address: burst requests at once, refilled at perMinute.
func WithAuthRateLimit(perMinute float64, burst int) Option {
	return func(s *Server) { s.authLimiter = newRateLimiter(perMinute/60, burst, time.Now) }
}

// WithStoreStats exposes the counters of an instrumented store on /metrics.
func WithStoreStats(st *instrumented.Store) Option { return func(s *Server) { s.storeStats = st } }

// WithHeartbeat sets how often an idle listen stream sends a keep-alive.
// A duration that is not positive is ignored and the default stays.
func WithHeartbeat(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.heartbeat = d
		}
	}
}

// New builds the HTTP API on top of the use cases.
func New(docs *app.Service, auth *app.AuthService, opts ...Option) *Server {
	s := &Server{
		docs:         docs,
		auth:         auth,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		corsOrigins:  []string{"*"},
		maxBodyBytes: 10 << 20,
		heartbeat:    20 * time.Second,
		authLimiter:  newRateLimiter(30.0/60, 10, time.Now),
	}
	for _, opt := range opts {
		opt(s)
	}
	s.handler = s.routes()
	return s
}

// Handler returns the complete HTTP handler, middleware included.
func (s *Server) Handler() http.Handler { return s.handler }

// routes builds the routing table. Since Go 1.22 the standard library's
// ServeMux matches on method and path patterns, so no router dependency is
// needed. "{path...}" captures the rest of the URL, slashes included.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /metrics", s.handleMetrics)

	limited := s.rateLimit(s.authLimiter)
	mux.Handle("POST /v1/auth/signup", limited(http.HandlerFunc(s.handleSignUp)))
	mux.Handle("POST /v1/auth/signin", limited(http.HandlerFunc(s.handleSignIn)))
	mux.Handle("POST /v1/auth/refresh", limited(http.HandlerFunc(s.handleRefresh)))
	mux.HandleFunc("POST /v1/auth/signout", s.handleSignOut)
	mux.HandleFunc("GET /v1/auth/me", s.handleMe)

	mux.HandleFunc("GET /v1/documents/{path...}", s.handleGet)
	mux.HandleFunc("PUT /v1/documents/{path...}", s.handleSet)
	mux.HandleFunc("PATCH /v1/documents/{path...}", s.handleUpdate)
	mux.HandleFunc("DELETE /v1/documents/{path...}", s.handleDelete)
	mux.HandleFunc("POST /v1/documents/{path...}", s.handleAdd)

	mux.HandleFunc("POST /v1/commit", s.handleCommit)
	mux.HandleFunc("POST /v1/query", s.handleQuery)

	mux.HandleFunc("GET /v1/listen", s.handleListen)
	mux.HandleFunc("POST /v1/listen/{session}/targets", s.handleAddTarget)
	mux.HandleFunc("DELETE /v1/listen/{session}/targets/{target}", s.handleRemoveTarget)

	// Middleware wraps the mux from the inside out: the last one listed is
	// the first to see a request.
	return chain(mux,
		s.authenticate,
		s.limitBody,
		s.cors,
		s.observe,
		s.recoverPanics,
	)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReady reports whether the server can reach its storage.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.docs.Ping(ctx); err != nil {
		s.logger.Error("readiness check failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
