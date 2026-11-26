package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/domain"
)

// writeTimeout is how long one event may take to reach the client before
// the connection is considered dead.
const writeTimeout = 30 * time.Second

// handleListen is GET /v1/listen: the realtime stream.
//
// The response is a Server-Sent Events stream, an ordinary HTTP response
// that never ends, made of "data: <json>\n\n" messages. The first message
// tells the client its session ID. The client then adds and removes
// listeners with separate requests naming that session, and their results
// arrive here. One stream therefore carries any number of listeners, which
// matters because browsers allow only a handful of connections per server.
func (s *Server) handleListen(w http.ResponseWriter, r *http.Request) {
	p := principal(r.Context())
	sess, err := s.docs.OpenSession(p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	defer sess.Close()

	// ResponseController reaches features of the underlying connection
	// (flushing, deadlines) through any wrapping middleware.
	rc := http.NewResponseController(w)
	send := func(ev eventJSON) error {
		payload, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		// A per-message deadline replaces the server-wide write timeout,
		// which would otherwise cut the stream off after a fixed time.
		if err := rc.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return err
		}
		return rc.Flush()
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no") // ask proxies such as nginx not to buffer
	w.WriteHeader(http.StatusOK)
	if err := send(eventJSON{Type: "ready", Session: sess.ID()}); err != nil {
		return
	}

	heartbeat := time.NewTicker(s.heartbeat)
	defer heartbeat.Stop()

	// A nil channel blocks forever in a select, so when the credentials do
	// not expire this case is simply never chosen.
	var expired <-chan time.Time
	if !p.Expires.IsZero() {
		timer := time.NewTimer(time.Until(p.Expires))
		defer timer.Stop()
		expired = timer.C
	}

	// select waits on several channels at once and runs the case of
	// whichever is ready first.
	for {
		select {
		case <-r.Context().Done(): // the client disconnected
			return
		case ev, open := <-sess.Events():
			if !open { // the session ended: server shutdown, or the client fell behind
				_ = send(eventJSON{Type: "error", Code: "session_closed", Message: "reconnect to resume"})
				return
			}
			if err := send(toEventJSON(ev)); err != nil {
				return
			}
		case <-heartbeat.C:
			// A comment line keeps idle connections from being closed by
			// proxies, and lets us notice a client that has gone away.
			if err := rc.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
				return
			}
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		case <-expired:
			_ = send(eventJSON{Type: "error", Code: "unauthenticated", Message: "access token has expired"})
			return
		}
	}
}

// session finds the listen session named in the URL and checks that it
// belongs to the caller. A session of someone else is reported exactly like
// a missing one.
func (s *Server) session(r *http.Request) (*app.Session, error) {
	sess, ok := s.docs.Session(r.PathValue("session"))
	p := principal(r.Context())
	if !ok || sess.Principal().UID != p.UID || sess.Principal().Admin != p.Admin {
		return nil, fmt.Errorf("%w: listen session", domain.ErrNotFound)
	}
	return sess, nil
}

func (s *Server) handleAddTarget(w http.ResponseWriter, r *http.Request) {
	sess, err := s.session(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req targetRequest
	if err := decodeBody(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	target, err := req.toDomain()
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := sess.AddTarget(r.Context(), req.ID, target); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoveTarget(w http.ResponseWriter, r *http.Request) {
	sess, err := s.session(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	id, err := strconv.Atoi(r.PathValue("target"))
	if err != nil {
		s.writeError(w, r, fmt.Errorf("%w: target ID must be a number", domain.ErrInvalidArgument))
		return
	}
	sess.RemoveTarget(id)
	w.WriteHeader(http.StatusNoContent)
}
