package memory

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// defaultMaxKeys caps how many (tenant, project) counters are tracked so an
// unexpected key cannot grow the map without bound. Configured projects are the
// only keys the gateway should ever see, so this is a safety valve.
const defaultMaxKeys = 4096

// queueRetryAfter is the Retry-After advertised when a project is throttled by
// its Queued backlog cap. There is no window to reset, so it is a fixed
// suggestion for a client to back off and let the dispatcher drain.
const queueRetryAfter = 30 * time.Second

// RateLimiter is an in-memory app.RateLimiter using fixed-window counters
// partitioned by (tenant, project). It is correct for a single instance only:
// counters are process-local, so a multi-instance deployment must supply a
// shared-state implementation instead (see docs/rate-limiting.md). Counters are
// reset lazily as their windows roll over and the oldest entry is evicted at
// capacity, keeping memory bounded.
//
// shortcut: every (tenant, project) shares the one global [limits] budget;
// per-project overrides move to the database later (#35). Until then this one
// budget is applied to every partition.
type RateLimiter struct {
	mu      sync.Mutex
	clock   app.Clock
	limit   app.Limits
	entries map[limitKey]*limitEntry
	maxKeys int
	// queue reports the durable backlog per (tenant, project) for the Queued
	// cap. Nil disables that cap.
	queue app.QueueGauge
}

type limitKey struct {
	tenant  domain.TenantID
	project domain.ProjectID
}

type limitEntry struct {
	windowStart time.Time
	windowCount int
	dayStart    time.Time
	dayCount    int
	monthStart  time.Time
	monthCount  int
}

// NewRateLimiter builds a limiter for the given global budget, applied to every
// (tenant, project). A disabled budget never limits. queue, when non-nil,
// supplies the durable backlog for the Queued cap; a nil queue leaves that cap
// disabled.
func NewRateLimiter(limit app.Limits, clock app.Clock, queue app.QueueGauge) *RateLimiter {
	return &RateLimiter{
		clock:   clock,
		limit:   limit,
		entries: make(map[limitKey]*limitEntry),
		maxKeys: defaultMaxKeys,
		queue:   queue,
	}
}

var _ app.RateLimiter = (*RateLimiter)(nil)

// Allow consumes one submission for (tenant, project) if a budget remains. It
// reports a Decision with the tightest limit so the caller can set Retry-After
// and the X-RateLimit-* headers.
func (l *RateLimiter) Allow(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) (app.Decision, error) {
	lim := l.limit
	if lim.Disabled() {
		return app.Decision{Allowed: true}, nil
	}
	now := l.clock.Now()

	// Backpressure is checked against the durable queue before any counter is
	// consumed and outside the mutex, so a slow count cannot serialise submits.
	if lim.Queued > 0 && l.queue != nil {
		outstanding, err := l.queue.Outstanding(ctx, tenant, project)
		if err != nil {
			return app.Decision{}, err
		}
		if outstanding >= lim.Queued {
			return app.Decision{
				Allowed:    false,
				Scope:      app.ScopeQueue,
				Limit:      lim.Queued,
				Remaining:  0,
				RetryAfter: queueRetryAfter,
			}, nil
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	e := l.entry(limitKey{tenant: tenant, project: project})
	if e.windowStart.IsZero() || now.Sub(e.windowStart) >= lim.Window {
		e.windowStart, e.windowCount = now, 0
	}
	if day := utcDay(now); !day.Equal(e.dayStart) {
		e.dayStart, e.dayCount = day, 0
	}
	if month := utcMonth(now); !month.Equal(e.monthStart) {
		e.monthStart, e.monthCount = month, 0
	}

	if lim.Requests > 0 && e.windowCount >= lim.Requests {
		return scoped(blocked(lim.Requests, e.windowStart.Add(lim.Window), now), app.ScopeRequests), nil
	}
	if lim.Daily > 0 && e.dayCount >= lim.Daily {
		return scoped(blocked(lim.Daily, e.dayStart.AddDate(0, 0, 1), now), app.ScopeDaily), nil
	}
	if lim.Monthly > 0 && e.monthCount >= lim.Monthly {
		return scoped(blocked(lim.Monthly, e.monthStart.AddDate(0, 1, 0), now), app.ScopeMonthly), nil
	}

	e.windowCount++
	e.dayCount++
	e.monthCount++
	return remaining(lim, e), nil
}

// scoped tags a rejection with the limit that tripped it.
func scoped(d app.Decision, scope string) app.Decision {
	d.Scope = scope
	return d
}

// entry returns the counter for k, creating it if needed. At capacity the entry
// whose window started longest ago is dropped first.
func (l *RateLimiter) entry(k limitKey) *limitEntry {
	if e, ok := l.entries[k]; ok {
		return e
	}
	if len(l.entries) >= l.maxKeys {
		l.evictOldest()
	}
	e := &limitEntry{}
	l.entries[k] = e
	return e
}

func (l *RateLimiter) evictOldest() {
	var oldest limitKey
	var oldestAt time.Time
	for k, e := range l.entries {
		if oldestAt.IsZero() || e.windowStart.Before(oldestAt) {
			oldest, oldestAt = k, e.windowStart
		}
	}
	delete(l.entries, oldest)
}

func blocked(limit int, reset, now time.Time) app.Decision {
	secs := int(math.Ceil(reset.Sub(now).Seconds()))
	if secs < 1 {
		secs = 1
	}
	return app.Decision{
		Allowed:    false,
		Limit:      limit,
		Remaining:  0,
		Reset:      reset,
		RetryAfter: time.Duration(secs) * time.Second,
	}
}

// remaining reports the allowed decision with the limit closest to exhaustion.
func remaining(lim app.Limits, e *limitEntry) app.Decision {
	d := app.Decision{Allowed: true, Remaining: math.MaxInt}
	consider := func(limit, count int, reset time.Time) {
		if limit <= 0 {
			return
		}
		if r := limit - count; r < d.Remaining {
			d.Limit, d.Remaining, d.Reset = limit, r, reset
		}
	}
	consider(lim.Requests, e.windowCount, e.windowStart.Add(lim.Window))
	consider(lim.Daily, e.dayCount, e.dayStart.AddDate(0, 0, 1))
	consider(lim.Monthly, e.monthCount, e.monthStart.AddDate(0, 1, 0))
	return d
}

func utcDay(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

func utcMonth(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
}
