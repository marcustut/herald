package memory

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// stubQueue is a QueueGauge whose per-(tenant, project) depth the test sets.
type stubQueue struct {
	outstanding map[limitKey]int
	err         error
}

func (q stubQueue) Outstanding(_ context.Context, tenant domain.TenantID, project domain.ProjectID) (int, error) {
	if q.err != nil {
		return 0, q.err
	}
	return q.outstanding[limitKey{tenant: tenant, project: project}], nil
}

// stepClock is a mutable app.Clock for driving window resets in tests.
type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func newStepClock(t time.Time) *stepClock { return &stepClock{t: t} }

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *stepClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

var (
	tenantA = domain.TenantID("ten_a")
	tenantB = domain.TenantID("ten_b")
	projA   = domain.ProjectID("axis")
	projB   = domain.ProjectID("finance")
)

func allow(t *testing.T, l *RateLimiter, tenant domain.TenantID, project domain.ProjectID) app.Decision {
	t.Helper()
	d, err := l.Allow(context.Background(), tenant, project)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRateLimiterAllowsUpToLimit(t *testing.T) {
	start := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	clock := newStepClock(start)
	l := NewRateLimiter(map[domain.ProjectID]app.Limits{
		projA: {Requests: 2, Window: time.Minute, Daily: 100, Monthly: 100},
	}, clock, nil)

	for i, want := range []int{1, 0} {
		d := allow(t, l, tenantA, projA)
		if !d.Allowed || d.Limit != 2 || d.Remaining != want {
			t.Fatalf("call %d: %+v", i, d)
		}
	}

	d := allow(t, l, tenantA, projA)
	if d.Allowed || d.Remaining != 0 || d.Limit != 2 {
		t.Fatalf("third call should block: %+v", d)
	}
	if d.RetryAfter != time.Minute {
		t.Fatalf("RetryAfter = %v, want 1m", d.RetryAfter)
	}
	if !d.Reset.Equal(start.Add(time.Minute)) {
		t.Fatalf("Reset = %v", d.Reset)
	}
}

func TestRateLimiterWindowResets(t *testing.T) {
	start := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	clock := newStepClock(start)
	l := NewRateLimiter(map[domain.ProjectID]app.Limits{
		projA: {Requests: 1, Window: time.Minute, Daily: 100, Monthly: 100},
	}, clock, nil)

	if d := allow(t, l, tenantA, projA); !d.Allowed {
		t.Fatalf("first call blocked: %+v", d)
	}
	if d := allow(t, l, tenantA, projA); d.Allowed {
		t.Fatal("second call in same window allowed")
	}

	clock.set(start.Add(time.Minute))
	if d := allow(t, l, tenantA, projA); !d.Allowed {
		t.Fatalf("call after window reset blocked: %+v", d)
	}
}

func TestRateLimiterDailyQuotaResets(t *testing.T) {
	start := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	clock := newStepClock(start)
	l := NewRateLimiter(map[domain.ProjectID]app.Limits{
		projA: {Daily: 2},
	}, clock, nil)

	allow(t, l, tenantA, projA)
	allow(t, l, tenantA, projA)
	d := allow(t, l, tenantA, projA)
	if d.Allowed || d.Limit != 2 {
		t.Fatalf("daily quota not enforced: %+v", d)
	}
	wantReset := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	if !d.Reset.Equal(wantReset) {
		t.Fatalf("Reset = %v, want %v", d.Reset, wantReset)
	}
	if d.RetryAfter != 15*time.Hour {
		t.Fatalf("RetryAfter = %v, want 15h", d.RetryAfter)
	}

	clock.set(wantReset)
	if d := allow(t, l, tenantA, projA); !d.Allowed {
		t.Fatalf("quota did not reset next day: %+v", d)
	}
}

func TestRateLimiterMonthlyQuotaResets(t *testing.T) {
	start := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	clock := newStepClock(start)
	l := NewRateLimiter(map[domain.ProjectID]app.Limits{
		projA: {Monthly: 1},
	}, clock, nil)

	if d := allow(t, l, tenantA, projA); !d.Allowed {
		t.Fatalf("first call blocked: %+v", d)
	}
	d := allow(t, l, tenantA, projA)
	if d.Allowed {
		t.Fatal("monthly quota not enforced")
	}
	wantReset := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if !d.Reset.Equal(wantReset) {
		t.Fatalf("Reset = %v, want %v", d.Reset, wantReset)
	}

	clock.set(wantReset)
	if d := allow(t, l, tenantA, projA); !d.Allowed {
		t.Fatalf("quota did not reset next month: %+v", d)
	}
}

func TestRateLimiterPartitionsByTenantAndProject(t *testing.T) {
	clock := newStepClock(time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC))
	l := NewRateLimiter(map[domain.ProjectID]app.Limits{
		projA: {Requests: 1, Window: time.Hour, Daily: 100, Monthly: 100},
		projB: {Requests: 1, Window: time.Hour, Daily: 100, Monthly: 100},
	}, clock, nil)

	if d := allow(t, l, tenantA, projA); !d.Allowed {
		t.Fatalf("tenantA/axis blocked: %+v", d)
	}
	if d := allow(t, l, tenantA, projA); d.Allowed {
		t.Fatal("tenantA/axis exceeded budget")
	}
	// A different tenant and a different project each get their own budget.
	if d := allow(t, l, tenantB, projA); !d.Allowed {
		t.Fatalf("tenantB shared tenantA budget: %+v", d)
	}
	if d := allow(t, l, tenantA, projB); !d.Allowed {
		t.Fatalf("finance shared axis budget: %+v", d)
	}
}

func TestRateLimiterUnconfiguredProjectAllowed(t *testing.T) {
	clock := newStepClock(time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC))
	l := NewRateLimiter(map[domain.ProjectID]app.Limits{}, clock, nil)
	if d := allow(t, l, tenantA, projA); !d.Allowed || d.Limit != 0 {
		t.Fatalf("unconfigured project limited: %+v", d)
	}
}

func TestRateLimiterDisabledLimitsAllowed(t *testing.T) {
	clock := newStepClock(time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC))
	l := NewRateLimiter(map[domain.ProjectID]app.Limits{projA: {}}, clock, nil)
	for i := 0; i < 10; i++ {
		if d := allow(t, l, tenantA, projA); !d.Allowed {
			t.Fatalf("call %d blocked with no limits: %+v", i, d)
		}
	}
}

func TestRateLimiterConcurrent(t *testing.T) {
	clock := newStepClock(time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC))
	const limit = 100
	l := NewRateLimiter(map[domain.ProjectID]app.Limits{
		projA: {Requests: limit, Window: time.Hour, Daily: 1000, Monthly: 1000},
	}, clock, nil)

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 4*limit; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := l.Allow(context.Background(), tenantA, projA)
			if err != nil {
				t.Error(err)
				return
			}
			if d.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := allowed.Load(); got != limit {
		t.Fatalf("allowed = %d, want %d", got, limit)
	}
}

// A project at its Queued cap is rejected with the queue scope and a
// Retry-After, without consuming a request-window slot; another tenant sharing
// the same project slug is unaffected.
func TestRateLimiterQueueBackpressure(t *testing.T) {
	clock := newStepClock(time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC))
	q := stubQueue{outstanding: map[limitKey]int{}}
	l := NewRateLimiter(map[domain.ProjectID]app.Limits{
		projA: {Requests: 100, Window: time.Hour, Daily: 100, Monthly: 100, Queued: 2},
	}, clock, q)

	q.outstanding[limitKey{tenantA, projA}] = 1
	if d := allow(t, l, tenantA, projA); !d.Allowed {
		t.Fatalf("under queue cap blocked: %+v", d)
	}

	q.outstanding[limitKey{tenantA, projA}] = 2
	d := allow(t, l, tenantA, projA)
	if d.Allowed || d.Scope != app.ScopeQueue || d.Limit != 2 || d.RetryAfter <= 0 {
		t.Fatalf("queue cap not enforced: %+v", d)
	}
	// Tenant B's counter for the same slug is independent.
	if d := allow(t, l, tenantB, projA); !d.Allowed {
		t.Fatalf("tenant B throttled by tenant A backlog: %+v", d)
	}

	l.queue = stubQueue{err: errors.New("db down")}
	if _, err := l.Allow(context.Background(), tenantA, projA); err == nil {
		t.Fatal("queue gauge error was swallowed")
	}
}

// Counter rejections carry the scope of the limit that tripped.
func TestRateLimiterRejectionScopes(t *testing.T) {
	start := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	clock := newStepClock(start)
	l := NewRateLimiter(map[domain.ProjectID]app.Limits{
		projA: {Requests: 1, Window: time.Minute, Daily: 100, Monthly: 100},
	}, clock, nil)

	allow(t, l, tenantA, projA)
	if d := allow(t, l, tenantA, projA); d.Scope != app.ScopeRequests {
		t.Fatalf("scope = %q, want %q", d.Scope, app.ScopeRequests)
	}
}

func TestRateLimiterBoundedMemory(t *testing.T) {
	clock := newStepClock(time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC))
	l := NewRateLimiter(map[domain.ProjectID]app.Limits{
		projA: {Requests: 1, Window: time.Hour, Daily: 100, Monthly: 100},
	}, clock, nil)
	l.maxKeys = 1

	allow(t, l, tenantA, projA)
	allow(t, l, tenantB, projA)
	if got := len(l.entries); got != 1 {
		t.Fatalf("entries = %d, want 1 (bounded)", got)
	}
}
