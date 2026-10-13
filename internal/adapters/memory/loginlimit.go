package memory

import (
	"context"
	"sync"
	"time"

	"github.com/marcustut/herald/internal/app"
)

// LoginLimiter is an in-memory app.LoginLimiter: a fixed-window counter per
// opaque key (client IP or normalized email). It is process-local, so a
// multi-instance deployment needs a shared-state implementation, exactly like
// the submit RateLimiter.
type LoginLimiter struct {
	mu      sync.Mutex
	clock   app.Clock
	limit   int
	window  time.Duration
	entries map[string]*limitEntry
	maxKeys int
}

// NewLoginLimiter builds a limiter allowing limit requests per window per key.
// A non-positive limit disables limiting.
func NewLoginLimiter(limit int, window time.Duration, clock app.Clock) *LoginLimiter {
	return &LoginLimiter{
		clock:   clock,
		limit:   limit,
		window:  window,
		entries: make(map[string]*limitEntry),
		maxKeys: defaultMaxKeys,
	}
}

var _ app.LoginLimiter = (*LoginLimiter)(nil)

func (l *LoginLimiter) Allow(_ context.Context, key string) (app.Decision, error) {
	if l.limit <= 0 {
		return app.Decision{Allowed: true}, nil
	}
	now := l.clock.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	e, ok := l.entries[key]
	if !ok {
		if len(l.entries) >= l.maxKeys {
			l.evictOldest()
		}
		e = &limitEntry{}
		l.entries[key] = e
	}
	if e.windowStart.IsZero() || now.Sub(e.windowStart) >= l.window {
		e.windowStart, e.windowCount = now, 0
	}
	if e.windowCount >= l.limit {
		return blocked(l.limit, e.windowStart.Add(l.window), now), nil
	}
	e.windowCount++
	return app.Decision{
		Allowed:   true,
		Limit:     l.limit,
		Remaining: l.limit - e.windowCount,
		Reset:     e.windowStart.Add(l.window),
	}, nil
}

// evictOldest drops the key whose window started longest ago, keeping memory
// bounded when callers present unbounded keys.
func (l *LoginLimiter) evictOldest() {
	var oldest string
	var oldestAt time.Time
	for k, e := range l.entries {
		if oldestAt.IsZero() || e.windowStart.Before(oldestAt) {
			oldest, oldestAt = k, e.windowStart
		}
	}
	delete(l.entries, oldest)
}
