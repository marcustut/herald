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
	NewAccountID() domain.AccountID
	NewUserID() domain.UserID
	NewTenantID() domain.TenantID
}

// MailMessage is one transactional email. The bodies are rendered by the use
// case; the mailer owns addressing, MIME envelope and headers.
type MailMessage struct {
	To      string
	Subject string
	Text    string
	HTML    string
}

// Mailer sends transactional mail. It is the single mail seam shared by the
// delivery sinks and the hosted sign-in flow, so there is no second SMTP path.
type Mailer interface {
	SendMail(ctx context.Context, m MailMessage) error
}

// LoginLimiter throttles unauthenticated sign-in requests by an opaque key
// (client IP or normalized email). Implementations must be safe for concurrent
// use; a nil limiter means no limit.
type LoginLimiter interface {
	Allow(ctx context.Context, key string) (Decision, error)
}

// Identity is the account-scoped actor a session acts for. It is resolved from
// stored rows, never from client input, so a caller cannot widen its scope.
type Identity struct {
	UserID    domain.UserID
	AccountID domain.AccountID
	Email     string
	TenantID  domain.TenantID
}

// NewIdentityIDs are the candidate identifiers for the account, user and
// default tenant created on first sign-in. The store uses them only when it
// creates the rows.
type NewIdentityIDs struct {
	Account domain.AccountID
	User    domain.UserID
	Tenant  domain.TenantID
}

// IdentityStore provisions and removes the account/user/tenant rows behind an
// identity. EnsureUser is idempotent by email: a returning address resolves to
// its existing identity, never a second one.
type IdentityStore interface {
	EnsureUser(ctx context.Context, email string, ids NewIdentityIDs, now time.Time) (Identity, error)
	// DeleteAccount removes the account and everything it owns; account, user,
	// tenant, project, destination and session rows all cascade away.
	DeleteAccount(ctx context.Context, account domain.AccountID) error
}

// SessionRecord is a stored browser session. Only the hashes of the cookie
// value and the CSRF token are persisted; the raw values are shown once at
// sign-in and never recoverable.
type SessionRecord struct {
	Hash      string
	UserID    domain.UserID
	AccountID domain.AccountID
	TenantID  domain.TenantID
	Email     string
	CSRFHash  string
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt time.Time
}

// SessionStore persists sessions and revokes them by token hash or account. A
// lookup of an unknown, expired or revoked session reports ErrSessionNotFound.
type SessionStore interface {
	PutSession(ctx context.Context, s SessionRecord) error
	SessionByHash(ctx context.Context, hash string, now time.Time) (SessionRecord, error)
	RevokeSession(ctx context.Context, hash string, now time.Time) error
	RevokeAccountSessions(ctx context.Context, account domain.AccountID, now time.Time) error
}

// LoginTokenRecord is a stored magic-link token. Only the hash is persisted;
// Email is the normalized address the link was mailed to.
type LoginTokenRecord struct {
	Hash      string
	Email     string
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    time.Time
}

// LoginTokenStore persists single-use magic-link tokens.
type LoginTokenStore interface {
	PutLoginToken(ctx context.Context, t LoginTokenRecord) error
	// ConsumeLoginToken atomically marks an unused, unexpired token used and
	// returns it. A consumed, expired or unknown token reports
	// ErrLoginTokenNotFound, so a replayed link can never mint a second session.
	ConsumeLoginToken(ctx context.Context, hash string, now time.Time) (LoginTokenRecord, error)
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

// ProjectStore persists the tenant-owned projects an account provisions. Every
// method is scoped by tenant: a project slug is unique per tenant, and a
// lookup that does not match the owning tenant must report not-found rather
// than return another tenant's row.
type ProjectStore interface {
	// PutProject upserts a project within its own tenant. It can never move an
	// existing project to another tenant.
	PutProject(ctx context.Context, p domain.Project) error
	// Project returns the project with id under tenant.
	Project(ctx context.Context, tenant domain.TenantID, id domain.ProjectID) (domain.Project, bool, error)
	// DeleteProject removes the project and its destinations. A project owned
	// by another tenant is not deleted and reports ErrProjectNotFound.
	DeleteProject(ctx context.Context, tenant domain.TenantID, id domain.ProjectID) error
}

// DestinationStore persists project-owned delivery destinations. Every method
// is scoped by (tenant, project); the ownership of a stored destination is
// never taken from client input, so a caller cannot read, re-point or delete
// another tenant's destination even when it knows the id.
type DestinationStore interface {
	// PutDestination upserts a destination within its own (tenant, project). An
	// id that already belongs to a different owner is not modified and reports
	// ErrDestinationNotFound.
	PutDestination(ctx context.Context, d domain.Destination) error
	// Destination returns the destination with id under (tenant, project).
	Destination(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, id domain.DestinationID) (domain.Destination, bool, error)
	// DestinationsByProject returns every destination owned by (tenant,
	// project), newest first.
	DestinationsByProject(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) ([]domain.Destination, error)
	// DeleteDestination removes the destination under (tenant, project).
	DeleteDestination(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, id domain.DestinationID) error
}

// EventStore persists a event and its delivery rows atomically, and answers
// idempotency lookups. Save returns ErrIdempotencyConflict when an event for the
// same (tenant, project, idempotency key) already exists; the project scope lets
// two projects reuse the same key without colliding.
type EventStore interface {
	Save(ctx context.Context, event domain.Event, deliveries []domain.Delivery) error
	ByID(ctx context.Context, id domain.EventID) (domain.Event, bool, error)
	ByIdempotencyKey(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, key domain.IdempotencyKey) (domain.Event, bool, error)
}

// DeliveryFilter narrows a delivery listing or a bulk requeue. Every zero field
// matches any value, so the zero filter matches every row.
type DeliveryFilter struct {
	ID      domain.DeliveryID
	State   domain.DeliveryState
	EventID domain.EventID
	SinkID  domain.SinkID
}

// DeliveryStore is the queue backend the dispatcher drains and the operator
// tooling inspects.
type DeliveryStore interface {
	// Claim recovers expired leases and then atomically leases up to limit due
	// deliveries, moving each to inflight and counting an attempt.
	Claim(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]domain.Delivery, error)
	Update(ctx context.Context, d domain.Delivery) error

	// DeliveryByID returns a single delivery row.
	DeliveryByID(ctx context.Context, id domain.DeliveryID) (domain.Delivery, bool, error)

	// ListDeliveries returns the deliveries matching filter, newest first. A
	// limit <= 0 returns every match.
	ListDeliveries(ctx context.Context, filter DeliveryFilter, limit int) ([]domain.Delivery, error)

	// Requeue re-drives the pending or dead deliveries matching filter so the
	// dispatcher will attempt them again, returning how many rows changed. A
	// delivered row is never selected, so a redrive cannot duplicate a sink
	// delivery that already succeeded; an inflight row whose lease is still held
	// is left to its worker.
	Requeue(ctx context.Context, filter DeliveryFilter, now time.Time) (int, error)

	// ReclaimExpired returns inflight rows whose lease expired to pending so a
	// crashed worker's deliveries are retried, returning how many rows changed.
	// It does not consume an attempt.
	ReclaimExpired(ctx context.Context, now time.Time) (int, error)
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
