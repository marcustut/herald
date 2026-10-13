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
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE deliveries, reports, credentials"); err != nil {
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

	reports := NewReports(pool)
	rep := domain.Report{
		ID: "rep_1", TenantID: "ten_1", ProjectID: "axis", AgentID: "herdr",
		Status: domain.StatusSuccess, Title: "t", Body: "b",
		Links: []domain.Link{{Label: "PR", URL: "https://x.test/1"}}, Context: domain.Context{"host": "h1"},
		IdempotencyKey: "run-1", CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
	del := domain.NewDelivery("del_1", "rep_1", "email", rep.CreatedAt)
	if err := reports.Save(ctx, rep, []domain.Delivery{del}); err != nil {
		t.Fatal(err)
	}
	if err := reports.Save(ctx, rep, nil); !errors.Is(err, app.ErrIdempotencyConflict) {
		t.Fatalf("conflict err = %v", err)
	}
	stored, ok, err := reports.ByID(ctx, "rep_1")
	if err != nil || !ok || stored.Links[0].URL != "https://x.test/1" || stored.Context.Host() != "h1" {
		t.Fatalf("stored %+v ok %v err %v", stored, ok, err)
	}
	if _, ok, _ := reports.ByIdempotencyKey(ctx, "ten_1", "run-1"); !ok {
		t.Fatal("idempotency lookup failed")
	}

	claimed, err := reports.Claim(ctx, time.Now().UTC(), time.Minute, 10)
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
	if err := reports.Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	if _, err := reports.Claim(ctx, time.Now().UTC(), time.Minute, 10); err != nil {
		t.Fatal(err)
	}
	if err := reports.Update(ctx, domain.Delivery{ID: "nope"}); !errors.Is(err, app.ErrDeliveryNotFound) {
		t.Fatalf("err = %v", err)
	}
}
