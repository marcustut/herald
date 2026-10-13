package app

import "errors"

var (
	// ErrUnauthenticated collapses every credential failure — unknown, revoked,
	// or out of scope — into one error so a caller cannot probe for valid tokens.
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrInvalidInput is returned for a request that is well-formed but cannot
	// be acted on, such as issuing a scoped token with no projects.
	ErrInvalidInput = errors.New("invalid input")
	// ErrCredentialNotFound is an internal signal from a CredentialStore,
	// distinct from an infrastructure failure.
	ErrCredentialNotFound = errors.New("credential not found")
	// ErrDeliveryNotFound is returned by a DeliveryStore when updating a row
	// that no longer exists.
	ErrDeliveryNotFound = errors.New("delivery not found")
	// ErrIdempotencyConflict signals that Save collided with an existing report
	// for the same (tenant, idempotency key); the caller returns the original.
	ErrIdempotencyConflict = errors.New("idempotency conflict")
)
