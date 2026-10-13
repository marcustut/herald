package postgres_test

import (
	"context"
	"net/smtp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/marcustut/herald/internal/adapters/id"
	"github.com/marcustut/herald/internal/adapters/postgres"
	"github.com/marcustut/herald/internal/adapters/sink"
	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/config"
	"github.com/marcustut/herald/internal/domain"
)

// recordingTransport captures the recipients of every report email the email
// sink would have sent, so a DB-backed delivery can be observed without SMTP.
type recordingTransport struct {
	mu sync.Mutex
	to []string
}

func (r *recordingTransport) Send(_ string, _ smtp.Auth, _ string, to []string, _ []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.to = append(r.to, to...)
	return nil
}

func (r *recordingTransport) recipients() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.to...)
}

// seedHosted owns the account → tenant → project chain a hosted destination
// hangs off. Account/tenant provisioning is #29; here it is seeded directly.
func seedHosted(t *testing.T, ctx context.Context, pool *pgxpool.Pool, store *postgres.Ownership, account, tenant string, project domain.ProjectID, now time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO accounts (id, name, created_at) VALUES ($1,$1,$2)`, account, now); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, account_id, name, created_at) VALUES ($1,$2,$1,$3)`, tenant, account, now); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	p, err := domain.NewProject(project, domain.TenantID(tenant), string(project), now)
	if err != nil {
		t.Fatalf("new project: %v", err)
	}
	if err := store.PutProject(ctx, p); err != nil {
		t.Fatalf("put project: %v", err)
	}
}

func putDestination(t *testing.T, ctx context.Context, store *postgres.Ownership, dstID domain.DestinationID, tenant domain.TenantID, project domain.ProjectID, kind domain.DestinationKind, target string, enabled bool, verifiedAt, now time.Time) {
	t.Helper()
	d, err := domain.NewDestination(domain.Destination{
		ID: dstID, TenantID: tenant, ProjectID: project, Kind: kind, Target: target,
		Enabled: enabled, VerifiedAt: verifiedAt, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("new destination %s: %v", dstID, err)
	}
	if err := store.PutDestination(ctx, d); err != nil {
		t.Fatalf("put destination %s: %v", dstID, err)
	}
}

func hostedDispatcher(events *postgres.Events, source app.SinkSource, clock app.Clock) app.Dispatcher {
	return app.Dispatcher{
		Deliveries:  events,
		Events:      events,
		Sinks:       source,
		Clock:       clock,
		Lease:       time.Minute,
		MaxAttempts: 3,
		Backoff:     domain.Backoff{Base: time.Second, Cap: time.Minute},
		Batch:       10,
	}
}

func submitEvent(t *testing.T, ctx context.Context, events *postgres.Events, directory app.SinkDirectory, clock app.Clock, tenant domain.TenantID, project domain.ProjectID) domain.EventID {
	t.Helper()
	principal, err := domain.NewPrincipal(tenant, project)
	if err != nil {
		t.Fatalf("principal: %v", err)
	}
	uc := app.SubmitEvent{Events: events, Sinks: directory, IDs: id.Random{}, Clock: clock}
	eventID, err := uc.Handle(ctx, principal, app.SubmitRequest{Status: "success", Title: "deploy", Body: "done", Agent: "herdr"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return eventID
}

// TestIntegrationHostedEmailDeliversOnlyVerified proves the hosted delivery
// path end to end: a verified email destination is resolved from the database
// and receives the report, while an unverified destination is never routed to.
func TestIntegrationHostedEmailDeliversOnlyVerified(t *testing.T) {
	pool, ctx := openMigrated(t)
	store := postgres.NewOwnership(pool)
	events := postgres.NewEvents(pool)
	clock := newFakeClock()
	now := clock.Now()
	seedHosted(t, ctx, pool, store, "acct_1", "ten_1", "axis", now)
	putDestination(t, ctx, store, "dst_verified", "ten_1", "axis", domain.DestinationEmail, "ok@example.com", true, now, now)
	putDestination(t, ctx, store, "dst_unverified", "ten_1", "axis", domain.DestinationEmail, "no@example.com", true, time.Time{}, now)

	transport := &recordingTransport{}
	cfg := config.Gateway{SMTP: config.SMTP{Addr: "127.0.0.1:25", From: "herald@example.com"}}
	source := sink.NewDestinationSource(store, cfg, "", "").WithMailTransport(transport)

	submitEvent(t, ctx, events, store, clock, "ten_1", "axis")

	queued, err := events.ListDeliveries(ctx, app.DeliveryFilter{}, 0)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(queued) != 1 || queued[0].SinkID != "dst_verified" {
		t.Fatalf("queued = %+v, want exactly the verified destination", queued)
	}

	if n, err := hostedDispatcher(events, source, clock).ProcessBatch(ctx); err != nil || n != 1 {
		t.Fatalf("process n=%d err=%v", n, err)
	}
	got := transport.recipients()
	if len(got) != 1 || got[0] != "ok@example.com" {
		t.Fatalf("recipients = %v, want [ok@example.com]", got)
	}
	stored, ok, err := events.DeliveryByID(ctx, queued[0].ID)
	if err != nil || !ok || stored.State != domain.DeliveryDelivered {
		t.Fatalf("delivery = %+v ok=%v err=%v, want delivered", stored, ok, err)
	}
}

// TestIntegrationHostedDispatchRechecksDestination proves the source re-reads
// the row at delivery time: a destination disabled or removed after the event
// was queued is not delivered to, and its delivery dead-letters.
func TestIntegrationHostedDispatchRechecksDestination(t *testing.T) {
	pool, ctx := openMigrated(t)
	store := postgres.NewOwnership(pool)
	events := postgres.NewEvents(pool)
	clock := newFakeClock()
	now := clock.Now()
	seedHosted(t, ctx, pool, store, "acct_1", "ten_1", "axis", now)
	putDestination(t, ctx, store, "dst_1", "ten_1", "axis", domain.DestinationEmail, "ok@example.com", true, now, now)

	transport := &recordingTransport{}
	cfg := config.Gateway{SMTP: config.SMTP{Addr: "127.0.0.1:25", From: "herald@example.com"}}
	source := sink.NewDestinationSource(store, cfg, "", "").WithMailTransport(transport)
	d := hostedDispatcher(events, source, clock)

	// Queued while enabled and verified, then disabled before the dispatch.
	submitEvent(t, ctx, events, store, clock, "ten_1", "axis")
	putDestination(t, ctx, store, "dst_1", "ten_1", "axis", domain.DestinationEmail, "ok@example.com", false, now, now)
	if n, err := d.ProcessBatch(ctx); err != nil || n != 1 {
		t.Fatalf("disabled: process n=%d err=%v", n, err)
	}
	if got := transport.recipients(); len(got) != 0 {
		t.Fatalf("disabled destination received %v", got)
	}
	assertDead(t, ctx, events, deliveryIDs(t, ctx, events)[0])

	// Re-enable and re-verify, queue a second event, then remove the
	// destination before the dispatch.
	putDestination(t, ctx, store, "dst_1", "ten_1", "axis", domain.DestinationEmail, "ok@example.com", true, now, now)
	submitEvent(t, ctx, events, store, clock, "ten_1", "axis")
	if err := store.DeleteDestination(ctx, "ten_1", "axis", "dst_1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n, err := d.ProcessBatch(ctx); err != nil || n != 1 {
		t.Fatalf("removed: process n=%d err=%v", n, err)
	}
	if got := transport.recipients(); len(got) != 0 {
		t.Fatalf("removed destination received %v", got)
	}
	for _, id := range deliveryIDs(t, ctx, events) {
		assertDead(t, ctx, events, id)
	}
}

// TestIntegrationHostedUnknownSinkIsDeadLetter proves a delivery whose sink id
// has no destination fails safely: a permanent failure, no panic.
func TestIntegrationHostedUnknownSinkIsDeadLetter(t *testing.T) {
	pool, ctx := openMigrated(t)
	store := postgres.NewOwnership(pool)
	events := postgres.NewEvents(pool)
	clock := newFakeClock()
	now := clock.Now()
	seedHosted(t, ctx, pool, store, "acct_1", "ten_1", "axis", now)

	event, err := domain.NewEvent(domain.NewEventInput{
		ID: "evt_ghost", TenantID: "ten_1", ProjectID: "axis", AgentID: "herdr",
		Status: domain.StatusSuccess, Title: "t", IdempotencyKey: "k-ghost", CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("new event: %v", err)
	}
	if err := events.Save(ctx, event, []domain.Delivery{domain.NewDelivery("del_ghost", "evt_ghost", "dst_missing", now)}); err != nil {
		t.Fatalf("save: %v", err)
	}

	cfg := config.Gateway{SMTP: config.SMTP{Addr: "127.0.0.1:25", From: "herald@example.com"}}
	source := sink.NewDestinationSource(store, cfg, "", "").WithMailTransport(&recordingTransport{})
	if n, err := hostedDispatcher(events, source, clock).ProcessBatch(ctx); err != nil || n != 1 {
		t.Fatalf("process n=%d err=%v", n, err)
	}
	assertDead(t, ctx, events, "del_ghost")
}

func deliveryIDs(t *testing.T, ctx context.Context, events *postgres.Events) []domain.DeliveryID {
	t.Helper()
	rows, err := events.ListDeliveries(ctx, app.DeliveryFilter{}, 0)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	ids := make([]domain.DeliveryID, len(rows))
	for i, d := range rows {
		ids[i] = d.ID
	}
	return ids
}

func assertDead(t *testing.T, ctx context.Context, events *postgres.Events, id domain.DeliveryID) {
	t.Helper()
	d, ok, err := events.DeliveryByID(ctx, id)
	if err != nil || !ok {
		t.Fatalf("delivery %s ok=%v err=%v", id, ok, err)
	}
	if d.State != domain.DeliveryDead {
		t.Fatalf("delivery %s state = %s, want dead", id, d.State)
	}
	if !strings.Contains(d.LastError, "unknown sink") {
		t.Fatalf("delivery %s last error = %q, want unknown sink", id, d.LastError)
	}
}
