package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/domain"
)

// errorBody is the JSON shape of every error response:
//
//	{"error": {"code": "not_found", "message": "not found: document users/x"}}
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// errorStatus maps the domain's error categories to HTTP. The order matters
// only in that the first matching entry wins.
var errorStatus = []struct {
	err    error
	status int
	code   string
}{
	{domain.ErrInvalidArgument, http.StatusBadRequest, "invalid_argument"},
	{domain.ErrUnauthenticated, http.StatusUnauthorized, "unauthenticated"},
	{domain.ErrPermissionDenied, http.StatusForbidden, "permission_denied"},
	{domain.ErrNotFound, http.StatusNotFound, "not_found"},
	{domain.ErrAlreadyExists, http.StatusConflict, "already_exists"},
	{domain.ErrFailedPrecondition, http.StatusPreconditionFailed, "failed_precondition"},
	{domain.ErrResourceExhausted, http.StatusTooManyRequests, "resource_exhausted"},
	{domain.ErrUnavailable, http.StatusServiceUnavailable, "unavailable"},
	{app.ErrSessionClosed, http.StatusGone, "session_closed"},
}

// classify finds the HTTP status and public code for an error. errors.Is
// looks through any number of wrapping layers, so it does not matter how
// much context was added on the way up.
func classify(err error) (status int, code string) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return http.StatusRequestEntityTooLarge, "request_too_large"
	}
	for _, e := range errorStatus {
		if errors.Is(err, e.err) {
			return e.status, e.code
		}
	}
	return http.StatusInternalServerError, "internal"
}

// writeError sends err to the client. Errors the client caused are shown as
// they are; anything else is logged and replaced by a generic message, so
// internal details such as SQL errors never leave the server.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, code := classify(err)
	message := err.Error()
	if status == http.StatusInternalServerError {
		s.logger.Error("request failed", "error", err, "method", r.Method, "path", r.URL.Path,
			"request_id", requestID(r.Context()))
		message = "internal error"
	}
	writeJSON(w, status, errorBody{errorDetail{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// The status line is already sent, so an encoding error here (in
	// practice: the client went away) has nowhere to be reported.
	_ = json.NewEncoder(w).Encode(body)
}
