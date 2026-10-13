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
	Deliveries DeliveryStore
	Events     EventStore
	// Sinks resolves the sink for a delivery at delivery time. A hosted
	// deployment loads the destination from the database so a removed, disabled
	// or unverified destination stops receiving immediately; a self-host
	// deployment uses a static SinkMap built from gateway.toml.
	Sinks       SinkSource
	Clock       Clock
	Lease       time.Duration
	MaxAttempts int
	Backoff     domain.Backoff
	Batch       int
	Logger      *slog.Logger
	// Observer records delivery attempts and poll progress. Nil falls back to
	// DefaultObserver.
	Observer Observer
	// Activation derives the beta funnel's first-delivery and setup-time signal
	// from stored rows. Nil disables funnel sampling (a self-host deployment).
	Activation ActivationStore
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
	switch {
	case !ok:
		deliveryErr = Permanent(fmt.Errorf("event %q not found", del.EventID))
	case d.Sinks == nil:
		deliveryErr = Permanent(fmt.Errorf("unknown sink %q", del.SinkID))
	default:
		sink, found, err := d.Sinks.Sink(ctx, event.TenantID, event.ProjectID, del.SinkID)
		if err != nil {
			return err
		}
		if !found {
			deliveryErr = Permanent(fmt.Errorf("unknown sink %q", del.SinkID))
		} else {
			start = d.Clock.Now()
			deliveryErr = sink.Deliver(ctx, event)
		}
	}
	if deliveryErr != nil && d.Logger != nil {
		d.Logger.Warn("delivery attempt failed",
			"sink", del.SinkID, "event", del.EventID, "attempts", del.Attempts, "error", deliveryErr)
	}
	err = d.finish(ctx, del, now, deliveryErr, deliveryErr == nil)
	d.observer().ObserveDelivery(del.SinkID, outcomeOf(del.State, deliveryErr), d.Clock.Now().Sub(start))
	if err == nil && ok && del.State == domain.DeliveryDelivered {
		d.observeFunnel(ctx, event.TenantID)
	}
	return err
}

// observeFunnel records the first-delivery and repeat-send stages and the
// account-created -> first-delivered setup time. It derives the answer from
// stored rows and writes nothing. Errors are ignored: the funnel is best-effort
// telemetry and must never affect delivery.
//
// shortcut: with concurrent dispatchers two deliveries for one fresh account
// can both read "not first", under-counting first_delivered; exact once-only
// detection would need a persisted claim.
func (d Dispatcher) observeFunnel(ctx context.Context, tenant domain.TenantID) {
	if d.Activation == nil {
		return
	}
	a, err := d.Activation.Activation(ctx, tenant)
	if err != nil || !a.Known {
		return
	}
	if a.First {
		d.observer().ObserveFunnel(string(FunnelFirstDelivered))
		if !a.AccountCreatedAt.IsZero() {
			d.observer().ObserveActivation(d.Clock.Now().Sub(a.AccountCreatedAt))
		}
		return
	}
	d.observer().ObserveFunnel(string(FunnelRepeatSend))
}

// outcomeOf maps the terminal/retry state a delivery landed in to the bounded
// outcome label. A dead delivery whose cause was a recipient bounce is counted
// as a bounce, and one refused because the address is suppressed is counted as
// a suppression, so both are visible separately from other dead letters.
func outcomeOf(state domain.DeliveryState, cause error) DeliveryOutcome {
	switch state {
	case domain.DeliveryDelivered:
		return OutcomeDelivered
	case domain.DeliveryDead:
		if IsSuppressed(cause) {
			return OutcomeSuppressed
		}
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
