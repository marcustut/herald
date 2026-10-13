package app

import (
	"context"
	"errors"
	"time"

	"github.com/marcustut/herald/internal/domain"
)

// Clock is injected so use cases stay deterministic under test.
type Clock interface {
	Now() time.Time
}

// SecretGenerator mints high-entropy token secrets.
type SecretGenerator interface {
	NewSecret() string
}

// Hasher produces the at-rest representation of a credential. Implementations
// must be deterministic and one-way.
type Hasher interface {
	Hash(raw string) string
}

// IDGenerator mints identifiers for new aggregates.
type IDGenerator interface {
	NewEventID() domain.EventID
	NewDeliveryID() domain.DeliveryID
}

// Credential is a stored token. Only Hash is persisted; the raw token is shown
// once at issue time and is not recoverable.
//
// A project token has Kind TokenProject and exactly one entry in Projects; a
// scoped token has Kind TokenScoped and one or more.
type Credential struct {
	Hash       string
	TenantID   domain.TenantID
	Kind       domain.TokenKind
	Projects   []domain.ProjectID
	CreatedAt  time.Time
	RevokedAt  time.Time
	LastUsedAt time.Time
}

// Allows events whether the credential may act for project p. An empty project
// never matches, so a scoped token must always name one.
func (c Credential) Allows(p domain.ProjectID) bool {
	if p == "" {
		return false
	}
	for _, x := range c.Projects {
		if x == p {
			return true
		}
	}
	return false
}

func (c Credential) Revoked() bool { return !c.RevokedAt.IsZero() }

// CredentialStore persists and retrieves credentials.
type CredentialStore interface {
	Put(ctx context.Context, c Credential) error
	ByHash(ctx context.Context, hash string) (Credential, error)
	// ProjectCredential returns the project-scoped credential for project, used
	// to revoke it. It must not match a scoped token that merely covers project.
	ProjectCredential(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) (Credential, error)
}

// SinkDirectory resolves the sinks a project's events fan out to.
type SinkDirectory interface {
	EnabledSinks(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) ([]domain.SinkID, error)
}

// EventStore persists a event and its delivery rows atomically, and answers
// idempotency lookups. Save returns ErrIdempotencyConflict when a event for the
// same (tenant, idempotency key) already exists.
type EventStore interface {
	Save(ctx context.Context, event domain.Event, deliveries []domain.Delivery) error
	ByID(ctx context.Context, id domain.EventID) (domain.Event, bool, error)
	ByIdempotencyKey(ctx context.Context, tenant domain.TenantID, key domain.IdempotencyKey) (domain.Event, bool, error)
}

// DeliveryStore is the queue backend the dispatcher drains.
type DeliveryStore interface {
	// Claim atomically leases up to limit due deliveries, moving each to
	// inflight and counting an attempt.
	Claim(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]domain.Delivery, error)
	Update(ctx context.Context, d domain.Delivery) error
}

// Sink pushes a event to one external destination, rendering whatever payload
// that destination needs.
type Sink interface {
	ID() domain.SinkID
	Deliver(ctx context.Context, event domain.Event) error
}

// SinkError classifies a delivery failure. A permanent failure is not retried.
type SinkError struct {
	Err       error
	Permanent bool
}

func (e *SinkError) Error() string { return e.Err.Error() }
func (e *SinkError) Unwrap() error { return e.Err }

// Permanent wraps err as non-retryable.
func Permanent(err error) error { return &SinkError{Err: err, Permanent: true} }

// Retryable wraps err as retryable.
func Retryable(err error) error { return &SinkError{Err: err} }

// IsPermanent events whether err was classified permanent.
func IsPermanent(err error) bool {
	var se *SinkError
	return errors.As(err, &se) && se.Permanent
}
