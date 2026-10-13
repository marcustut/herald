package domain

import (
	"errors"
	"testing"
	"time"
)

const testLease = 30 * time.Second

func claimed(t *testing.T, now time.Time) *Delivery {
	t.Helper()
	d := NewDelivery("del_1", "evt_1", "sink_1", now)
	if err := d.Claim(now, testLease); err != nil {
		t.Fatalf("claim: %v", err)
	}
	return &d
}

func TestNewDelivery(t *testing.T) {
	d := NewDelivery("del_1", "evt_1", "sink_1", refTime)
	if d.State != DeliveryPending || d.Attempts != 0 {
		t.Fatalf("got %+v", d)
	}
	if !d.NextAttemptAt.Equal(refTime) {
		t.Fatalf("next attempt = %v", d.NextAttemptAt)
	}
}

func TestClaim(t *testing.T) {
	d := NewDelivery("del_1", "evt_1", "sink_1", refTime)
	if err := d.Claim(refTime, testLease); err != nil {
		t.Fatal(err)
	}
	if d.State != DeliveryInflight {
		t.Fatalf("state = %q", d.State)
	}
	if d.Attempts != 1 {
		t.Fatalf("attempts = %d", d.Attempts)
	}
	if !d.LeaseUntil.Equal(refTime.Add(testLease)) {
		t.Fatalf("lease = %v", d.LeaseUntil)
	}
}

func TestClaimInvalidFromInflight(t *testing.T) {
	d := claimed(t, refTime)
	if err := d.Claim(refTime, testLease); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err = %v", err)
	}
}

func TestSucceed(t *testing.T) {
	d := claimed(t, refTime)
	if err := d.Succeed(refTime); err != nil {
		t.Fatal(err)
	}
	if d.State != DeliveryDelivered || !d.LeaseUntil.IsZero() {
		t.Fatalf("got %+v", d)
	}
}

func TestSucceedInvalidFromPending(t *testing.T) {
	d := NewDelivery("del_1", "evt_1", "sink_1", refTime)
	if err := d.Succeed(refTime); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err = %v", err)
	}
}

func TestFailRetryableReturnsToPending(t *testing.T) {
	b := Backoff{Base: time.Second, Cap: time.Minute}
	d := claimed(t, refTime)
	if err := d.Fail(refTime, errors.New("boom"), true, b, 3); err != nil {
		t.Fatal(err)
	}
	if d.State != DeliveryPending {
		t.Fatalf("state = %q", d.State)
	}
	if got, want := d.NextAttemptAt, refTime.Add(time.Second); !got.Equal(want) {
		t.Fatalf("next = %v, want %v", got, want)
	}
	if d.LastError != "boom" || !d.LeaseUntil.IsZero() {
		t.Fatalf("got %+v", d)
	}
}

func TestFailPermanentGoesDead(t *testing.T) {
	b := Backoff{Base: time.Second, Cap: time.Minute}
	d := claimed(t, refTime)
	if err := d.Fail(refTime, errors.New("404"), false, b, 3); err != nil {
		t.Fatal(err)
	}
	if d.State != DeliveryDead {
		t.Fatalf("state = %q", d.State)
	}
}

func TestFailExhaustsAttempts(t *testing.T) {
	b := Backoff{Base: time.Second, Cap: time.Minute}
	d := NewDelivery("del_1", "evt_1", "sink_1", refTime)
	now := refTime
	for i := 1; i <= 2; i++ {
		if err := d.Claim(now, testLease); err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if err := d.Fail(now, errors.New("5xx"), true, b, 3); err != nil {
			t.Fatalf("fail %d: %v", i, err)
		}
		if d.State != DeliveryPending {
			t.Fatalf("attempt %d: state = %q", i, d.State)
		}
	}
	if err := d.Claim(now, testLease); err != nil {
		t.Fatal(err)
	}
	if err := d.Fail(now, errors.New("5xx"), true, b, 3); err != nil {
		t.Fatal(err)
	}
	if d.State != DeliveryDead {
		t.Fatalf("state = %q, want dead after max attempts", d.State)
	}
}

func TestFailInvalidFromPending(t *testing.T) {
	d := NewDelivery("del_1", "evt_1", "sink_1", refTime)
	if err := d.Fail(refTime, nil, true, Backoff{}, 3); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err = %v", err)
	}
}

func TestReclaim(t *testing.T) {
	d := claimed(t, refTime)
	later := refTime.Add(time.Hour)
	if err := d.Reclaim(later); err != nil {
		t.Fatal(err)
	}
	if d.State != DeliveryPending || d.Attempts != 1 {
		t.Fatalf("got %+v", d)
	}
	if !d.NextAttemptAt.Equal(later) || !d.LeaseUntil.IsZero() {
		t.Fatalf("got %+v", d)
	}
}

func TestReclaimInvalidFromPending(t *testing.T) {
	d := NewDelivery("del_1", "evt_1", "sink_1", refTime)
	if err := d.Reclaim(refTime); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err = %v", err)
	}
}

func TestLeaseExpired(t *testing.T) {
	d := claimed(t, refTime)
	if d.LeaseExpired(refTime) {
		t.Fatal("lease should still be held at claim time")
	}
	if !d.LeaseExpired(refTime.Add(testLease)) {
		t.Fatal("lease should be expired at its deadline")
	}
	pending := NewDelivery("del_2", "evt_1", "sink_1", refTime)
	if pending.LeaseExpired(refTime.Add(time.Hour)) {
		t.Fatal("pending delivery has no lease")
	}
}
