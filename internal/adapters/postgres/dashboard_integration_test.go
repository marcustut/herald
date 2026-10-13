package postgres

import (
	"testing"
	"time"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// TestEventLogIntegration covers the dashboard's tenant-scoped read model
// against a real database. It runs only when HERALD_TEST_DATABASE_URL is set.
func TestEventLogIntegration(t *testing.T) {
	pool, ctx := testPool(t)
	resetSchema(t, ctx, pool)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	events := NewEvents(pool)
	base := time.Now().UTC().Truncate(time.Microsecond)

	type seed struct {
		id      domain.EventID
		tenant  domain.TenantID
		project domain.ProjectID
		status  domain.Status
		created time.Time
	}
	seeds := []seed{
		{"evt_a1", "ten_a", "alpha", domain.StatusSuccess, base.Add(3 * time.Hour)},
		{"evt_a2", "ten_a", "alpha", domain.StatusFailure, base.Add(2 * time.Hour)},
		{"evt_a3", "ten_a", "beta", domain.StatusSuccess, base.Add(time.Hour)},
		{"evt_b1", "ten_b", "alpha", domain.StatusSuccess, base},
	}
	for _, s := range seeds {
		rep := domain.Event{
			ID: s.id, TenantID: s.tenant, ProjectID: s.project, AgentID: "herdr",
			Status: s.status, Title: "t", Body: "b",
			Links: []domain.Link{}, Context: domain.Context{},
			IdempotencyKey: domain.IdempotencyKey(string(s.id)),
			CreatedAt:      s.created,
		}
		d := domain.NewDelivery(domain.DeliveryID("del_"+string(s.id)), s.id, "email", s.created)
		if err := events.Save(ctx, rep, []domain.Delivery{d}); err != nil {
			t.Fatalf("save %s: %v", s.id, err)
		}
	}

	ids := func(evs []domain.Event) []domain.EventID {
		out := make([]domain.EventID, len(evs))
		for i, e := range evs {
			out[i] = e.ID
		}
		return out
	}
	eq := func(got, want []domain.EventID) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	// Tenant isolation and newest-first ordering.
	got, err := events.ListEvents(ctx, "ten_a", app.EventFilter{}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []domain.EventID{"evt_a1", "evt_a2", "evt_a3"}; !eq(ids(got), want) {
		t.Fatalf("ten_a events = %v, want %v", ids(got), want)
	}
	got, err = events.ListEvents(ctx, "ten_b", app.EventFilter{}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []domain.EventID{"evt_b1"}; !eq(ids(got), want) {
		t.Fatalf("ten_b events = %v, want %v", ids(got), want)
	}

	// Filters narrow within the tenant only.
	got, _ = events.ListEvents(ctx, "ten_a", app.EventFilter{Project: "beta"}, 0, 0)
	if want := []domain.EventID{"evt_a3"}; !eq(ids(got), want) {
		t.Fatalf("project filter = %v, want %v", ids(got), want)
	}
	got, _ = events.ListEvents(ctx, "ten_a", app.EventFilter{Status: domain.StatusFailure}, 0, 0)
	if want := []domain.EventID{"evt_a2"}; !eq(ids(got), want) {
		t.Fatalf("status filter = %v, want %v", ids(got), want)
	}
	got, _ = events.ListEvents(ctx, "ten_a", app.EventFilter{
		Since: base.Add(90 * time.Minute), Until: base.Add(150 * time.Minute),
	}, 0, 0)
	if want := []domain.EventID{"evt_a2"}; !eq(ids(got), want) {
		t.Fatalf("date filter = %v, want %v", ids(got), want)
	}

	// Pagination.
	got, _ = events.ListEvents(ctx, "ten_a", app.EventFilter{}, 2, 1)
	if want := []domain.EventID{"evt_a2", "evt_a3"}; !eq(ids(got), want) {
		t.Fatalf("page = %v, want %v", ids(got), want)
	}

	// A foreign tenant cannot see rows by naming the same project.
	got, _ = events.ListEvents(ctx, "ten_b", app.EventFilter{Project: "alpha"}, 0, 0)
	if want := []domain.EventID{"evt_b1"}; !eq(ids(got), want) {
		t.Fatalf("cross-tenant project filter = %v, want %v", ids(got), want)
	}
}

// TestDeliveryLogIntegration covers the tenant-scoped delivery listing, which
// joins the owning event to derive ownership.
func TestDeliveryLogIntegration(t *testing.T) {
	pool, ctx := testPool(t)
	resetSchema(t, ctx, pool)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	events := NewEvents(pool)
	base := time.Now().UTC().Truncate(time.Microsecond)

	seedEvent := func(id domain.EventID, tenant domain.TenantID, project domain.ProjectID, created time.Time) {
		rep := domain.Event{
			ID: id, TenantID: tenant, ProjectID: project, AgentID: "herdr",
			Status: domain.StatusSuccess, Title: "t", Body: "b",
			Links: []domain.Link{}, Context: domain.Context{},
			IdempotencyKey: domain.IdempotencyKey(string(id)), CreatedAt: created,
		}
		d := domain.NewDelivery(domain.DeliveryID("del_"+string(id)), id, "email", created)
		if err := events.Save(ctx, rep, []domain.Delivery{d}); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}
	seedEvent("evt_a1", "ten_a", "alpha", base.Add(2*time.Hour))
	seedEvent("evt_a2", "ten_a", "beta", base.Add(time.Hour))
	seedEvent("evt_b1", "ten_b", "alpha", base)

	ids := func(ds []domain.Delivery) []domain.DeliveryID {
		out := make([]domain.DeliveryID, len(ds))
		for i, d := range ds {
			out[i] = d.ID
		}
		return out
	}
	eq := func(got, want []domain.DeliveryID) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	got, err := events.ListDeliveryAttempts(ctx, "ten_a", app.DeliveryLogFilter{}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []domain.DeliveryID{"del_evt_a1", "del_evt_a2"}; !eq(ids(got), want) {
		t.Fatalf("ten_a deliveries = %v, want %v", ids(got), want)
	}

	got, _ = events.ListDeliveryAttempts(ctx, "ten_a", app.DeliveryLogFilter{Project: "beta"}, 0, 0)
	if want := []domain.DeliveryID{"del_evt_a2"}; !eq(ids(got), want) {
		t.Fatalf("project filter = %v, want %v", ids(got), want)
	}
	got, _ = events.ListDeliveryAttempts(ctx, "ten_a", app.DeliveryLogFilter{EventID: "evt_a1"}, 0, 0)
	if want := []domain.DeliveryID{"del_evt_a1"}; !eq(ids(got), want) {
		t.Fatalf("event filter = %v, want %v", ids(got), want)
	}

	// ten_b naming ten_a's project still sees only its own row.
	got, _ = events.ListDeliveryAttempts(ctx, "ten_b", app.DeliveryLogFilter{Project: "alpha"}, 0, 0)
	if want := []domain.DeliveryID{"del_evt_b1"}; !eq(ids(got), want) {
		t.Fatalf("cross-tenant filter = %v, want %v", ids(got), want)
	}
}
