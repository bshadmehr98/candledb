package domain

import "errors"

// Sentinel errors. Code that fails wraps one of these with extra context
// (fmt.Errorf("%w: ...", ErrNotFound, ...)) and callers test for the category
// with errors.Is. The HTTP adapter maps each one to a status code.
var (
	ErrInvalidArgument    = errors.New("invalid argument")
	ErrNotFound           = errors.New("not found")
	ErrAlreadyExists      = errors.New("already exists")
	ErrFailedPrecondition = errors.New("failed precondition")
	ErrPermissionDenied   = errors.New("permission denied")
	ErrUnauthenticated    = errors.New("unauthenticated")
	ErrResourceExhausted  = errors.New("resource exhausted")
	ErrUnavailable        = errors.New("unavailable")
)
