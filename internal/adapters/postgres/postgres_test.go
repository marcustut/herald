package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

func TestNullTimeHelpers(t *testing.T) {
	if nullTime(time.Time{}) != nil {
		t.Fatal("zero time should be nil")
	}
	now := time.Now()
	if got := nullTime(now); got == nil || !got.Equal(now) {
		t.Fatal("non-zero time should round-trip")
	}
	if !timeOrZero(nil).IsZero() {
		t.Fatal("nil should be zero time")
	}
	if !timeOrZero(&now).Equal(now) {
		t.Fatal("value should round-trip")
	}
}

func TestIsUniqueViolation(t *testing.T) {
	if isUniqueViolation(errors.New("nope")) {
		t.Fatal("plain error is not a unique violation")
	}
}

// Integration tests run only when HERALD_TEST_DATABASE_URL is set, e.g.
//
//	HERALD_TEST_DATABASE_URL=postgres://... go test ./internal/adapters/postgres/
func TestIntegration(t *testing.T) {
	url := os.Getenv("HERALD_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("HERALD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	resetSchema(t, ctx, pool)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	creds := NewCredentials(pool)
	cred := app.Credential{
		Hash: "h1", TenantID: "ten_1", Kind: domain.TokenProject,
		Projects: []domain.ProjectID{"axis"}, CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
	if err := creds.Put(ctx, cred); err != nil {
		t.Fatal(err)
	}
	got, err := creds.ByHash(ctx, "h1")
	if err != nil || got.TenantID != "ten_1" || !got.Allows("axis") {
		t.Fatalf("got %+v err %v", got, err)
	}
	if _, err := creds.ByHash(ctx, "missing"); !errors.Is(err, app.ErrCredentialNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, err := creds.ProjectCredential(ctx, "ten_1", "axis"); err != nil {
		t.Fatal(err)
	}
	// last-used is a hash-keyed write; an unknown hash is a no-op, not an error.
	usedAt := time.Now().UTC().Truncate(time.Microsecond)
	if err := creds.TouchLastUsed(ctx, "h1", usedAt); err != nil {
		t.Fatal(err)
	}
	if got, err := creds.ByHash(ctx, "h1"); err != nil || !got.LastUsedAt.Equal(usedAt) {
		t.Fatalf("last-used = %v err %v, want %v", got.LastUsedAt, err, usedAt)
	}
	if err := creds.TouchLastUsed(ctx, "missing", usedAt); err != nil {
		t.Fatalf("unknown hash must be a no-op: %v", err)
	}

	events := NewEvents(pool)
	rep := domain.Event{
		ID: "evt_1", TenantID: "ten_1", ProjectID: "axis", AgentID: "herdr",
		Status: domain.StatusSuccess, Title: "t", Body: "b",
		Links: []domain.Link{{Label: "PR", URL: "https://x.test/1"}}, Context: domain.Context{"host": "h1"},
		IdempotencyKey: "run-1", CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
	del := domain.NewDelivery("del_1", "evt_1", "email", rep.CreatedAt)
	if err := events.Save(ctx, rep, []domain.Delivery{del}); err != nil {
		t.Fatal(err)
	}
	if err := events.Save(ctx, rep, nil); !errors.Is(err, app.ErrIdempotencyConflict) {
		t.Fatalf("conflict err = %v", err)
	}
	stored, ok, err := events.ByID(ctx, "evt_1")
	if err != nil || !ok || stored.Links[0].URL != "https://x.test/1" || stored.Context.Host() != "h1" {
		t.Fatalf("stored %+v ok %v err %v", stored, ok, err)
	}
	if _, ok, _ := events.ByIdempotencyKey(ctx, "ten_1", "axis", "run-1"); !ok {
		t.Fatal("idempotency lookup failed")
	}
	// Keys are scoped per project: another project may reuse the same key.
	other := rep
	other.ID = "evt_2"
	other.ProjectID = "herald"
	if err := events.Save(ctx, other, nil); err != nil {
		t.Fatalf("cross-project key should not conflict: %v", err)
	}
	if _, ok, _ := events.ByIdempotencyKey(ctx, "ten_1", "herald", "run-1"); !ok {
		t.Fatal("cross-project lookup failed")
	}

	claimed, err := events.Claim(ctx, time.Now().UTC(), time.Minute, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claimed %+v err %v", claimed, err)
	}
	d := claimed[0]
	if d.State != domain.DeliveryInflight || d.Attempts != 1 {
		t.Fatalf("claimed delivery %+v", d)
	}
	if err := d.Succeed(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := events.Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	if _, err := events.Claim(ctx, time.Now().UTC(), time.Minute, 10); err != nil {
		t.Fatal(err)
	}
	if err := events.Update(ctx, domain.Delivery{ID: "nope"}); !errors.Is(err, app.ErrDeliveryNotFound) {
		t.Fatalf("err = %v", err)
	}
}
