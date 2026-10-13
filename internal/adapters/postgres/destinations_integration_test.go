package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// TestDestinationRoutingRequiresVerification proves an enabled but unverified
// destination is never routed to, and that verification (owned by the stored
// token, not the caller) turns it on.
func TestDestinationRoutingRequiresVerification(t *testing.T) {
	pool, ctx := testPool(t)
	resetSchema(t, ctx, pool)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewOwnership(pool)
	now := time.Now().UTC()
	seedAccountTenant(t, ctx, pool, "acct_a", "ten_a")
	if err := store.PutProject(ctx, mustProject(t, "alpha", "ten_a", now)); err != nil {
		t.Fatalf("put project: %v", err)
	}

	unverified := mustUnverifiedDestination(t, "dst_unverified", "ten_a", "alpha", domain.DestinationEmail, "ops@example.com", now)
	webhook := mustDestination(t, "dst_hook", "ten_a", "alpha", domain.DestinationWebhook, "https://hooks.example.com/x", true, now)
	for _, d := range []domain.Destination{unverified, webhook} {
		if err := store.PutDestination(ctx, d); err != nil {
			t.Fatalf("put destination %s: %v", d.ID, err)
		}
	}

	// The unverified email is enabled but not routed to; the webhook is
	// verified by configuration and is.
	sinks, err := store.EnabledSinks(ctx, "ten_a", "alpha")
	if err != nil {
		t.Fatalf("enabled sinks: %v", err)
	}
	if len(sinks) != 1 || sinks[0] != "dst_hook" {
		t.Fatalf("sinks = %v, want [dst_hook]", sinks)
	}

	// verified_at round-trips through the store.
	stored, ok, err := store.Destination(ctx, "ten_a", "alpha", "dst_hook")
	if err != nil || !ok || !stored.Verified() {
		t.Fatalf("stored webhook = %+v ok=%v err=%v", stored, ok, err)
	}

	// Marking verified turns routing on, for the owner only.
	if err := store.MarkVerified(ctx, "ten_b", "beta", "dst_unverified", now); !errors.Is(err, app.ErrDestinationNotFound) {
		t.Fatalf("cross-tenant mark verified err = %v, want ErrDestinationNotFound", err)
	}
	if err := store.MarkVerified(ctx, "ten_a", "alpha", "dst_unverified", now); err != nil {
		t.Fatalf("mark verified: %v", err)
	}
	sinks, err = store.EnabledSinks(ctx, "ten_a", "alpha")
	if err != nil {
		t.Fatalf("enabled sinks after verify: %v", err)
	}
	if len(sinks) != 2 {
		t.Fatalf("sinks = %v, want both destinations", sinks)
	}

	// Removal stops routing.
	if err := store.DeleteDestination(ctx, "ten_a", "alpha", "dst_unverified"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	sinks, err = store.EnabledSinks(ctx, "ten_a", "alpha")
	if err != nil {
		t.Fatalf("enabled sinks after delete: %v", err)
	}
	if len(sinks) != 1 || sinks[0] != "dst_hook" {
		t.Fatalf("sinks after delete = %v, want [dst_hook]", sinks)
	}
}

// TestDestinationVerificationTokens proves the single-use, expiry and cascade
// behaviour of verification tokens.
func TestDestinationVerificationTokens(t *testing.T) {
	pool, ctx := testPool(t)
	resetSchema(t, ctx, pool)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewOwnership(pool)
	tokens := NewDestinationVerifications(pool)
	now := time.Now().UTC()
	seedAccountTenant(t, ctx, pool, "acct_a", "ten_a")
	if err := store.PutProject(ctx, mustProject(t, "alpha", "ten_a", now)); err != nil {
		t.Fatalf("put project: %v", err)
	}
	if err := store.PutDestination(ctx, mustUnverifiedDestination(t, "dst_1", "ten_a", "alpha", domain.DestinationEmail, "ops@example.com", now)); err != nil {
		t.Fatalf("put destination: %v", err)
	}

	rec := app.DestinationVerificationRecord{
		Hash: "vh_1", DestinationID: "dst_1", TenantID: "ten_a", ProjectID: "alpha",
		Email: "ops@example.com", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := tokens.PutDestinationVerification(ctx, rec); err != nil {
		t.Fatalf("put verification: %v", err)
	}

	got, err := tokens.ConsumeDestinationVerification(ctx, "vh_1", now)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if got.DestinationID != "dst_1" || got.TenantID != "ten_a" || got.ProjectID != "alpha" || got.Email != "ops@example.com" {
		t.Fatalf("consumed = %+v, want the stored owner", got)
	}
	// The stored owner is enough to mark verified.
	if err := store.MarkVerified(ctx, got.TenantID, got.ProjectID, got.DestinationID, now); err != nil {
		t.Fatalf("mark verified from token: %v", err)
	}
	// Replay is refused.
	if _, err := tokens.ConsumeDestinationVerification(ctx, "vh_1", now); !errors.Is(err, app.ErrDestinationVerificationNotFound) {
		t.Fatalf("replay err = %v, want ErrDestinationVerificationNotFound", err)
	}

	// Expiry is refused.
	if err := tokens.PutDestinationVerification(ctx, app.DestinationVerificationRecord{
		Hash: "vh_expired", DestinationID: "dst_1", TenantID: "ten_a", ProjectID: "alpha",
		Email: "ops@example.com", CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatalf("put expired token: %v", err)
	}
	if _, err := tokens.ConsumeDestinationVerification(ctx, "vh_expired", now.Add(2*time.Minute)); !errors.Is(err, app.ErrDestinationVerificationNotFound) {
		t.Fatalf("expired err = %v, want ErrDestinationVerificationNotFound", err)
	}

	// Deleting the destination cascades its tokens away, so a later link cannot
	// resurrect a removed destination.
	if err := tokens.PutDestinationVerification(ctx, app.DestinationVerificationRecord{
		Hash: "vh_cascade", DestinationID: "dst_1", TenantID: "ten_a", ProjectID: "alpha",
		Email: "ops@example.com", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("put cascade token: %v", err)
	}
	if err := store.DeleteDestination(ctx, "ten_a", "alpha", "dst_1"); err != nil {
		t.Fatalf("delete destination: %v", err)
	}
	if _, err := tokens.ConsumeDestinationVerification(ctx, "vh_cascade", now); !errors.Is(err, app.ErrDestinationVerificationNotFound) {
		t.Fatalf("cascade err = %v, want ErrDestinationVerificationNotFound", err)
	}
}

func mustUnverifiedDestination(t *testing.T, id domain.DestinationID, tenant domain.TenantID, project domain.ProjectID, kind domain.DestinationKind, target string, at time.Time) domain.Destination {
	t.Helper()
	d, err := domain.NewDestination(domain.Destination{
		ID: id, TenantID: tenant, ProjectID: project, Kind: kind, Target: target, Enabled: true, CreatedAt: at,
	})
	if err != nil {
		t.Fatalf("new unverified destination: %v", err)
	}
	return d
}
