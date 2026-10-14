// Package postgres_test holds black-box integration tests for the whole
// submission pipeline. They live beside the postgres adapter (as an external
// test package) so they share one test binary with the adapter's own
// integration tests: `go test` runs separate packages in parallel, and every
// DB-backed test here resets the same schema, so co-locating them serializes
// access to the one test database.
//
// Nothing in this file touches production code. It drives only the public
// adapters: the HTTP API, the CLI, the application dispatcher, and the
// PostgreSQL stores, with the network sinks replaced by an in-process fake.
package postgres_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/marcustut/herald/internal/adapters/hash"
	"github.com/marcustut/herald/internal/adapters/httpapi"
	"github.com/marcustut/herald/internal/adapters/id"
	"github.com/marcustut/herald/internal/adapters/memory"
	"github.com/marcustut/herald/internal/adapters/postgres"
	"github.com/marcustut/herald/internal/adapters/secret"
	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/cli"
	"github.com/marcustut/herald/internal/client"
	"github.com/marcustut/herald/internal/domain"
)

// requireDatabase returns the test DSN, or skips. When
// HERALD_TEST_REQUIRE_DATABASE is set (CI does this) a missing DSN is a hard
// failure instead of a silent skip, so DB-backed coverage cannot rot.
func requireDatabase(t *testing.T) string {
	t.Helper()
	url := os.Getenv("HERALD_TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("HERALD_TEST_REQUIRE_DATABASE") != "" {
			t.Fatal("HERALD_TEST_DATABASE_URL is required but empty (HERALD_TEST_REQUIRE_DATABASE is set)")
		}
		t.Skip("HERALD_TEST_DATABASE_URL not set; skipping DB-backed integration test")
	}
	return url
}

// openMigrated returns a pool to a freshly migrated, empty schema.
func openMigrated(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	url := requireDatabase(t)
	ctx := context.Background()
	pool, err := postgres.Open(ctx, url)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS destination_verifications, sessions, login_tokens, destinations, projects, users, tenants, accounts, deliveries, events, reports, credentials, schema_migrations CASCADE`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool, ctx
}

// fakeClock is a controllable, concurrency-safe app.Clock. Fixing it keeps
// backoff and lease timing deterministic without sleeping in tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// recordingSink is the in-process sink the integration tests deliver to. It
// records every accepted event and can fail a chosen attempt, so retry and
// dead-letter behaviour can be driven from tests.
type recordingSink struct {
	id domain.SinkID

	mu   sync.Mutex
	n    int
	seen []domain.Event

	// fail, when set, is consulted after the call is counted.
	fail func(call int, e domain.Event) error
}

func newRecordingSink(id domain.SinkID) *recordingSink {
	return &recordingSink{id: id}
}

func (s *recordingSink) ID() domain.SinkID { return s.id }

func (s *recordingSink) Deliver(_ context.Context, e domain.Event) error {
	s.mu.Lock()
	s.n++
	call := s.n
	s.seen = append(s.seen, e)
	s.mu.Unlock()
	if s.fail != nil {
		return s.fail(call, e)
	}
	return nil
}

func (s *recordingSink) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func (s *recordingSink) events() []domain.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.Event(nil), s.seen...)
}

func (s *recordingSink) deliveriesByEvent() map[domain.EventID]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[domain.EventID]int, len(s.seen))
	for _, e := range s.seen {
		out[e.ID]++
	}
	return out
}

// fixedSinks is a static app.SinkDirectory: project "axis" fans out to whatever
// sink ids the test declares.
type fixedSinks []domain.SinkID

func (s fixedSinks) EnabledSinks(context.Context, domain.TenantID, domain.ProjectID) ([]domain.SinkID, error) {
	return s, nil
}

var _ app.SinkDirectory = fixedSinks(nil)

// staticSource is a test app.SinkSource over a fixed map of built sinks.
type staticSource map[domain.SinkID]app.Sink

func (m staticSource) Sink(_ context.Context, _ domain.TenantID, _ domain.ProjectID, id domain.SinkID) (app.Sink, bool, error) {
	s, ok := m[id]
	return s, ok, nil
}

// harness wires a real PostgreSQL-backed submission path: an HTTP API over the
// postgres stores, a project token, and a recording sink the dispatcher pushes
// to.
type harness struct {
	ctx    context.Context
	events *postgres.Events
	clock  *fakeClock
	sink   *recordingSink
	server *httptest.Server
	token  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	pool, ctx := openMigrated(t)
	events := postgres.NewEvents(pool)
	creds := postgres.NewCredentials(pool)
	clock := newFakeClock()
	sink := newRecordingSink("email")

	issuer := app.Issuer{Store: creds, Hasher: hash.SHA256{}, Secrets: secret.Generator{}, Clock: clock}
	token, err := issuer.IssueProjectToken(ctx, "ten_1", "axis")
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	api := &httpapi.Server{
		Auth:   app.Authenticator{Store: creds, Hasher: hash.SHA256{}},
		Submit: app.SubmitEvent{Events: events, Sinks: fixedSinks{"email"}, IDs: id.Random{}, Clock: clock},
	}
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	return &harness{
		ctx:    ctx,
		events: events,
		clock:  clock,
		sink:   sink,
		server: ts,
		token:  token,
	}
}

func (h *harness) dispatcher(maxAttempts int) app.Dispatcher {
	return app.Dispatcher{
		Deliveries:  h.events,
		Events:      h.events,
		Sinks:       staticSource{"email": h.sink},
		Clock:       h.clock,
		Lease:       time.Minute,
		MaxAttempts: maxAttempts,
		Backoff:     domain.Backoff{Base: time.Second, Cap: time.Minute},
		Batch:       10,
	}
}

func (h *harness) deliveries(t *testing.T, f app.DeliveryFilter) []domain.Delivery {
	t.Helper()
	ds, err := h.events.ListDeliveries(h.ctx, f, 0)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	return ds
}

func (h *harness) delivery(t *testing.T, id domain.DeliveryID) domain.Delivery {
	t.Helper()
	d, ok, err := h.events.DeliveryByID(h.ctx, id)
	if err != nil || !ok {
		t.Fatalf("delivery %s ok=%v err=%v", id, ok, err)
	}
	return d
}

// saveEvent inserts one event and one email delivery directly, for tests that
// focus on the dispatcher rather than the submission surface.
func (h *harness) saveEvent(t *testing.T, id, key string) domain.Event {
	t.Helper()
	event, err := domain.NewEvent(domain.NewEventInput{
		ID:             domain.EventID(id),
		TenantID:       "ten_1",
		ProjectID:      "axis",
		AgentID:        "herdr",
		Status:         domain.StatusSuccess,
		Title:          "t",
		Body:           "b",
		IdempotencyKey: domain.IdempotencyKey(key),
		CreatedAt:      h.clock.Now(),
	})
	if err != nil {
		t.Fatalf("new event: %v", err)
	}
	d := domain.NewDelivery(domain.DeliveryID("del_"+id), event.ID, "email", h.clock.Now())
	if err := h.events.Save(h.ctx, event, []domain.Delivery{d}); err != nil {
		t.Fatalf("save event: %v", err)
	}
	return event
}

// TestIntegrationHTTPSubmitToSink is the end-to-end happy path: POST via the
// HTTP API, then a dispatcher drains the queue and the fake sink receives the
// event exactly once. A repeat of the same occurrence id is accepted but does
// not queue a second delivery.
func TestIntegrationHTTPSubmitToSink(t *testing.T) {
	h := newHarness(t)
	cl := client.Client{Gateway: h.server.URL, Token: h.token}

	accepted, err := cl.Submit(h.ctx, client.Event{
		Project: "axis", Status: "success", Title: "deploy", Body: "all green",
		Agent: "herdr", OccurrenceID: "deploy-2026-10-13",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if accepted.ID == "" {
		t.Fatal("submit returned an empty event id")
	}
	eventID := accepted.ID

	queued := h.deliveries(t, app.DeliveryFilter{SinkID: "email"})
	if len(queued) != 1 || queued[0].State != domain.DeliveryPending {
		t.Fatalf("queued = %+v, want one pending delivery", queued)
	}

	d := h.dispatcher(8)
	if n, err := d.ProcessBatch(h.ctx); err != nil || n != 1 {
		t.Fatalf("process batch n=%d err=%v", n, err)
	}
	if got := h.sink.calls(); got != 1 {
		t.Fatalf("sink calls = %d, want 1", got)
	}
	delivered := h.sink.events()
	if len(delivered) != 1 || delivered[0].ID != domain.EventID(eventID) {
		t.Fatalf("sink events = %+v", delivered)
	}
	if got := h.delivery(t, queued[0].ID); got.State != domain.DeliveryDelivered || got.Attempts != 1 {
		t.Fatalf("delivery = %+v, want delivered after 1 attempt", got)
	}

	// An idempotent retry returns the original id and fans out nothing new.
	again, err := cl.Submit(h.ctx, client.Event{
		Project: "axis", Status: "success", Title: "deploy", Body: "all green",
		Agent: "herdr", OccurrenceID: "deploy-2026-10-13",
	})
	if err != nil {
		t.Fatalf("retry submit: %v", err)
	}
	if again.ID != eventID {
		t.Fatalf("retry id = %q, want %q", again.ID, eventID)
	}
	if again.Deliveries != client.DeliveriesUnknown {
		t.Fatalf("retry deliveries = %d, want unknown", again.Deliveries)
	}
	if got := len(h.deliveries(t, app.DeliveryFilter{})); got != 1 {
		t.Fatalf("deliveries after retry = %d, want 1", got)
	}
}

// TestIntegrationCLISubmitToSink drives the same pipeline through `herald send`
// so the CLI contract (env-driven gateway/token, printed event id) is covered
// end to end.
func TestIntegrationCLISubmitToSink(t *testing.T) {
	h := newHarness(t)
	t.Setenv("HERALD_GATEWAY", h.server.URL)
	t.Setenv("HERALD_TOKEN", h.token)
	t.Setenv("HERALD_PROJECT", "axis")

	var stdout, stderr bytes.Buffer
	code := cli.Run(h.ctx,
		[]string{"send", "--status", "success", "--title", "cli done", "--occurrence-id", "cli-1"},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("herald send exit=%d stderr=%s", code, stderr.String())
	}
	eventID := strings.TrimSpace(stdout.String())
	if eventID == "" {
		t.Fatal("herald send printed no event id")
	}
	if got := len(h.deliveries(t, app.DeliveryFilter{})); got != 1 {
		t.Fatalf("queued deliveries = %d, want 1", got)
	}

	if n, err := h.dispatcher(8).ProcessBatch(h.ctx); err != nil || n != 1 {
		t.Fatalf("process batch n=%d err=%v", n, err)
	}
	if delivered := h.sink.events(); len(delivered) != 1 || delivered[0].ID != domain.EventID(eventID) {
		t.Fatalf("sink events = %+v, want the CLI event", delivered)
	}
}

// TestIntegrationRetryBackoffAndAtLeastOnce forces two retryable failures and
// shows the capped exponential backoff gating re-claims, then a third attempt
// succeeding. The sink sees the event more than once, which is the at-least-once
// contract.
func TestIntegrationRetryBackoffAndAtLeastOnce(t *testing.T) {
	h := newHarness(t)
	h.saveEvent(t, "evt_retry", "k-retry")
	h.sink.fail = func(call int, _ domain.Event) error {
		if call <= 2 {
			return errors.New("sink unavailable")
		}
		return nil
	}
	d := h.dispatcher(5)

	if n, err := d.ProcessBatch(h.ctx); err != nil || n != 1 {
		t.Fatalf("attempt 1 n=%d err=%v", n, err)
	}
	if got := h.sink.calls(); got != 1 {
		t.Fatalf("sink calls after 1 = %d", got)
	}
	row := h.deliveries(t, app.DeliveryFilter{SinkID: "email"})[0]
	if row.State != domain.DeliveryPending || row.Attempts != 1 {
		t.Fatalf("after 1 failure = %+v, want pending with 1 attempt", row)
	}
	if want := h.clock.Now().Add(time.Second); !row.NextAttemptAt.Equal(want) {
		t.Fatalf("next attempt = %v, want backoff to %v", row.NextAttemptAt, want)
	}

	// Not yet due: the backoff must hold the row back.
	if n, _ := d.ProcessBatch(h.ctx); n != 0 {
		t.Fatalf("claimed before backoff elapsed: %d", n)
	}

	h.clock.Advance(time.Second)
	if n, err := d.ProcessBatch(h.ctx); err != nil || n != 1 {
		t.Fatalf("attempt 2 n=%d err=%v", n, err)
	}
	if got := h.sink.calls(); got != 2 {
		t.Fatalf("sink calls after 2 = %d", got)
	}

	h.clock.Advance(2 * time.Second) // second backoff is Base*2
	if n, err := d.ProcessBatch(h.ctx); err != nil || n != 1 {
		t.Fatalf("attempt 3 n=%d err=%v", n, err)
	}
	if got := h.sink.calls(); got != 3 {
		t.Fatalf("sink calls = %d, want 3 (at-least-once retry)", got)
	}
	if got := h.deliveries(t, app.DeliveryFilter{})[0]; got.State != domain.DeliveryDelivered || got.Attempts != 3 {
		t.Fatalf("final delivery = %+v, want delivered after 3 attempts", got)
	}
}

// TestIntegrationPermanentFailureGoesDead shows a permanent sink error is not
// retried: one attempt, then dead.
func TestIntegrationPermanentFailureGoesDead(t *testing.T) {
	h := newHarness(t)
	h.saveEvent(t, "evt_perm", "k-perm")
	h.sink.fail = func(int, domain.Event) error {
		return app.Permanent(errors.New("recipient rejected"))
	}
	d := h.dispatcher(5)

	if n, err := d.ProcessBatch(h.ctx); err != nil || n != 1 {
		t.Fatalf("process batch n=%d err=%v", n, err)
	}
	row := h.deliveries(t, app.DeliveryFilter{SinkID: "email"})[0]
	if row.State != domain.DeliveryDead || row.Attempts != 1 {
		t.Fatalf("permanent failure = %+v, want dead after 1 attempt", row)
	}
	if !strings.Contains(row.LastError, "recipient rejected") {
		t.Fatalf("last error = %q", row.LastError)
	}
	if n, _ := d.ProcessBatch(h.ctx); n != 0 {
		t.Fatalf("dead delivery was retried: %d", n)
	}
	if got := h.sink.calls(); got != 1 {
		t.Fatalf("sink calls = %d, want 1", got)
	}
}

// TestIntegrationDeadLetterAfterMaxAttempts retries a persistently failing sink
// up to the attempt budget and lands the delivery in the dead state.
func TestIntegrationDeadLetterAfterMaxAttempts(t *testing.T) {
	h := newHarness(t)
	h.saveEvent(t, "evt_dead", "k-dead")
	h.sink.fail = func(int, domain.Event) error { return errors.New("still down") }
	const maxAttempts = 3
	d := h.dispatcher(maxAttempts)

	for i := 0; i < maxAttempts; i++ {
		if n, err := d.ProcessBatch(h.ctx); err != nil || n != 1 {
			t.Fatalf("attempt %d n=%d err=%v", i+1, n, err)
		}
		h.clock.Advance(10 * time.Second) // past any backoff
	}
	row := h.deliveries(t, app.DeliveryFilter{SinkID: "email"})[0]
	if row.State != domain.DeliveryDead || row.Attempts != maxAttempts {
		t.Fatalf("delivery = %+v, want dead after %d attempts", row, maxAttempts)
	}
	if got := h.sink.calls(); got != maxAttempts {
		t.Fatalf("sink calls = %d, want %d", got, maxAttempts)
	}
	if n, _ := d.ProcessBatch(h.ctx); n != 0 {
		t.Fatalf("dead-lettered delivery was claimed again: %d", n)
	}
}

// TestIntegrationLeaseReclaimAfterWorkerCrash claims a delivery and then
// "crashes" (never records an outcome). A second worker must leave the live
// lease alone, then pick the row back up once the lease expires, without the
// reclaim itself consuming an attempt.
func TestIntegrationLeaseReclaimAfterWorkerCrash(t *testing.T) {
	h := newHarness(t)
	h.saveEvent(t, "evt_crash", "k-crash")
	const lease = time.Minute

	claimed, err := h.events.Claim(h.ctx, h.clock.Now(), lease, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim n=%d err=%v", len(claimed), err)
	}
	if claimed[0].State != domain.DeliveryInflight || claimed[0].Attempts != 1 {
		t.Fatalf("claimed = %+v", claimed[0])
	}

	// The crashed worker's lease is still live, so a healthy worker ignores it.
	d := h.dispatcher(5)
	if n, _ := d.ProcessBatch(h.ctx); n != 0 {
		t.Fatalf("stole a live lease: %d", n)
	}
	if got := h.sink.calls(); got != 0 {
		t.Fatalf("sink called before recovery: %d", got)
	}

	h.clock.Advance(lease + time.Second)
	if n, err := d.ProcessBatch(h.ctx); err != nil || n != 1 {
		t.Fatalf("recovery batch n=%d err=%v", n, err)
	}
	row := h.deliveries(t, app.DeliveryFilter{SinkID: "email"})[0]
	if row.State != domain.DeliveryDelivered {
		t.Fatalf("recovered row = %+v, want delivered", row)
	}
	// Reclaim did not consume an attempt; the recovery claim counted one more.
	if row.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (1 crash + 1 recovery)", row.Attempts)
	}
	if got := h.sink.calls(); got != 1 {
		t.Fatalf("sink calls = %d, want 1", got)
	}
}

// TestIntegrationConcurrentDispatchersDeliverEachOnce stresses SELECT ... FOR
// UPDATE SKIP LOCKED: two dispatchers drain the same queue concurrently and no
// event may be delivered twice.
func TestIntegrationConcurrentDispatchersDeliverEachOnce(t *testing.T) {
	h := newHarness(t)
	const total = 12
	for i := 0; i < total; i++ {
		h.saveEvent(t, fmt.Sprintf("evt_%02d", i), fmt.Sprintf("k-%02d", i))
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := h.dispatcher(5)
			d.Batch = 3
			for i := 0; i < 1000; i++ {
				n, err := d.ProcessBatch(h.ctx)
				if err != nil {
					errCh <- err
					return
				}
				if n > 0 {
					continue
				}
				pending, err := h.events.ListDeliveries(h.ctx, app.DeliveryFilter{State: domain.DeliveryPending}, 0)
				if err != nil {
					errCh <- err
					return
				}
				if len(pending) == 0 {
					return
				}
			}
			errCh <- errors.New("dispatcher did not drain the queue")
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}

	if got := h.sink.calls(); got != total {
		t.Fatalf("sink calls = %d, want %d", got, total)
	}
	byEvent := h.sink.deliveriesByEvent()
	if len(byEvent) != total {
		t.Fatalf("distinct events delivered = %d, want %d", len(byEvent), total)
	}
	for id, n := range byEvent {
		if n != 1 {
			t.Fatalf("event %s delivered %d times, want 1", id, n)
		}
	}
	for _, row := range h.deliveries(t, app.DeliveryFilter{}) {
		if row.State != domain.DeliveryDelivered {
			t.Fatalf("delivery %s state = %s, want delivered", row.ID, row.State)
		}
	}
}

// TestIntegrationTokenCreateRequiresTenant pins the CLI contract: no --tenant
// fails, an explicit --tenant issues a project token against the real database.
func TestIntegrationTokenCreateRequiresTenant(t *testing.T) {
	url := requireDatabase(t)
	ctx := context.Background()

	var out, errOut bytes.Buffer
	code := cli.Run(ctx, []string{"token", "create", "--database-url", url, "--project", "axis"}, strings.NewReader(""), &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "tenant") {
		t.Fatalf("without --tenant: code=%d stderr=%q, want a tenant error", code, errOut.String())
	}

	out.Reset()
	errOut.Reset()
	code = cli.Run(ctx, []string{"token", "create", "--database-url", url, "--tenant", "ten_cli", "--project", "axis"}, strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("with --tenant: code=%d stderr=%q", code, errOut.String())
	}
	if got := strings.TrimSpace(out.String()); !strings.HasPrefix(got, "herald_") {
		t.Fatalf("token output = %q, want a herald_ project token", got)
	}
}

// TestIntegrationQueueDepthAndBackpressure proves the durable queue depth is
// scoped by (tenant, project) and that the submit limiter applies the Queued
// cap against it: a congested project is rejected with the queue scope while a
// different tenant sharing the same project slug keeps its own budget.
func TestIntegrationQueueDepthAndBackpressure(t *testing.T) {
	h := newHarness(t)
	h.saveEvent(t, "evt_q1", "q1")
	h.saveEvent(t, "evt_q2", "q2")

	if n, err := h.events.Outstanding(h.ctx, "ten_1", "axis"); err != nil || n != 2 {
		t.Fatalf("outstanding = %d, err %v, want 2", n, err)
	}
	if n, err := h.events.Outstanding(h.ctx, "ten_1", "finance"); err != nil || n != 0 {
		t.Fatalf("cross-project outstanding = %d, err %v, want 0", n, err)
	}
	if n, err := h.events.Outstanding(h.ctx, "ten_2", "axis"); err != nil || n != 0 {
		t.Fatalf("cross-tenant outstanding = %d, err %v, want 0", n, err)
	}
	if n, err := h.events.Backlog(h.ctx); err != nil || n != 2 {
		t.Fatalf("backlog = %d, err %v, want 2", n, err)
	}

	limiter := memory.NewRateLimiter(app.Limits{
		Requests: 1000, Window: time.Minute, Daily: 100000, Monthly: 100000, Queued: 1,
	}, h.clock, h.events)
	if d, err := limiter.Allow(h.ctx, "ten_1", "axis"); err != nil || d.Allowed || d.Scope != app.ScopeQueue {
		t.Fatalf("over-cap decision = %+v, err %v, want queue rejection", d, err)
	}
	if d, err := limiter.Allow(h.ctx, "ten_2", "axis"); err != nil || !d.Allowed {
		t.Fatalf("tenant B throttled by tenant A backlog: %+v, err %v", d, err)
	}

	if _, err := h.dispatcher(8).ProcessBatch(h.ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := h.events.Outstanding(h.ctx, "ten_1", "axis"); err != nil || n != 0 {
		t.Fatalf("outstanding after drain = %d, err %v, want 0", n, err)
	}
	if d, err := limiter.Allow(h.ctx, "ten_1", "axis"); err != nil || !d.Allowed {
		t.Fatalf("after drain blocked: %+v, err %v", d, err)
	}
}
