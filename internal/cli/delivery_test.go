package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/adapters/memory"
	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// withDeliveryStore injects an in-memory store and a dummy DSN so the delivery
// command runs without a database.
func withDeliveryStore(t *testing.T, store app.DeliveryStore) {
	t.Helper()
	prev := openDeliveryStore
	openDeliveryStore = func(context.Context, string) (app.DeliveryStore, func(), error) {
		return store, func() {}, nil
	}
	t.Cleanup(func() { openDeliveryStore = prev })
	t.Setenv("HERALD_DATABASE_URL", "test-dsn")
}

func seedDeliveryStore(t *testing.T) *memory.Events {
	t.Helper()
	store := memory.NewEvents()
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

	dead := domain.NewDelivery("del_dead", "evt_1", "email", now)
	dead.State = domain.DeliveryDead
	dead.Attempts = 8
	dead.LastError = "smtp 500"
	done := domain.NewDelivery("del_done", "evt_2", "webhook", now.Add(time.Minute))
	done.State = domain.DeliveryDelivered
	stuck := domain.NewDelivery("del_stuck", "evt_3", "email", now.Add(2*time.Minute))
	stuck.State = domain.DeliveryInflight
	stuck.Attempts = 1
	stuck.LeaseUntil = time.Now().Add(-time.Hour)

	for _, d := range []domain.Delivery{dead, done, stuck} {
		ev := domain.Event{ID: d.EventID, TenantID: "ten_1", IdempotencyKey: domain.IdempotencyKey("k-" + string(d.EventID))}
		if err := store.Save(context.Background(), ev, []domain.Delivery{d}); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func TestDeliveryList(t *testing.T) {
	withDeliveryStore(t, seedDeliveryStore(t))

	code, out, errOut := run(t, "", "delivery", "list")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	for _, want := range []string{"del_dead", "dead", "del_done", "delivered"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output missing %q:\n%s", want, out)
		}
	}

	code, out, _ = run(t, "", "delivery", "list", "--state", "dead")
	if code != 0 || !strings.Contains(out, "del_dead") || strings.Contains(out, "del_done") {
		t.Fatalf("state filter code=%d out=%q", code, out)
	}
	code, out, _ = run(t, "", "delivery", "list", "--sink", "webhook")
	if code != 0 || !strings.Contains(out, "del_done") || strings.Contains(out, "del_dead") {
		t.Fatalf("sink filter code=%d out=%q", code, out)
	}
	code, out, _ = run(t, "", "delivery", "list", "--event", "evt_1")
	if code != 0 || !strings.Contains(out, "del_dead") || strings.Contains(out, "del_done") {
		t.Fatalf("event filter code=%d out=%q", code, out)
	}
}

func TestDeliveryShow(t *testing.T) {
	withDeliveryStore(t, seedDeliveryStore(t))

	code, out, errOut := run(t, "", "delivery", "show", "del_dead")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	for _, want := range []string{"del_dead", "evt_1", "email", "dead", "smtp 500"} {
		if !strings.Contains(out, want) {
			t.Fatalf("show output missing %q:\n%s", want, out)
		}
	}

	if code, _, errOut := run(t, "", "delivery", "show", "missing"); code != 1 || !strings.Contains(errOut, "not found") {
		t.Fatalf("missing show code=%d err=%q", code, errOut)
	}
	if code, _, _ := run(t, "", "delivery", "show"); code != 2 {
		t.Fatal("show without id should be a usage error")
	}
}

func TestDeliveryRetry(t *testing.T) {
	store := seedDeliveryStore(t)
	withDeliveryStore(t, store)

	code, out, errOut := run(t, "", "delivery", "retry", "del_dead")
	if code != 0 || !strings.Contains(out, "requeued") {
		t.Fatalf("retry code=%d out=%q err=%q", code, out, errOut)
	}
	if d, _, _ := store.DeliveryByID(context.Background(), "del_dead"); d.State != domain.DeliveryPending || d.Attempts != 0 {
		t.Fatalf("retry did not revive: %+v", d)
	}

	// A delivered row is never re-sent.
	code, out, _ = run(t, "", "delivery", "retry", "del_done")
	if code != 0 || !strings.Contains(out, "delivered") || !strings.Contains(out, "not requeued") {
		t.Fatalf("delivered retry code=%d out=%q", code, out)
	}
	if d, _, _ := store.DeliveryByID(context.Background(), "del_done"); d.State != domain.DeliveryDelivered {
		t.Fatalf("delivered row changed: %+v", d)
	}

	if code, _, errOut := run(t, "", "delivery", "retry", "missing"); code != 1 || !strings.Contains(errOut, "not found") {
		t.Fatalf("missing retry code=%d err=%q", code, errOut)
	}
	if code, _, _ := run(t, "", "delivery", "retry"); code != 2 {
		t.Fatal("retry without id should be a usage error")
	}
}

func TestDeliveryRequeue(t *testing.T) {
	store := seedDeliveryStore(t)
	withDeliveryStore(t, store)

	code, out, errOut := run(t, "", "delivery", "requeue", "--state", "dead")
	if code != 0 || !strings.Contains(out, "requeued 1") {
		t.Fatalf("requeue code=%d out=%q err=%q", code, out, errOut)
	}

	// Idempotent: the dead row is now pending, so nothing is left to redrive.
	code, out, _ = run(t, "", "delivery", "requeue", "--state", "dead")
	if code != 0 || !strings.Contains(out, "requeued 0") {
		t.Fatalf("second requeue code=%d out=%q", code, out)
	}

	if code, _, _ := run(t, "", "delivery", "requeue", "--state", "delivered"); code != 2 {
		t.Fatal("requeue delivered should be rejected")
	}
}

func TestDeliveryReclaim(t *testing.T) {
	store := seedDeliveryStore(t)
	withDeliveryStore(t, store)

	code, out, errOut := run(t, "", "delivery", "reclaim")
	if code != 0 || !strings.Contains(out, "reclaimed 1") {
		t.Fatalf("reclaim code=%d out=%q err=%q", code, out, errOut)
	}
	if d, _, _ := store.DeliveryByID(context.Background(), "del_stuck"); d.State != domain.DeliveryPending || d.Attempts != 1 {
		t.Fatalf("reclaimed row = %+v", d)
	}
}

func TestDeliveryUsage(t *testing.T) {
	t.Setenv("HERALD_DATABASE_URL", "")
	if code, _, _ := run(t, "", "delivery"); code != 2 {
		t.Fatal("delivery without subcommand should be a usage error")
	}
	if code, _, _ := run(t, "", "delivery", "bogus"); code != 2 {
		t.Fatal("unknown subcommand should be a usage error")
	}
	if code, _, _ := run(t, "", "delivery", "list", "--state", "nope"); code != 2 {
		t.Fatal("unknown state should be a usage error")
	}
	if code, _, _ := run(t, "", "delivery", "list"); code != 1 {
		t.Fatal("list without a database url should fail")
	}
}
