package metrics

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Heartbeat records when the dispatcher last completed a poll. The zero value
// is stale, so readiness is false until the dispatcher has made progress.
type Heartbeat struct {
	mu   sync.Mutex
	last time.Time
}

// Beat records one completed poll.
func (h *Heartbeat) Beat(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last = now
}

// Stale reports whether the dispatcher has never beaten or last beat more than
// maxAge before now.
func (h *Heartbeat) Stale(now time.Time, maxAge time.Duration) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last.IsZero() || now.Sub(h.last) > maxAge
}

var errStalled = errors.New("dispatcher has not made progress")

// Health answers readiness: ready only when the database answers a ping and the
// dispatcher has polled within MaxAge. Liveness stays a plain process check, so
// a stalled database does not restart the process. A nil field is not checked.
type Health struct {
	Ping   func(ctx context.Context) error
	Beat   *Heartbeat
	MaxAge time.Duration
	Now    func() time.Time
}

// Ready implements the readiness probe wired into the HTTP server.
func (h Health) Ready(ctx context.Context) error {
	if h.Ping != nil {
		if err := h.Ping(ctx); err != nil {
			return fmt.Errorf("database not reachable: %w", err)
		}
	}
	if h.Beat != nil {
		now := time.Now()
		if h.Now != nil {
			now = h.Now()
		}
		if h.Beat.Stale(now, h.MaxAge) {
			return errStalled
		}
	}
	return nil
}
