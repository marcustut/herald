package domain

import (
	"errors"
	"testing"
	"time"
)

func TestRequeueFromPendingResetsBudget(t *testing.T) {
	d := NewDelivery("del_1", "evt_1", "sink_1", refTime)
	d.Attempts = 4
	d.LastError = "503"
	changed, err := d.Requeue(refTime.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("pending delivery should be requeued")
	}
	if d.State != DeliveryPending || d.Attempts != 0 || d.LastError != "" {
		t.Fatalf("got %+v", d)
	}
	if !d.NextAttemptAt.Equal(refTime.Add(time.Hour)) || !d.LeaseUntil.IsZero() {
		t.Fatalf("got %+v", d)
	}
}

func TestRequeueFromDeadResetsBudget(t *testing.T) {
	d := claimed(t, refTime)
	d.State = DeliveryDead
	d.Attempts = 8
	d.LastError = "permanent 404"
	changed, err := d.Requeue(refTime)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if d.State != DeliveryPending || d.Attempts != 0 {
		t.Fatalf("got %+v", d)
	}
}

// The core safety guarantee: a redrive never revives a delivery whose sink
// already accepted it.
func TestRequeueDeliveredIsNoop(t *testing.T) {
	d := claimed(t, refTime)
	if err := d.Succeed(refTime); err != nil {
		t.Fatal(err)
	}
	changed, err := d.Requeue(refTime)
	if err != nil {
		t.Fatal(err)
	}
	if changed || d.State != DeliveryDelivered {
		t.Fatalf("delivered row changed: changed=%v state=%s", changed, d.State)
	}
}

func TestRequeueInflightHeldLeaseIsNoop(t *testing.T) {
	d := claimed(t, refTime)
	changed, err := d.Requeue(refTime.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if changed || d.State != DeliveryInflight {
		t.Fatalf("leased row changed: changed=%v state=%s", changed, d.State)
	}
}

func TestRequeueInflightExpiredLeaseRevives(t *testing.T) {
	d := claimed(t, refTime)
	d.Attempts = 3
	changed, err := d.Requeue(refTime.Add(testLease))
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if d.State != DeliveryPending || d.Attempts != 0 || !d.LeaseUntil.IsZero() {
		t.Fatalf("got %+v", d)
	}
}

func TestRequeueUnknownState(t *testing.T) {
	d := NewDelivery("del_1", "evt_1", "sink_1", refTime)
	d.State = DeliveryState("bogus")
	if _, err := d.Requeue(refTime); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err = %v", err)
	}
}
