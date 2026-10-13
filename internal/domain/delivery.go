package domain

import (
	"fmt"
	"time"
)

type DeliveryState string

const (
	DeliveryPending   DeliveryState = "pending"
	DeliveryInflight  DeliveryState = "inflight"
	DeliveryDelivered DeliveryState = "delivered"
	DeliveryDead      DeliveryState = "dead"
)

// Delivery is one row of the durable queue: the attempt to push a single
// Event to a single sink. Its state machine is the core scheduling logic and
// is enforced here rather than in SQL.
//
//	pending --Claim--> inflight --Succeed--> delivered
//	                       |
//	                       +--Fail(retryable, attempts<max)--> pending
//	                       +--Fail(permanent | attempts>=max)--> dead
//	                       +--Reclaim(lease expired)--> pending
type Delivery struct {
	ID            DeliveryID
	EventID       EventID
	SinkID        SinkID
	State         DeliveryState
	Attempts      int
	NextAttemptAt time.Time
	LeaseUntil    time.Time
	LastError     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func NewDelivery(id DeliveryID, eventID EventID, sinkID SinkID, now time.Time) Delivery {
	return Delivery{
		ID:            id,
		EventID:       eventID,
		SinkID:        sinkID,
		State:         DeliveryPending,
		Attempts:      0,
		NextAttemptAt: now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

// Claim moves a pending delivery to inflight, counting an attempt and holding a
// lease. A worker that crashes after claiming has its lease reclaimed later.
func (d *Delivery) Claim(now time.Time, lease time.Duration) error {
	if d.State != DeliveryPending {
		return fmt.Errorf("%w: claim from %s", ErrInvalidTransition, d.State)
	}
	d.State = DeliveryInflight
	d.Attempts++
	d.LeaseUntil = now.Add(lease)
	d.UpdatedAt = now
	return nil
}

func (d *Delivery) Succeed(now time.Time) error {
	if d.State != DeliveryInflight {
		return fmt.Errorf("%w: succeed from %s", ErrInvalidTransition, d.State)
	}
	d.State = DeliveryDelivered
	d.LastError = ""
	d.LeaseUntil = time.Time{}
	d.UpdatedAt = now
	return nil
}

// Fail records a failed attempt. Permanent failures and exhausted attempts go
// straight to dead; retryable ones return to pending with a backoff delay.
func (d *Delivery) Fail(now time.Time, cause error, retryable bool, backoff Backoff, maxAttempts int) error {
	if d.State != DeliveryInflight {
		return fmt.Errorf("%w: fail from %s", ErrInvalidTransition, d.State)
	}
	if cause != nil {
		d.LastError = cause.Error()
	}
	d.LeaseUntil = time.Time{}
	d.UpdatedAt = now
	if !retryable || d.Attempts >= maxAttempts {
		d.State = DeliveryDead
		return nil
	}
	d.State = DeliveryPending
	d.NextAttemptAt = now.Add(backoff.Delay(d.Attempts))
	return nil
}

// Reclaim returns an inflight delivery whose lease expired back to pending,
// without consuming a further attempt. Used to recover from crashed workers.
func (d *Delivery) Reclaim(now time.Time) error {
	if d.State != DeliveryInflight {
		return fmt.Errorf("%w: reclaim from %s", ErrInvalidTransition, d.State)
	}
	d.State = DeliveryPending
	d.NextAttemptAt = now
	d.LeaseUntil = time.Time{}
	d.UpdatedAt = now
	return nil
}

// Requeue re-drives a dead or pending delivery for another attempt: it makes
// the row due now and gives it a fresh attempt budget. It reports whether the
// row changed.
//
// It is deliberately a no-op for a delivered row, so an operator redrive can
// never duplicate a sink delivery that already succeeded, and for an inflight
// row whose lease is still held, which a worker currently owns. An inflight row
// whose lease has expired is revived like a dead one.
func (d *Delivery) Requeue(now time.Time) (bool, error) {
	switch d.State {
	case DeliveryDelivered:
		return false, nil
	case DeliveryInflight:
		if !d.LeaseExpired(now) {
			return false, nil
		}
	case DeliveryDead, DeliveryPending:
	default:
		return false, fmt.Errorf("%w: requeue from %s", ErrInvalidTransition, d.State)
	}
	d.State = DeliveryPending
	d.Attempts = 0
	d.NextAttemptAt = now
	d.LeaseUntil = time.Time{}
	d.LastError = ""
	d.UpdatedAt = now
	return true, nil
}

func (d Delivery) LeaseExpired(now time.Time) bool {
	return d.State == DeliveryInflight && !d.LeaseUntil.IsZero() && !now.Before(d.LeaseUntil)
}
