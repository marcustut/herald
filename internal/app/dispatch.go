package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/marcustut/herald/internal/domain"
)

// Dispatcher drains the delivery queue: it claims due deliveries, pushes each
// report to its sink, and records success or schedules a retry.
type Dispatcher struct {
	Deliveries  DeliveryStore
	Reports     ReportStore
	Sinks       map[domain.SinkID]Sink
	Clock       Clock
	Lease       time.Duration
	MaxAttempts int
	Backoff     domain.Backoff
	Batch       int
	Logger      *slog.Logger
}

// ProcessBatch claims and processes up to Batch deliveries, returning how many
// were handled.
func (d Dispatcher) ProcessBatch(ctx context.Context) (int, error) {
	claimed, err := d.Deliveries.Claim(ctx, d.Clock.Now(), d.Lease, d.Batch)
	if err != nil {
		return 0, err
	}
	for i := range claimed {
		if err := d.process(ctx, &claimed[i]); err != nil {
			return i, err
		}
	}
	return len(claimed), nil
}

func (d Dispatcher) process(ctx context.Context, del *domain.Delivery) error {
	now := d.Clock.Now()
	report, ok, err := d.Reports.ByID(ctx, del.ReportID)
	if err != nil {
		return err
	}
	if !ok {
		return d.finish(ctx, del, now, Permanent(fmt.Errorf("report %q not found", del.ReportID)), false)
	}
	sink, ok := d.Sinks[del.SinkID]
	if !ok {
		return d.finish(ctx, del, now, Permanent(fmt.Errorf("unknown sink %q", del.SinkID)), false)
	}
	deliveryErr := sink.Deliver(ctx, report)
	if deliveryErr != nil && d.Logger != nil {
		d.Logger.Warn("delivery attempt failed",
			"sink", del.SinkID, "report", del.ReportID, "attempts", del.Attempts, "error", deliveryErr)
	}
	return d.finish(ctx, del, now, deliveryErr, deliveryErr == nil)
}

// finish applies the terminal or retry transition and persists the row.
func (d Dispatcher) finish(ctx context.Context, del *domain.Delivery, now time.Time, cause error, succeeded bool) error {
	if succeeded {
		if err := del.Succeed(now); err != nil {
			return err
		}
	} else {
		retryable := !errors.Is(cause, context.Canceled) && !IsPermanent(cause)
		if err := del.Fail(now, cause, retryable, d.Backoff, d.MaxAttempts); err != nil {
			return err
		}
	}
	return d.Deliveries.Update(ctx, *del)
}

// Run polls the queue until ctx is cancelled.
func (d Dispatcher) Run(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := d.ProcessBatch(ctx); err != nil && ctx.Err() == nil && d.Logger != nil {
			d.Logger.Error("dispatch batch", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
