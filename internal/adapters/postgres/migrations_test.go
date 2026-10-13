package postgres

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// legacySchema is the pre-Event v1 shape: reports / deliveries.report_id.
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

// testPool returns a pool to HERALD_TEST_DATABASE_URL, skipping when unset.
// When HERALD_TEST_REQUIRE_DATABASE is set (CI does this) a missing URL is a
// hard failure, so DB-backed coverage cannot silently disappear from the build.
func testPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	url := os.Getenv("HERALD_TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("HERALD_TEST_REQUIRE_DATABASE") != "" {
			t.Fatal("HERALD_TEST_DATABASE_URL is required but empty (HERALD_TEST_REQUIRE_DATABASE is set)")
		}
		t.Skip("HERALD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

// resetSchema drops every Herald table so each test starts from a clean slate.
func resetSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS destinations, projects, users, tenants, accounts, deliveries, events, reports, credentials, schema_migrations CASCADE`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
}

func insertEvent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, project, key string, now time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, tenant_id, project_id, agent_id, status, title, body, links, context, idempotency_key, created_at)
		VALUES ($1,'ten_1',$2,'herdr','success','t','b','[]','{}',$3,$4)`, id, project, key, now); err != nil {
		t.Fatalf("insert event: %v", err)
	}
}

func TestMigrateFresh(t *testing.T) {
	pool, ctx := testPool(t)
	resetSchema(t, ctx, pool)

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate fresh: %v", err)
	}

	for _, table := range []string{"credentials", "events", "deliveries", "accounts", "tenants", "users", "projects", "destinations", "schema_migrations"} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&ok); err != nil || !ok {
			t.Fatalf("table %s exists=%v err=%v", table, ok, err)
		}
	}

	var (
		name      string
		sum       string
		appliedAt time.Time
	)
	if err := pool.QueryRow(ctx, `SELECT name, checksum, applied_at FROM schema_migrations WHERE version = 1`).
		Scan(&name, &sum, &appliedAt); err != nil {
		t.Fatalf("read migration row: %v", err)
	}
	if name != "baseline" || sum != checksum(baselineSchema) || appliedAt.IsZero() {
		t.Fatalf("migration row name=%q sum=%q applied_at=%v", name, sum, appliedAt)
	}
	if err := pool.QueryRow(ctx, `SELECT name, checksum FROM schema_migrations WHERE version = 2`).
		Scan(&name, &sum); err != nil {
		t.Fatalf("read ownership migration row: %v", err)
	}
	if name != "ownership" || sum != checksum(ownershipSchema) {
		t.Fatalf("ownership migration row name=%q sum=%q", name, sum)
	}

	var hasReportID bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_name = 'deliveries' AND column_name = 'report_id')`).Scan(&hasReportID); err != nil {
		t.Fatal(err)
	}
	if hasReportID {
		t.Fatal("fresh schema still has deliveries.report_id")
	}
}

// A second Migrate must be a no-op: no new rows and no rewritten applied_at.
func TestMigrateIdempotent(t *testing.T) {
	pool, ctx := testPool(t)
	resetSchema(t, ctx, pool)

	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var firstV1, firstV2 time.Time
	if err := pool.QueryRow(ctx, `SELECT applied_at FROM schema_migrations WHERE version = 1`).Scan(&firstV1); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT applied_at FROM schema_migrations WHERE version = 2`).Scan(&firstV2); err != nil {
		t.Fatal(err)
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var (
		count              int
		secondV1, secondV2 time.Time
	)
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT applied_at FROM schema_migrations WHERE version = 1`).Scan(&secondV1); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT applied_at FROM schema_migrations WHERE version = 2`).Scan(&secondV2); err != nil {
		t.Fatal(err)
	}
	if count != 2 || !secondV1.Equal(firstV1) || !secondV2.Equal(firstV2) {
		t.Fatalf("second run changed history: count=%d v1 %v->%v v2 %v->%v", count, firstV1, secondV1, firstV2, secondV2)
	}
}

// Upgrade from the legacy reports schema must rename in place and keep rows.
func TestMigrateFromLegacyReports(t *testing.T) {
	pool, ctx := testPool(t)
	resetSchema(t, ctx, pool)

	if _, err := pool.Exec(ctx, legacySchema); err != nil {
		t.Fatalf("seed legacy schema: %v", err)
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
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.events') IS NOT NULL, to_regclass('public.reports') IS NOT NULL`).
		Scan(&eventsExists, &reportsExists); err != nil {
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
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version = 1 AND name = 'baseline'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("migration not recorded: count=%d err=%v", count, err)
	}
	// The old per-tenant key constraint is replaced by a project-scoped one, so
	// another project can now reuse the migrated key.
	insertEvent(t, ctx, pool, "rep_2", "herald", "k-1", now)
}

// Upgrade from a v1 Event schema that predates schema_migrations (no history
// row) must record the baseline and add the ownership tables without touching
// existing data.
func TestMigrateFromEventSchema(t *testing.T) {
	pool, ctx := testPool(t)
	resetSchema(t, ctx, pool)

	if _, err := pool.Exec(ctx, baselineSchema); err != nil {
		t.Fatalf("seed Event schema: %v", err)
	}
	now := time.Now().UTC()
	insertEvent(t, ctx, pool, "evt_1", "axis", "k-1", now)
	if _, err := pool.Exec(ctx, `INSERT INTO credentials (token_hash, tenant_id, kind, projects, created_at)
		VALUES ('h1','owner','project',ARRAY['axis'],$1)`, now); err != nil {
		t.Fatal(err)
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate over Event schema: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE id='evt_1'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("event lost: count=%d err=%v", count, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM credentials WHERE token_hash='h1'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("credential lost: count=%d err=%v", count, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("migration rows=%d err=%v", count, err)
	}
	for _, table := range []string{"accounts", "tenants", "users", "projects", "destinations"} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&ok); err != nil || !ok {
			t.Fatalf("ownership table %s exists=%v err=%v", table, ok, err)
		}
	}
}

// Concurrent Migrate calls (as serve and dispatch would do at startup) must all
// succeed and record each step exactly once.
func TestMigrateConcurrent(t *testing.T) {
	pool, ctx := testPool(t)
	resetSchema(t, ctx, pool)

	if _, err := pool.Exec(ctx, legacySchema); err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO reports (id, tenant_id, project_id, agent_id, status, title, body, links, context, idempotency_key, created_at)
		VALUES ('rep_1','ten_1','axis','herdr','success','t','b','[]','{}','k-1',$1)`, now); err != nil {
		t.Fatal(err)
	}

	const n = 6
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- Migrate(ctx, pool)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent migrate: %v", err)
		}
	}

	var rows, count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("migration rows = %d, want 2", rows)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE id='rep_1'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("data lost after concurrent rename: count=%d err=%v", count, err)
	}
}

// An applied migration whose checksum changed must fail loudly.
func TestMigrateChecksumMismatch(t *testing.T) {
	pool, ctx := testPool(t)
	resetSchema(t, ctx, pool)
	t.Cleanup(func() { resetSchema(t, ctx, pool) })

	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE schema_migrations SET checksum = 'deadbeef' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}

	err := Migrate(ctx, pool)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, want checksum mismatch", err)
	}
}

// The advisory lock must not leak back into the pool: repeated Migrate calls on
// the same pool must finish instead of blocking on a held lock.
func TestMigrateReleasesLock(t *testing.T) {
	pool, ctx := testPool(t)
	resetSchema(t, ctx, pool)

	for i := 0; i < 3; i++ {
		done := make(chan error, 1)
		go func() { done <- Migrate(ctx, pool) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("migrate %d: %v", i, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("migrate %d blocked; advisory lock leaked", i)
		}
	}
}
