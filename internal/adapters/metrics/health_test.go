package metrics_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/adapters/metrics"
)

func TestReadyNoChecksIsReady(t *testing.T) {
	if err := (metrics.Health{}).Ready(context.Background()); err != nil {
		t.Fatalf("empty health = %v", err)
	}
}

func TestReadyDatabaseDown(t *testing.T) {
	h := metrics.Health{Ping: func(context.Context) error { return errors.New("connection refused") }}
	err := h.Ready(context.Background())
	if err == nil || !strings.Contains(err.Error(), "database not reachable") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadyDispatcherStale(t *testing.T) {
	now := time.Unix(1_760_000_000, 0)
	beat := &metrics.Heartbeat{}
	h := metrics.Health{Beat: beat, MaxAge: 10 * time.Second, Now: func() time.Time { return now }}

	if err := h.Ready(context.Background()); err == nil {
		t.Fatal("never-beaten dispatcher reported ready")
	}
	beat.Beat(now.Add(-time.Minute))
	if err := h.Ready(context.Background()); err == nil {
		t.Fatal("stale dispatcher reported ready")
	}
	beat.Beat(now)
	if err := h.Ready(context.Background()); err != nil {
		t.Fatalf("fresh dispatcher not ready: %v", err)
	}
}

func TestReadyRequiresBoth(t *testing.T) {
	now := time.Unix(1_760_000_000, 0)
	beat := &metrics.Heartbeat{}
	beat.Beat(now)
	h := metrics.Health{
		Ping:   func(context.Context) error { return errors.New("down") },
		Beat:   beat,
		MaxAge: time.Minute,
		Now:    func() time.Time { return now },
	}
	if err := h.Ready(context.Background()); err == nil {
		t.Fatal("database failure did not fail readiness")
	}
}
