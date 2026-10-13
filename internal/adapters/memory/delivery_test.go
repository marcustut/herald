package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

func TestDeliveryQueue(t *testing.T) {
	ctx := context.Background()
	store := NewEvents()
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	rep := domain.Event{ID: "evt_1", TenantID: "ten_1", IdempotencyKey: "k"}
	if err := store.Save(ctx, rep, []domain.Delivery{domain.NewDelivery("del_1", "evt_1", "email", now)}); err != nil {
		t.Fatal(err)
	}

	if got, ok, err := store.ByID(ctx, "evt_1"); err != nil || !ok || got.ID != "evt_1" {
		t.Fatalf("ByID = %+v, %v, %v", got, ok, err)
	}
	if _, ok, _ := store.ByID(ctx, "missing"); ok {
		t.Fatal("missing event resolved")
	}

	claimed, err := store.Claim(ctx, now, time.Minute, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	if d := claimed[0]; d.State != domain.DeliveryInflight || d.Attempts != 1 {
		t.Fatalf("claimed %+v", d)
	}
	if again, _ := store.Claim(ctx, now, time.Minute, 10); len(again) != 0 {
		t.Fatal("inflight delivery was claimed twice")
	}

	d := claimed[0]
	if err := d.Succeed(now); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	if got := store.Deliveries()[0].State; got != domain.DeliveryDelivered {
		t.Fatalf("state = %q", got)
	}

	if err := store.Update(ctx, domain.Delivery{ID: "missing"}); !errors.Is(err, app.ErrDeliveryNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestClaimRespectsDueTime(t *testing.T) {
	ctx := context.Background()
	store := NewEvents()
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	del := domain.NewDelivery("del_1", "evt_1", "email", now)
	del.NextAttemptAt = now.Add(time.Hour)
	if err := store.Save(ctx, domain.Event{ID: "evt_1", TenantID: "t", IdempotencyKey: "k"}, []domain.Delivery{del}); err != nil {
		t.Fatal(err)
	}
	if claimed, _ := store.Claim(ctx, now, time.Minute, 10); len(claimed) != 0 {
		t.Fatal("claimed a delivery that is not due")
	}
}
