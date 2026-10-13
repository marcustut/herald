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
	NewDestinationID() domain.DestinationID
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
	// UserByEmail returns the identity for a registered address, reporting
	// false (no error) when no user has it. It provisions nothing, so the
	// signup gate can tell a new address from an existing one without creating
	// a row.
	UserByEmail(ctx context.Context, email string) (Identity, bool, error)
	// DeleteAccount removes the account and everything it owns; account, user,
	// tenant, project, destination and session rows all cascade away.
	DeleteAccount(ctx context.Context, account domain.AccountID) error
}

// DeliveryActivation is the beta funnel's per-account answer for one successful
// delivery. It carries no identifier, address or event content.
type DeliveryActivation struct {
	// Known is false for a tenant with no cloud account (a self-host tenant),
	// so the caller records nothing.
	Known bool
	// First is true when this delivery is the account's first distinct event to
	// reach a sink.
	First bool
	// AccountCreatedAt is when the owning account was provisioned.
	AccountCreatedAt time.Time
}

// ActivationStore answers the funnel's first-delivery question. It stores
// nothing: first-delivery and the delay since the account was created are
// derived from the delivered queue and the accounts table, so the funnel adds no
// new data collection.
type ActivationStore interface {
	Activation(ctx context.Context, tenant domain.TenantID) (DeliveryActivation, error)
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
	// ByTenant returns every credential issued to tenant, newest first. The
	// list carries only stored metadata; a raw token is never recoverable.
	ByTenant(ctx context.Context, tenant domain.TenantID) ([]Credential, error)
}

// SinkDirectory resolves the sinks a project's events fan out to.
type SinkDirectory interface {
	EnabledSinks(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) ([]domain.SinkID, error)
}

// SinkSource resolves the sink a delivery is pushed to. The tenant and project
// come from the stored event, never from the delivery row, so a source can
// scope the lookup to the owner. The only routing is a database destination
// (sink.DestinationSource), which loads the destination row on every call so
// removal, disablement or a revoked verification takes effect immediately.
type SinkSource interface {
	Sink(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, id domain.SinkID) (Sink, bool, error)
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
	// Projects returns every project owned by tenant, newest first.
	Projects(ctx context.Context, tenant domain.TenantID) ([]domain.Project, error)
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
	// MarkVerified records that ownership of the destination's target has been
	// proven. The owner is supplied by the caller (from a stored verification
	// token), never from a request body; a destination under a different owner
	// is not modified and reports ErrDestinationNotFound.
	MarkVerified(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, id domain.DestinationID, now time.Time) error
}

// DestinationVerificationRecord is a stored destination-verification token.
// Only the hash is persisted; the raw token exists only in the emailed link.
// The tenant and project are stored with the token so confirmation proves
// ownership from the stored row, never from the request.
type DestinationVerificationRecord struct {
	Hash          string
	DestinationID domain.DestinationID
	TenantID      domain.TenantID
	ProjectID     domain.ProjectID
	Email         string
	CreatedAt     time.Time
	ExpiresAt     time.Time
	UsedAt        time.Time
}

// DestinationVerificationStore persists single-use destination-verification
// tokens.
type DestinationVerificationStore interface {
	PutDestinationVerification(ctx context.Context, r DestinationVerificationRecord) error
	// ConsumeDestinationVerification atomically marks an unused, unexpired
	// token used and returns it. A consumed, expired or unknown token reports
	// ErrDestinationVerificationNotFound, so a replayed link cannot verify a
	// second time.
	ConsumeDestinationVerification(ctx context.Context, hash string, now time.Time) (DestinationVerificationRecord, error)
}

// EmailEventKind is the bounded kind of a provider notification about an
// outbound email.
type EmailEventKind string

const (
	EmailEventDelivery  EmailEventKind = "delivery"
	EmailEventBounce    EmailEventKind = "bounce"
	EmailEventComplaint EmailEventKind = "complaint"
)

// ProviderEvent is one notification about an outbound email: a delivery
// acceptance, a bounce or a complaint. Only the fields Herald acts on are
// carried; the raw provider payload and the recipient address are never logged
// or persisted beyond the suppression it may create.
type ProviderEvent struct {
	// ID is the provider's event id. Ingestion is idempotent by it, so a
	// replayed webhook is a no-op.
	ID      string
	Kind    EmailEventKind
	Address string
	// Permanent reports a hard failure (a permanent bounce). A transient bounce
	// (a full mailbox, greylisting) is recorded but never suppresses.
	Permanent  bool
	ReceivedAt time.Time
}

// Suppresses reports whether the event puts the address on the suppression
// list: a complaint always, a bounce only when permanent. It is the same rule
// the store applies, named once here.
func (e ProviderEvent) Suppresses() bool {
	return e.Kind == EmailEventComplaint || (e.Kind == EmailEventBounce && e.Permanent)
}

// Reason is the bounded suppression reason ("bounce" or "complaint").
func (e ProviderEvent) Reason() string {
	if e.Kind == EmailEventComplaint {
		return "complaint"
	}
	return "bounce"
}

// SuppressionStore records platform-wide recipient suppressions and answers the
// delivery path's checks. Suppression is keyed by normalized address and is
// global to the deployment: one hard bounce or complaint stops every further
// send to that mailbox, protecting the shared sending reputation the tenants
// rely on.
type SuppressionStore interface {
	// Suppressed reports whether a normalized address is on the suppression
	// list. The delivery path calls it live, so a suppression added after an
	// event was queued still takes effect.
	Suppressed(ctx context.Context, address string) (bool, error)
	// IngestProviderEvent records a provider event id and, when the event is a
	// permanent failure or complaint, suppresses its address, in one
	// transaction. It returns false when the id was already recorded, so a
	// replayed webhook cannot re-apply its effect.
	IngestProviderEvent(ctx context.Context, e ProviderEvent) (recorded bool, err error)
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

// EventFilter narrows a tenant-scoped event listing for the dashboard. Every
// zero field matches any value; Since is inclusive and Until is exclusive. The
// tenant is supplied separately (from the session), never by the filter, so a
// caller cannot widen scope by naming a project it does not own.
type EventFilter struct {
	Project domain.ProjectID
	Status  domain.Status
	Since   time.Time
	Until   time.Time
}

// DeliveryLogFilter narrows a tenant-scoped delivery listing for the dashboard.
// Every zero field matches any value; Since is inclusive and Until is exclusive.
// It is deliberately separate from DeliveryFilter (the operator requeue filter):
// this one is always joined to the owning event's tenant, so it can never select
// a row another tenant owns.
type DeliveryLogFilter struct {
	Project domain.ProjectID
	State   domain.DeliveryState
	EventID domain.EventID
	Since   time.Time
	Until   time.Time
}

// EventLog is the read-only query side of the event and delivery tables behind
// the cloud dashboard: a tenant lists its recent events and delivery attempts
// with filters and pagination. Ownership always comes from the tenant the
// caller passes (taken from the authenticated session), never from the request.
// Results are newest first; a limit <= 0 means no limit.
type EventLog interface {
	ListEvents(ctx context.Context, tenant domain.TenantID, f EventFilter, limit, offset int) ([]domain.Event, error)
	ListDeliveryAttempts(ctx context.Context, tenant domain.TenantID, f DeliveryLogFilter, limit, offset int) ([]domain.Delivery, error)
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

	// Outstanding returns how many deliveries for (tenant, project) are still
	// pending or inflight — the project's live backlog. It backs submit
	// backpressure and is scoped by tenant and project.
	Outstanding(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) (int, error)

	// Backlog returns the total pending and inflight deliveries across every
	// tenant, sampled by the dispatcher for queue-congestion metrics.
	Backlog(ctx context.Context) (int, error)
}

// Sink pushes a event to one external destination, rendering whatever payload
// that destination needs.
type Sink interface {
	ID() domain.SinkID
	Deliver(ctx context.Context, event domain.Event) error
}

// SinkError classifies a delivery failure. A permanent failure is not retried.
// A bounce is a permanent failure that a sink recognised as a hard rejection of
// the recipient (an SMTP 5xx, an invalid mailbox); it is dead-lettered at once
// and counted separately from other dead letters. A suppression is a permanent
// refusal of an address Herald already knows is undeliverable, so it too is
// dead-lettered at once and counted on its own.
type SinkError struct {
	Err        error
	Permanent  bool
	Bounce     bool
	Suppressed bool
}

func (e *SinkError) Error() string { return e.Err.Error() }
func (e *SinkError) Unwrap() error { return e.Err }

// Permanent wraps err as non-retryable.
func Permanent(err error) error { return &SinkError{Err: err, Permanent: true} }

// Bounce wraps err as a permanent, non-retryable recipient rejection.
func Bounce(err error) error { return &SinkError{Err: err, Permanent: true, Bounce: true} }

// Suppressed wraps err as a permanent, non-retryable refusal of an address on
// the suppression list (a prior hard bounce or complaint).
func Suppressed(err error) error { return &SinkError{Err: err, Permanent: true, Suppressed: true} }

// Retryable wraps err as retryable.
func Retryable(err error) error { return &SinkError{Err: err} }

// IsPermanent events whether err was classified permanent. A bounce is
// permanent, so this is true for a bounce too.
func IsPermanent(err error) bool {
	var se *SinkError
	return errors.As(err, &se) && se.Permanent
}

// IsBounce reports whether err was classified as a hard recipient rejection.
func IsBounce(err error) bool {
	var se *SinkError
	return errors.As(err, &se) && se.Bounce
}

// IsSuppressed reports whether err was a refusal of a suppressed address.
func IsSuppressed(err error) bool {
	var se *SinkError
	return errors.As(err, &se) && se.Suppressed
}
