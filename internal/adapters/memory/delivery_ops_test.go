package memory

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// seed stores events and their deliveries, preserving the delivery state.
func seed(t *testing.T, store *Events, ds ...domain.Delivery) {
	t.Helper()
	grouped := map[domain.EventID][]domain.Delivery{}
	var order []domain.EventID
	for _, d := range ds {
		if _, ok := grouped[d.EventID]; !ok {
			order = append(order, d.EventID)
		}
		grouped[d.EventID] = append(grouped[d.EventID], d)
	}
	for _, id := range order {
		event := domain.Event{ID: id, TenantID: "ten_1", IdempotencyKey: domain.IdempotencyKey("k-" + string(id))}
		if err := store.Save(context.Background(), event, grouped[id]); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInspectDeliveries(t *testing.T) {
	ctx := context.Background()
	store := NewEvents()
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

	a := domain.NewDelivery("del_a", "evt_1", "email", now)
	b := domain.NewDelivery("del_b", "evt_1", "webhook", now.Add(time.Minute))
	c := domain.NewDelivery("del_c", "evt_2", "email", now.Add(2*time.Minute))
	seed(t, store, a, b, c)

	got, err := store.ListDeliveries(ctx, app.DeliveryFilter{}, 0)
	if err != nil || len(got) != 3 {
		t.Fatalf("list all = %d, err %v", len(got), err)
	}
	// Newest first.
	if got[0].ID != "del_c" || got[2].ID != "del_a" {
		t.Fatalf("order = %s, %s, %s", got[0].ID, got[1].ID, got[2].ID)
	}

	byEvent, _ := store.ListDeliveries(ctx, app.DeliveryFilter{EventID: "evt_1"}, 0)
	if len(byEvent) != 2 {
		t.Fatalf("event filter = %d", len(byEvent))
	}
	bySink, _ := store.ListDeliveries(ctx, app.DeliveryFilter{SinkID: "email"}, 0)
	if len(bySink) != 2 {
		t.Fatalf("sink filter = %d", len(bySink))
	}
	limited, _ := store.ListDeliveries(ctx, app.DeliveryFilter{}, 1)
	if len(limited) != 1 || limited[0].ID != "del_c" {
		t.Fatalf("limit = %+v", limited)
	}

	d, ok, err := store.DeliveryByID(ctx, "del_b")
	if err != nil || !ok || d.SinkID != "webhook" {
		t.Fatalf("byID = %+v ok=%v err=%v", d, ok, err)
	}
	if _, ok, _ := store.DeliveryByID(ctx, "missing"); ok {
		t.Fatal("missing delivery resolved")
	}
}

// Requeue re-drives dead/pending rows and is a no-op for anything already
// delivered, so a redrive can never duplicate a successful sink delivery.
func TestRequeueIdempotent(t *testing.T) {
	ctx := context.Background()
	store := NewEvents()
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

	delivered := domain.NewDelivery("del_done", "evt_1", "email", now)
	delivered.State = domain.DeliveryDelivered
	dead := domain.NewDelivery("del_dead", "evt_2", "email", now)
	dead.State = domain.DeliveryDead
	dead.Attempts = 8
	dead.LastError = "500"
	seed(t, store, delivered, dead)

	// Already delivered: never touched.
	n, err := store.Requeue(ctx, app.DeliveryFilter{ID: "del_done"}, now.Add(time.Hour))
	if err != nil || n != 0 {
		t.Fatalf("delivered requeue: n=%d err=%v", n, err)
	}
	if d, _, _ := store.DeliveryByID(ctx, "del_done"); d.State != domain.DeliveryDelivered {
		t.Fatalf("delivered row changed: %+v", d)
	}

	// Dead: revived with a fresh budget.
	n, err = store.Requeue(ctx, app.DeliveryFilter{ID: "del_dead"}, now.Add(time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("dead requeue: n=%d err=%v", n, err)
	}
	got, _, _ := store.DeliveryByID(ctx, "del_dead")
	if got.State != domain.DeliveryPending || got.Attempts != 0 || !got.NextAttemptAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("revived row = %+v", got)
	}

	// A second redrive of the same delivered row is still a no-op.
	if n, _ := store.Requeue(ctx, app.DeliveryFilter{ID: "del_done"}, now); n != 0 {
		t.Fatalf("delivered row requeued again: n=%d", n)
	}
	if got := store.Deliveries(); len(got) != 2 {
		t.Fatalf("requeue duplicated rows: %d", len(got))
	}
}

// A transient failure returns to pending and is not a dead letter; a permanent
// failure lands in dead and is exactly what a redrive targets.
func TestRequeuePermanentVsTransient(t *testing.T) {
	ctx := context.Background()
	store := NewEvents()
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	b := domain.Backoff{Base: time.Second, Cap: time.Minute}

	transient := domain.NewDelivery("del_transient", "evt_1", "email", now)
	if err := transient.Claim(now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := transient.Fail(now, errors.New("503"), true, b, 8); err != nil {
		t.Fatal(err)
	}
	permanent := domain.NewDelivery("del_permanent", "evt_2", "email", now)
	if err := permanent.Claim(now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := permanent.Fail(now, errors.New("404"), false, b, 8); err != nil {
		t.Fatal(err)
	}
	seed(t, store, transient, permanent)

	if d, _, _ := store.DeliveryByID(ctx, "del_transient"); d.State != domain.DeliveryPending {
		t.Fatalf("transient state = %s", d.State)
	}
	if d, _, _ := store.DeliveryByID(ctx, "del_permanent"); d.State != domain.DeliveryDead {
		t.Fatalf("permanent state = %s", d.State)
	}

	// Requeuing only dead letters leaves the transient retry alone.
	n, err := store.Requeue(ctx, app.DeliveryFilter{State: domain.DeliveryDead}, now)
	if err != nil || n != 1 {
		t.Fatalf("bulk dead requeue: n=%d err=%v", n, err)
	}
	if d, _, _ := store.DeliveryByID(ctx, "del_permanent"); d.State != domain.DeliveryPending {
		t.Fatalf("permanent not revived: %+v", d)
	}
	if d, _, _ := store.DeliveryByID(ctx, "del_transient"); d.NextAttemptAt.IsZero() {
		t.Fatalf("transient retry schedule lost: %+v", d)
	}
}

// Concurrent redrives of the same dead letter must converge on one pending row.
func TestRequeueConcurrent(t *testing.T) {
	ctx := context.Background()
	store := NewEvents()
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	dead := domain.NewDelivery("del_dead", "evt_1", "email", now)
	dead.State = domain.DeliveryDead
	dead.Attempts = 8
	seed(t, store, dead)

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = store.Requeue(ctx, app.DeliveryFilter{ID: "del_dead"}, now)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if got := store.Deliveries(); len(got) != 1 || got[0].State != domain.DeliveryPending {
		t.Fatalf("deliveries = %+v", got)
	}
}

// An expired lease is reclaimed without consuming another attempt; a live lease
// is left to its worker. Claim performs the same reclaim before leasing.
func TestReclaimExpired(t *testing.T) {
	ctx := context.Background()
	store := NewEvents()
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

	stuck := domain.NewDelivery("del_stuck", "evt_1", "email", now)
	if err := stuck.Claim(now, time.Minute); err != nil {
		t.Fatal(err)
	}
	active := domain.NewDelivery("del_active", "evt_2", "email", now)
	if err := active.Claim(now, time.Hour); err != nil {
		t.Fatal(err)
	}
	seed(t, store, stuck, active)

	later := now.Add(2 * time.Minute)
	n, err := store.ReclaimExpired(ctx, later)
	if err != nil || n != 1 {
		t.Fatalf("reclaim n=%d err=%v", n, err)
	}
	got, _, _ := store.DeliveryByID(ctx, "del_stuck")
	if got.State != domain.DeliveryPending || got.Attempts != 1 || !got.NextAttemptAt.Equal(later) {
		t.Fatalf("reclaimed row = %+v", got)
	}
	if d, _, _ := store.DeliveryByID(ctx, "del_active"); d.State != domain.DeliveryInflight {
		t.Fatalf("live lease reclaimed: %+v", d)
	}

	// Claim recovers an expired lease on its own.
	claimed, err := store.Claim(ctx, later, time.Minute, 10)
	if err != nil || len(claimed) != 1 || claimed[0].ID != "del_stuck" {
		t.Fatalf("claim = %+v err %v", claimed, err)
	}
	if d, _, _ := store.DeliveryByID(ctx, "del_stuck"); d.Attempts != 2 {
		t.Fatalf("attempts after re-claim = %d, want 2", d.Attempts)
	}
}
