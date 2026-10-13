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

// RateLimiter is an in-memory app.RateLimiter using fixed-window counters
// partitioned by (tenant, project). It is correct for a single instance only:
// counters are process-local, so a multi-instance deployment must supply a
// shared-state implementation instead (see docs/rate-limiting.md). Counters are
// reset lazily as their windows roll over and the oldest entry is evicted at
// capacity, keeping memory bounded.
type RateLimiter struct {
	mu      sync.Mutex
	clock   app.Clock
	limits  map[domain.ProjectID]app.Limits
	entries map[limitKey]*limitEntry
	maxKeys int
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

// NewRateLimiter builds a limiter for the given per-project budgets. A project
// with no entry (or with every limit disabled) is never limited.
func NewRateLimiter(limits map[domain.ProjectID]app.Limits, clock app.Clock) *RateLimiter {
	return &RateLimiter{
		clock:   clock,
		limits:  limits,
		entries: make(map[limitKey]*limitEntry),
		maxKeys: defaultMaxKeys,
	}
}

var _ app.RateLimiter = (*RateLimiter)(nil)

// Allow consumes one submission for (tenant, project) if a budget remains. It
// reports a Decision with the tightest limit so the caller can set Retry-After
// and the X-RateLimit-* headers.
func (l *RateLimiter) Allow(_ context.Context, tenant domain.TenantID, project domain.ProjectID) (app.Decision, error) {
	lim, ok := l.limits[project]
	if !ok || lim.Disabled() {
		return app.Decision{Allowed: true}, nil
	}
	now := l.clock.Now()

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
		return blocked(lim.Requests, e.windowStart.Add(lim.Window), now), nil
	}
	if lim.Daily > 0 && e.dayCount >= lim.Daily {
		return blocked(lim.Daily, e.dayStart.AddDate(0, 0, 1), now), nil
	}
	if lim.Monthly > 0 && e.monthCount >= lim.Monthly {
		return blocked(lim.Monthly, e.monthStart.AddDate(0, 1, 0), now), nil
	}

	e.windowCount++
	e.dayCount++
	e.monthCount++
	return remaining(lim, e), nil
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
