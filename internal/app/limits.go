package app

import (
	"context"
	"time"

	"github.com/marcustut/herald/internal/domain"
)

// Limit scopes name which budget a Decision tripped. They are a bounded set
// (safe as a metric label) and select the HTTP error code; they are never a
// tenant or project id.
const (
	ScopeRequests = "requests"
	ScopeDaily    = "daily"
	ScopeMonthly  = "monthly"
	ScopeQueue    = "queue"
)

// Limits is the effective submit budget for one project. A zero field disables
// that limit. Requests is enforced over a fixed Window; Daily and Monthly are
// calendar quotas that reset at 00:00 UTC on the next day / first of the month.
// Queued caps outstanding deliveries rather than submissions (see below).
type Limits struct {
	Requests int
	Window   time.Duration
	Daily    int
	Monthly  int
	// Queued caps how many deliveries for the project may be outstanding
	// (pending or inflight) in the durable queue at once. It is backpressure
	// against a congested queue — a noisy tenant cannot grow an unbounded
	// backlog and starve other tenants of dispatcher capacity — not a rate. It
	// is only enforced when the limiter is wired to a QueueGauge.
	Queued int
}

// Disabled reports whether no limit is configured.
func (l Limits) Disabled() bool {
	return l.Requests <= 0 && l.Daily <= 0 && l.Monthly <= 0 && l.Queued <= 0
}

// Decision is the outcome of a rate-limit check. On a rejection it carries the
// values for Retry-After and the X-RateLimit-* headers.
type Decision struct {
	Allowed    bool
	Limit      int
	Remaining  int
	Reset      time.Time
	RetryAfter time.Duration
	// Scope names the limit that tripped (ScopeRequests, ScopeDaily,
	// ScopeMonthly or ScopeQueue). Empty when allowed by a limiter that does
	// not set a scope.
	Scope string
}

// QueueGauge reports how many deliveries a project currently has outstanding
// (pending or inflight) in the durable queue. The submit limiter uses it to
// apply Queued backpressure. Implementations must be safe for concurrent use
// and must partition by (tenant, project).
type QueueGauge interface {
	Outstanding(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) (int, error)
}

// RateLimiter decides whether a submission acting for a principal is currently
// allowed. Implementations partition their counters by (tenant, project) and
// must be safe for concurrent use. An empty result (Allowed true, Limit 0) means
// no limit applies.
type RateLimiter interface {
	Allow(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) (Decision, error)
}
