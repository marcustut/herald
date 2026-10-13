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

func (d Delivery) LeaseExpired(now time.Time) bool {
	return d.State == DeliveryInflight && !d.LeaseUntil.IsZero() && !now.Before(d.LeaseUntil)
}
