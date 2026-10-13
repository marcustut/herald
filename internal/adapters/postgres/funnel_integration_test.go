package postgres

import (
	"testing"
	"time"
)

// The funnel is derived from existing rows: a first distinct delivered event
// activates the account, a second is a repeat send, and a second sink for the
// same event is not a repeat. A tenant with no account (self-host) is unknown.
func TestFunnelActivation(t *testing.T) {
	pool, ctx := testPool(t)
	resetSchema(t, ctx, pool)

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx,
		`INSERT INTO accounts (id, name, created_at) VALUES ('acct_1', 'Herald account', $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO tenants (id, account_id, name, created_at) VALUES ('ten_1', 'acct_1', 'default', $1)`, now); err != nil {
		t.Fatal(err)
	}

	f := NewFunnel(pool)

	a, err := f.Activation(ctx, "ten_1")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Known || a.First || !a.AccountCreatedAt.Equal(now) {
		t.Fatalf("activation = %+v, want known, not first, created %v", a, now)
	}

	insertEvent(t, ctx, pool, "evt_1", "axis", "k-1", now)
	if _, err := pool.Exec(ctx, `INSERT INTO deliveries
		(id, event_id, sink_id, state, attempts, next_attempt_at, created_at, updated_at)
		VALUES ('del_1', 'evt_1', 'email', 'delivered', 1, $1, $1, $1)`, now); err != nil {
		t.Fatal(err)
	}
	a, err = f.Activation(ctx, "ten_1")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Known || !a.First {
		t.Fatalf("activation = %+v, want first", a)
	}

	// A second sink for the same event is not a repeat send.
	if _, err := pool.Exec(ctx, `INSERT INTO deliveries
		(id, event_id, sink_id, state, attempts, next_attempt_at, created_at, updated_at)
		VALUES ('del_2', 'evt_1', 'webhook', 'delivered', 1, $1, $1, $1)`, now); err != nil {
		t.Fatal(err)
	}
	a, err = f.Activation(ctx, "ten_1")
	if err != nil {
		t.Fatal(err)
	}
	if !a.First {
		t.Fatalf("a second sink of one event counted as a repeat send: %+v", a)
	}

	// A second distinct delivered event is a repeat send.
	insertEvent(t, ctx, pool, "evt_2", "axis", "k-2", now)
	if _, err := pool.Exec(ctx, `INSERT INTO deliveries
		(id, event_id, sink_id, state, attempts, next_attempt_at, created_at, updated_at)
		VALUES ('del_3', 'evt_2', 'email', 'delivered', 1, $1, $1, $1)`, now); err != nil {
		t.Fatal(err)
	}
	a, err = f.Activation(ctx, "ten_1")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Known || a.First {
		t.Fatalf("activation = %+v, want repeat", a)
	}

	// A self-host tenant has no account row and is ignored.
	unknown, err := f.Activation(ctx, "ten_selfhost")
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Known {
		t.Fatalf("self-host tenant reported known: %+v", unknown)
	}
}
