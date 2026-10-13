package domain

import "errors"

// Sentinel errors are wrapped with context at the point of failure, so callers
// match them with errors.Is rather than comparing strings.
var (
	ErrInvalidReport         = errors.New("invalid report")
	ErrInvalidStatus         = errors.New("invalid status")
	ErrInvalidLink           = errors.New("invalid link")
	ErrInvalidID             = errors.New("invalid id")
	ErrInvalidAgent          = errors.New("invalid agent")
	ErrInvalidContext        = errors.New("invalid context")
	ErrInvalidIdempotencyKey = errors.New("invalid idempotency key")
	ErrInvalidToken          = errors.New("invalid token")
	ErrInvalidTransition     = errors.New("invalid delivery transition")
	ErrTitleRequired         = errors.New("title is required")
	ErrTitleTooLong          = errors.New("title too long")
	ErrBodyTooLong           = errors.New("body too long")
	ErrTooManyLinks          = errors.New("too many links")
)
