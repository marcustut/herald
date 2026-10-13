package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// seedAccountTenant inserts the account and tenant rows a project needs.
// Account and tenant provisioning is #29; until it lands, tests seed them
// directly so project/destination isolation can be exercised.
func seedAccountTenant(t *testing.T, ctx context.Context, pool *pgxpool.Pool, account, tenant string) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO accounts (id, name, created_at) VALUES ($1,$1,$2)`, account, now); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, account_id, name, created_at) VALUES ($1,$2,$1,$3)`, tenant, account, now); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
}

func mustProject(t *testing.T, id domain.ProjectID, tenant domain.TenantID, at time.Time) domain.Project {
	t.Helper()
	p, err := domain.NewProject(id, tenant, string(id), at)
	if err != nil {
		t.Fatalf("new project: %v", err)
	}
	return p
}

func mustDestination(t *testing.T, id domain.DestinationID, tenant domain.TenantID, project domain.ProjectID, kind domain.DestinationKind, target string, enabled bool, at time.Time) domain.Destination {
	t.Helper()
	d, err := domain.NewDestination(domain.Destination{
		ID: id, TenantID: tenant, ProjectID: project, Kind: kind, Target: target, Enabled: enabled, CreatedAt: at,
	})
	if err != nil {
		t.Fatalf("new destination: %v", err)
	}
	return d
}

// TestOwnershipTenantIsolation proves the project and destination stores are
// tenant-scoped: tenant B cannot inspect, re-point or delete tenant A's rows,
// even when it knows their ids.
func TestOwnershipTenantIsolation(t *testing.T) {
	pool, ctx := testPool(t)
	resetSchema(t, ctx, pool)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewOwnership(pool)
	now := time.Now().UTC()
	seedAccountTenant(t, ctx, pool, "acct_a", "ten_a")
	seedAccountTenant(t, ctx, pool, "acct_b", "ten_b")

	alpha := mustProject(t, "alpha", "ten_a", now)
	beta := mustProject(t, "beta", "ten_b", now)
	for _, p := range []domain.Project{alpha, beta} {
		if err := store.PutProject(ctx, p); err != nil {
			t.Fatalf("put project %s: %v", p.ID, err)
		}
	}

	// Read isolation: a project is only visible under its own tenant.
	if _, ok, err := store.Project(ctx, "ten_b", "alpha"); err != nil || ok {
		t.Fatalf("tenant B read tenant A project: ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.Project(ctx, "ten_a", "beta"); err != nil || ok {
		t.Fatalf("tenant A read tenant B project: ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.Project(ctx, "ten_a", "alpha"); err != nil || !ok {
		t.Fatalf("tenant A read own project: ok=%v err=%v", ok, err)
	}

	dstA := mustDestination(t, "dst_a", "ten_a", "alpha", domain.DestinationEmail, "a@example.com", true, now)
	dstB := mustDestination(t, "dst_b", "ten_b", "beta", domain.DestinationWebhook, "https://b.example.com", true, now)
	for _, d := range []domain.Destination{dstA, dstB} {
		if err := store.PutDestination(ctx, d); err != nil {
			t.Fatalf("put destination %s: %v", d.ID, err)
		}
	}

	// Read isolation: a destination id is only visible to its owning project.
	if _, ok, err := store.Destination(ctx, "ten_b", "beta", "dst_a"); err != nil || ok {
		t.Fatalf("tenant B read tenant A destination: ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.Destination(ctx, "ten_a", "alpha", "dst_b"); err != nil || ok {
		t.Fatalf("tenant A read tenant B destination: ok=%v err=%v", ok, err)
	}

	listB, err := store.DestinationsByProject(ctx, "ten_b", "beta")
	if err != nil {
		t.Fatalf("list tenant B destinations: %v", err)
	}
	if len(listB) != 1 || listB[0].ID != "dst_b" {
		t.Fatalf("tenant B destinations = %+v, want only dst_b", listB)
	}
	if got, _ := store.DestinationsByProject(ctx, "ten_a", "alpha"); len(got) != 1 || got[0].ID != "dst_a" {
		t.Fatalf("tenant A destinations = %+v, want only dst_a", got)
	}

	// Mutate isolation: tenant B cannot overwrite tenant A's destination id.
	hijack := mustDestination(t, "dst_a", "ten_b", "beta", domain.DestinationEmail, "attacker@evil.example", false, now)
	if err := store.PutDestination(ctx, hijack); !errors.Is(err, app.ErrDestinationNotFound) {
		t.Fatalf("cross-tenant put err = %v, want ErrDestinationNotFound", err)
	}
	got, ok, err := store.Destination(ctx, "ten_a", "alpha", "dst_a")
	if err != nil || !ok {
		t.Fatalf("tenant A destination after hijack: ok=%v err=%v", ok, err)
	}
	if got.Target != "a@example.com" || !got.Enabled {
		t.Fatalf("tenant A destination was mutated: %+v", got)
	}

	// Mutate isolation: tenant B cannot delete tenant A's destination or project.
	if err := store.DeleteDestination(ctx, "ten_b", "beta", "dst_a"); !errors.Is(err, app.ErrDestinationNotFound) {
		t.Fatalf("cross-tenant delete destination err = %v, want ErrDestinationNotFound", err)
	}
	if _, ok, _ := store.Destination(ctx, "ten_a", "alpha", "dst_a"); !ok {
		t.Fatal("tenant A destination disappeared after cross-tenant delete")
	}
	if err := store.DeleteProject(ctx, "ten_b", "alpha"); !errors.Is(err, app.ErrProjectNotFound) {
		t.Fatalf("cross-tenant delete project err = %v, want ErrProjectNotFound", err)
	}
	if _, ok, _ := store.Project(ctx, "ten_a", "alpha"); !ok {
		t.Fatal("tenant A project disappeared after cross-tenant delete")
	}

	// Sink resolution: only enabled destinations, and only under the right tenant.
	off := mustDestination(t, "dst_off", "ten_a", "alpha", domain.DestinationEmail, "off@example.com", false, now.Add(time.Second))
	if err := store.PutDestination(ctx, off); err != nil {
		t.Fatalf("put disabled destination: %v", err)
	}
	sinks, err := store.EnabledSinks(ctx, "ten_a", "alpha")
	if err != nil {
		t.Fatalf("enabled sinks: %v", err)
	}
	if len(sinks) != 1 || sinks[0] != "dst_a" {
		t.Fatalf("tenant A enabled sinks = %v, want [dst_a]", sinks)
	}
	if other, _ := store.EnabledSinks(ctx, "ten_b", "alpha"); len(other) != 0 {
		t.Fatalf("wrong tenant resolved sinks %v, want none", other)
	}
}

// TestEventAndCredentialTenantIsolation proves the pre-existing stores stay
// tenant-scoped: one tenant cannot read another's event by idempotency key, and
// cannot resolve another tenant's credential.
func TestEventAndCredentialTenantIsolation(t *testing.T) {
	pool, ctx := testPool(t)
	resetSchema(t, ctx, pool)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	events := NewEvents(pool)
	creds := NewCredentials(pool)
	now := time.Now().UTC()

	save := func(id domain.EventID, tenant domain.TenantID, project domain.ProjectID, key domain.IdempotencyKey) {
		t.Helper()
		event, err := domain.NewEvent(domain.NewEventInput{
			ID: id, TenantID: tenant, ProjectID: project, AgentID: "herdr",
			Status: domain.StatusSuccess, Title: "t", Body: "b", IdempotencyKey: key, CreatedAt: now,
		})
		if err != nil {
			t.Fatalf("new event: %v", err)
		}
		if err := events.Save(ctx, event, nil); err != nil {
			t.Fatalf("save event %s: %v", id, err)
		}
	}
	save("evt_a", "ten_a", "alpha", "shared")

	// Tenant B cannot read tenant A's event, and the same key is free for tenant
	// B's own project.
	if _, ok, err := events.ByIdempotencyKey(ctx, "ten_b", "beta", "shared"); err != nil || ok {
		t.Fatalf("tenant B read tenant A event: ok=%v err=%v", ok, err)
	}
	if _, ok, _ := events.ByIdempotencyKey(ctx, "ten_b", "alpha", "shared"); ok {
		t.Fatal("same project slug under another tenant leaked an event")
	}
	if _, ok, _ := events.ByIdempotencyKey(ctx, "ten_a", "beta", "shared"); ok {
		t.Fatal("same tenant under another project leaked an event")
	}
	if _, ok, _ := events.ByIdempotencyKey(ctx, "ten_a", "alpha", "shared"); !ok {
		t.Fatal("owner could not read its own event")
	}
	save("evt_b", "ten_b", "beta", "shared")

	// Credentials resolve by tenant; another tenant cannot revoke or use one.
	if err := creds.Put(ctx, app.Credential{
		Hash: "hash_a", TenantID: "ten_a", Kind: domain.TokenProject,
		Projects: []domain.ProjectID{"alpha"}, CreatedAt: now,
	}); err != nil {
		t.Fatalf("put credential: %v", err)
	}
	if _, err := creds.ProjectCredential(ctx, "ten_b", "alpha"); !errors.Is(err, app.ErrCredentialNotFound) {
		t.Fatalf("tenant B resolved tenant A credential: err = %v", err)
	}
	if _, err := creds.ProjectCredential(ctx, "ten_a", "alpha"); err != nil {
		t.Fatalf("owner did not resolve own credential: %v", err)
	}
}
