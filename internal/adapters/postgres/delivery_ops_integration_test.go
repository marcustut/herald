package postgres

import (
	"testing"
	"time"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// TestDeliveryOpsIntegration exercises inspection, redrive and reclaim against a
// real database. It runs only when HERALD_TEST_DATABASE_URL is set.
func TestDeliveryOpsIntegration(t *testing.T) {
	pool, ctx := testPool(t)
	resetSchema(t, ctx, pool)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	events := NewEvents(pool)
	base := time.Now().UTC().Truncate(time.Microsecond)

	event := domain.Event{
		ID: "evt_1", TenantID: "ten_1", ProjectID: "axis", AgentID: "herdr",
		Status: domain.StatusSuccess, Title: "t", Body: "b",
		IdempotencyKey: "run-1", CreatedAt: base,
	}
	mk := func(id, sink string, state domain.DeliveryState, attempts int, created time.Time, lease time.Time) domain.Delivery {
		d := domain.NewDelivery(domain.DeliveryID(id), "evt_1", domain.SinkID(sink), created)
		d.State = state
		d.Attempts = attempts
		d.NextAttemptAt = created
		d.LeaseUntil = lease
		return d
	}
	deliveries := []domain.Delivery{
		mk("del_pending", "email", domain.DeliveryPending, 0, base.Add(time.Minute), time.Time{}),
		mk("del_expired", "email", domain.DeliveryInflight, 1, base.Add(2*time.Minute), base.Add(-time.Minute)),
		mk("del_active", "email", domain.DeliveryInflight, 1, base.Add(3*time.Minute), base.Add(time.Hour)),
		mk("del_delivered", "webhook", domain.DeliveryDelivered, 1, base.Add(4*time.Minute), time.Time{}),
		mk("del_dead", "webhook", domain.DeliveryDead, 8, base.Add(5*time.Minute), time.Time{}),
	}
	if err := events.Save(ctx, event, deliveries); err != nil {
		t.Fatal(err)
	}

	all, err := events.ListDeliveries(ctx, app.DeliveryFilter{}, 0)
	if err != nil || len(all) != 5 {
		t.Fatalf("list all = %d err=%v", len(all), err)
	}
	if all[0].ID != "del_dead" {
		t.Fatalf("newest first got %q", all[0].ID)
	}
	dead, err := events.ListDeliveries(ctx, app.DeliveryFilter{State: domain.DeliveryDead}, 0)
	if err != nil || len(dead) != 1 || dead[0].ID != "del_dead" {
		t.Fatalf("state filter = %+v err=%v", dead, err)
	}
	webhooks, err := events.ListDeliveries(ctx, app.DeliveryFilter{SinkID: "webhook"}, 0)
	if err != nil || len(webhooks) != 2 {
		t.Fatalf("sink filter = %d err=%v", len(webhooks), err)
	}
	limited, err := events.ListDeliveries(ctx, app.DeliveryFilter{}, 2)
	if err != nil || len(limited) != 2 {
		t.Fatalf("limit = %d err=%v", len(limited), err)
	}

	if d, ok, err := events.DeliveryByID(ctx, "del_dead"); err != nil || !ok || d.Attempts != 8 {
		t.Fatalf("byID = %+v ok=%v err=%v", d, ok, err)
	}
	if _, ok, _ := events.DeliveryByID(ctx, "missing"); ok {
		t.Fatal("missing delivery resolved")
	}

	// A delivered row is never revived.
	if n, err := events.Requeue(ctx, app.DeliveryFilter{ID: "del_delivered"}, base); err != nil || n != 0 {
		t.Fatalf("delivered requeue n=%d err=%v", n, err)
	}
	// A dead row is revived with a fresh budget.
	if n, err := events.Requeue(ctx, app.DeliveryFilter{ID: "del_dead"}, base); err != nil || n != 1 {
		t.Fatalf("dead requeue n=%d err=%v", n, err)
	}
	if d, _, _ := events.DeliveryByID(ctx, "del_dead"); d.State != domain.DeliveryPending || d.Attempts != 0 {
		t.Fatalf("revived = %+v", d)
	}
	// A lease that is still held belongs to a worker.
	if n, err := events.Requeue(ctx, app.DeliveryFilter{ID: "del_active"}, base); err != nil || n != 0 {
		t.Fatalf("active lease requeue n=%d err=%v", n, err)
	}

	// Only the expired lease is reclaimed, and it keeps its attempt count.
	n, err := events.ReclaimExpired(ctx, base)
	if err != nil || n != 1 {
		t.Fatalf("reclaim n=%d err=%v", n, err)
	}
	if d, _, _ := events.DeliveryByID(ctx, "del_expired"); d.State != domain.DeliveryPending || d.Attempts != 1 {
		t.Fatalf("reclaimed = %+v", d)
	}
	if d, _, _ := events.DeliveryByID(ctx, "del_active"); d.State != domain.DeliveryInflight {
		t.Fatalf("live lease reclaimed: %+v", d)
	}

	// Bulk redrive of dead letters is idempotent: the second pass changes nothing.
	if n, err := events.Requeue(ctx, app.DeliveryFilter{State: domain.DeliveryDead}, base); err != nil || n != 0 {
		t.Fatalf("second bulk requeue n=%d err=%v", n, err)
	}
}
