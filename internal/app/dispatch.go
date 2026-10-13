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
// event to its sink, and records success or schedules a retry.
type Dispatcher struct {
	Deliveries  DeliveryStore
	Events      EventStore
	Sinks       map[domain.SinkID]Sink
	Clock       Clock
	Lease       time.Duration
	MaxAttempts int
	Backoff     domain.Backoff
	Batch       int
	Logger      *slog.Logger
	// Observer records delivery attempts and poll progress. Nil falls back to
	// DefaultObserver.
	Observer Observer
	// Heartbeat is signalled after each successful poll so readiness can tell a
	// stalled dispatcher from a live one. Nil disables the signal.
	Heartbeat Heartbeat
}

func (d Dispatcher) observer() Observer {
	if d.Observer != nil {
		return d.Observer
	}
	return DefaultObserver
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
	event, ok, err := d.Events.ByID(ctx, del.EventID)
	if err != nil {
		return err
	}
	start := now
	var deliveryErr error
	if !ok {
		deliveryErr = Permanent(fmt.Errorf("event %q not found", del.EventID))
	} else if sink, ok := d.Sinks[del.SinkID]; !ok {
		deliveryErr = Permanent(fmt.Errorf("unknown sink %q", del.SinkID))
	} else {
		start = d.Clock.Now()
		deliveryErr = sink.Deliver(ctx, event)
	}
	if deliveryErr != nil && d.Logger != nil {
		d.Logger.Warn("delivery attempt failed",
			"sink", del.SinkID, "event", del.EventID, "attempts", del.Attempts, "error", deliveryErr)
	}
	err = d.finish(ctx, del, now, deliveryErr, deliveryErr == nil)
	d.observer().ObserveDelivery(del.SinkID, outcomeOf(del.State, deliveryErr), d.Clock.Now().Sub(start))
	return err
}

// outcomeOf maps the terminal/retry state a delivery landed in to the bounded
// outcome label. A dead delivery whose cause was a recipient bounce is counted
// as a bounce, so hard rejections are visible separately from other dead
// letters.
func outcomeOf(state domain.DeliveryState, cause error) DeliveryOutcome {
	switch state {
	case domain.DeliveryDelivered:
		return OutcomeDelivered
	case domain.DeliveryDead:
		if IsBounce(cause) {
			return OutcomeBounce
		}
		return OutcomeDead
	default:
		return OutcomeRetry
	}
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
		if _, err := d.ProcessBatch(ctx); err != nil {
			if ctx.Err() == nil && d.Logger != nil {
				d.Logger.Error("dispatch batch", "error", err)
			}
		} else {
			now := d.Clock.Now()
			d.observer().ObserveDispatchPoll(now)
			if depth, err := d.Deliveries.Backlog(ctx); err == nil {
				d.observer().ObserveQueueDepth(depth)
			}
			if d.Heartbeat != nil {
				d.Heartbeat.Beat(now)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
