package httpapi

import (
	"fmt"
	"net/http"
	"sync/atomic"

	"github.com/bshadmehr98/candledb/internal/adapters/instrumented"
	"github.com/bshadmehr98/candledb/internal/domain"
)

// httpMetrics counts responses by status class.
type httpMetrics struct {
	ok, clientErr, serverErr atomic.Int64
}

func (m *httpMetrics) record(status int) {
	switch {
	case status >= 500:
		m.serverErr.Add(1)
	case status >= 400:
		m.clientErr.Add(1)
	default:
		m.ok.Add(1)
	}
}

// handleMetrics serves counters in the Prometheus text format, which is
// simple enough to write by hand: one "name{labels} value" line per series.
// The endpoint is for operators, so it requires the admin key.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !principal(r.Context()).Admin {
		s.writeError(w, r, fmt.Errorf("%w: metrics require the admin key", domain.ErrPermissionDenied))
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	fmt.Fprintf(w, "# TYPE candledb_http_requests_total counter\n")
	fmt.Fprintf(w, "candledb_http_requests_total{class=\"2xx_3xx\"} %d\n", s.metrics.ok.Load())
	fmt.Fprintf(w, "candledb_http_requests_total{class=\"4xx\"} %d\n", s.metrics.clientErr.Load())
	fmt.Fprintf(w, "candledb_http_requests_total{class=\"5xx\"} %d\n", s.metrics.serverErr.Load())

	stats := s.docs.Stats()
	fmt.Fprintf(w, "# TYPE candledb_database_version gauge\ncandledb_database_version %d\n", stats.Version)
	fmt.Fprintf(w, "# TYPE candledb_listen_sessions gauge\ncandledb_listen_sessions %d\n", stats.Sessions)
	fmt.Fprintf(w, "# TYPE candledb_listen_targets gauge\ncandledb_listen_targets %d\n", stats.Targets)

	if st := s.storeStats; st != nil {
		fmt.Fprintf(w, "# TYPE candledb_store_operations_total counter\n")
		fmt.Fprintf(w, "# TYPE candledb_store_errors_total counter\n")
		fmt.Fprintf(w, "# TYPE candledb_store_seconds_total counter\n")
		for _, op := range []struct {
			name  string
			stats *instrumented.OpStats
		}{{"get", &st.Gets}, {"query", &st.Queries}, {"apply", &st.Applies}} {
			fmt.Fprintf(w, "candledb_store_operations_total{op=%q} %d\n", op.name, op.stats.Count.Load())
			fmt.Fprintf(w, "candledb_store_errors_total{op=%q} %d\n", op.name, op.stats.Errors.Load())
			fmt.Fprintf(w, "candledb_store_seconds_total{op=%q} %g\n", op.name, float64(op.stats.Nanos.Load())/1e9)
		}
	}
}
