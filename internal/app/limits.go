package app

import (
	"context"
	"time"

	"github.com/marcustut/herald/internal/domain"
)

// Limits is the effective submit budget for one project. A zero field disables
// that limit. Requests is enforced over a fixed Window; Daily and Monthly are
// calendar quotas that reset at 00:00 UTC on the next day / first of the month.
type Limits struct {
	Requests int
	Window   time.Duration
	Daily    int
	Monthly  int
}

// Disabled reports whether no limit is configured.
func (l Limits) Disabled() bool {
	return l.Requests <= 0 && l.Daily <= 0 && l.Monthly <= 0
}

// Decision is the outcome of a rate-limit check. On a rejection it carries the
// values for Retry-After and the X-RateLimit-* headers.
type Decision struct {
	Allowed    bool
	Limit      int
	Remaining  int
	Reset      time.Time
	RetryAfter time.Duration
}

// RateLimiter decides whether a submission acting for a principal is currently
// allowed. Implementations partition their counters by (tenant, project) and
// must be safe for concurrent use. An empty result (Allowed true, Limit 0) means
// no limit applies.
type RateLimiter interface {
	Allow(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) (Decision, error)
}
