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
	if _, err := pool.Exec(ctx, "TRUNCATE deliveries, events, credentials"); err != nil {
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
	if _, ok, _ := events.ByIdempotencyKey(ctx, "ten_1", "run-1"); !ok {
		t.Fatal("idempotency lookup failed")
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

const legacySchema = `
CREATE TABLE reports (
	id text PRIMARY KEY, tenant_id text NOT NULL, project_id text NOT NULL, agent_id text NOT NULL,
	status text NOT NULL, title text NOT NULL, body text NOT NULL, links jsonb NOT NULL,
	context jsonb NOT NULL, idempotency_key text NOT NULL, created_at timestamptz NOT NULL,
	UNIQUE (tenant_id, idempotency_key)
);
CREATE TABLE deliveries (
	id text PRIMARY KEY, report_id text NOT NULL REFERENCES reports(id), sink_id text NOT NULL,
	state text NOT NULL, attempts integer NOT NULL, next_attempt_at timestamptz NOT NULL,
	lease_until timestamptz, last_error text NOT NULL DEFAULT '', created_at timestamptz NOT NULL,
	updated_at timestamptz NOT NULL
);
CREATE INDEX deliveries_report ON deliveries (report_id);
`

// Migrate must rename the pre-Event schema in place and preserve rows.
func TestIntegrationLegacyRename(t *testing.T) {
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

	if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS deliveries, events, reports, credentials"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, legacySchema); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO reports (id, tenant_id, project_id, agent_id, status, title, body, links, context, idempotency_key, created_at)
		VALUES ('rep_1','ten_1','axis','herdr','success','t','b','[]','{}','k-1',$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO deliveries (id, report_id, sink_id, state, attempts, next_attempt_at, created_at, updated_at)
		VALUES ('del_1','rep_1','email','pending',0,$1,$1,$1)`, now); err != nil {
		t.Fatal(err)
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate over legacy schema: %v", err)
	}

	var eventsExists, reportsExists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.events') IS NOT NULL, to_regclass('public.reports') IS NOT NULL`).Scan(&eventsExists, &reportsExists); err != nil {
		t.Fatal(err)
	}
	if !eventsExists || reportsExists {
		t.Fatalf("events=%v reports=%v", eventsExists, reportsExists)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE id='rep_1'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("data lost: count=%d err=%v", count, err)
	}
	var eventID string
	if err := pool.QueryRow(ctx, `SELECT event_id FROM deliveries WHERE id='del_1'`).Scan(&eventID); err != nil || eventID != "rep_1" {
		t.Fatalf("event_id=%q err=%v", eventID, err)
	}
	// Migrate is idempotent.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}
