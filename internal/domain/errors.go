package domain

import "errors"

// Sentinel errors are wrapped with context at the point of failure, so callers
// match them with errors.Is rather than comparing strings.
var (
	ErrInvalidEvent           = errors.New("invalid event")
	ErrInvalidStatus          = errors.New("invalid status")
	ErrInvalidLink            = errors.New("invalid link")
	ErrInvalidID              = errors.New("invalid id")
	ErrInvalidAgent           = errors.New("invalid agent")
	ErrInvalidContext         = errors.New("invalid context")
	ErrInvalidIdempotencyKey  = errors.New("invalid idempotency key")
	ErrInvalidOccurrenceID    = errors.New("invalid occurrence id")
	ErrInvalidToken           = errors.New("invalid token")
	ErrInvalidAccount         = errors.New("invalid account")
	ErrInvalidUser            = errors.New("invalid user")
	ErrInvalidEmail           = errors.New("invalid email")
	ErrInvalidDestination     = errors.New("invalid destination")
	ErrInvalidDestinationKind = errors.New("invalid destination kind")
	ErrInvalidTransition      = errors.New("invalid delivery transition")
	ErrTitleRequired          = errors.New("title is required")
	ErrTitleTooLong           = errors.New("title too long")
	ErrBodyTooLong            = errors.New("body too long")
	ErrTooManyLinks           = errors.New("too many links")
)
