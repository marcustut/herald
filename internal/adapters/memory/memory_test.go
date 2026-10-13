package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

func TestCredentials(t *testing.T) {
	ctx := context.Background()
	store := NewCredentials()
	if _, err := store.ByHash(ctx, "missing"); !errors.Is(err, app.ErrCredentialNotFound) {
		t.Fatalf("err = %v", err)
	}

	project := app.Credential{Hash: "h1", TenantID: "ten_1", Kind: domain.TokenProject, Projects: []domain.ProjectID{"axis"}}
	scoped := app.Credential{Hash: "h2", TenantID: "ten_1", Kind: domain.TokenScoped, Projects: []domain.ProjectID{"axis", "herald"}}
	for _, c := range []app.Credential{project, scoped} {
		if err := store.Put(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.ByHash(ctx, "h1")
	if err != nil || got.Hash != "h1" {
		t.Fatalf("got %+v, err %v", got, err)
	}

	// ProjectCredential returns the project token, never the scoped one.
	pc, err := store.ProjectCredential(ctx, "ten_1", "axis")
	if err != nil || pc.Hash != "h1" {
		t.Fatalf("project credential = %+v, err %v", pc, err)
	}
	if _, err := store.ProjectCredential(ctx, "ten_1", "herald"); !errors.Is(err, app.ErrCredentialNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestEvents(t *testing.T) {
	ctx := context.Background()
	store := NewEvents()

	rep := domain.Event{ID: "evt_1", TenantID: "ten_1", ProjectID: "axis", IdempotencyKey: "run-1"}
	delivery := domain.Delivery{ID: "del_1", EventID: "evt_1", SinkID: "email"}
	if err := store.Save(ctx, rep, []domain.Delivery{delivery}); err != nil {
		t.Fatal(err)
	}

	got, ok, err := store.ByIdempotencyKey(ctx, "ten_1", "axis", "run-1")
	if err != nil || !ok || got.ID != "evt_1" {
		t.Fatalf("got %+v, ok %v, err %v", got, ok, err)
	}
	if _, ok, _ := store.ByIdempotencyKey(ctx, "ten_1", "axis", "other"); ok {
		t.Fatal("unknown key should not resolve")
	}

	if err := store.Save(ctx, rep, nil); !errors.Is(err, app.ErrIdempotencyConflict) {
		t.Fatalf("err = %v", err)
	}

	// The same key in another project is a different event.
	other := domain.Event{ID: "evt_2", TenantID: "ten_1", ProjectID: "herald", IdempotencyKey: "run-1"}
	if err := store.Save(ctx, other, nil); err != nil {
		t.Fatalf("cross-project key should not conflict: %v", err)
	}
	if _, ok, _ := store.ByIdempotencyKey(ctx, "ten_1", "axis", "run-1"); !ok {
		t.Fatal("project-scoped lookup should still find the original")
	}

	if _, ok := store.Event("evt_1"); !ok {
		t.Fatal("event not stored")
	}
	if got := store.Deliveries(); len(got) != 1 || got[0].ID != "del_1" {
		t.Fatalf("deliveries = %+v", got)
	}
}

// Queue depth is scoped by (tenant, project): two tenants sharing a slug, and a
// second project, each count their own outstanding deliveries.
func TestQueueDepth(t *testing.T) {
	ctx := context.Background()
	store := NewEvents()
	for _, tc := range []struct {
		id, tenant, project string
		state               domain.DeliveryState
	}{
		{"evt_a", "ten_a", "axis", domain.DeliveryPending},
		{"evt_b", "ten_b", "axis", domain.DeliveryPending},
		{"evt_c", "ten_a", "finance", domain.DeliveryDelivered},
	} {
		event := domain.Event{ID: domain.EventID(tc.id), TenantID: domain.TenantID(tc.tenant), ProjectID: domain.ProjectID(tc.project), IdempotencyKey: "k"}
		d := domain.Delivery{ID: domain.DeliveryID("del_" + tc.id), EventID: event.ID, SinkID: "email", State: tc.state}
		if err := store.Save(ctx, event, []domain.Delivery{d}); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		tenant, project string
		want            int
	}{
		{"ten_a", "axis", 1},
		{"ten_b", "axis", 1},
		{"ten_a", "finance", 0},
	} {
		got, err := store.Outstanding(ctx, domain.TenantID(tc.tenant), domain.ProjectID(tc.project))
		if err != nil || got != tc.want {
			t.Fatalf("outstanding(%s,%s) = %d, err %v, want %d", tc.tenant, tc.project, got, err, tc.want)
		}
	}
	if got, err := store.Backlog(ctx); err != nil || got != 2 {
		t.Fatalf("backlog = %d, err %v, want 2", got, err)
	}
}
