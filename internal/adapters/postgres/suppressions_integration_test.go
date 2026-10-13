package postgres

import (
	"sync"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/app"
)

// TestSuppressionsIngest proves the suppression store applies a permanent
// bounce and a complaint, records a transient bounce without suppressing, and
// is idempotent by provider event id.
func TestSuppressionsIngest(t *testing.T) {
	pool, ctx := testPool(t)
	resetSchema(t, ctx, pool)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewSuppressions(pool)
	now := time.Now().UTC()

	permanent := app.ProviderEvent{
		ID: "evt-1:gone@example.com", Kind: app.EmailEventBounce,
		Address: "gone@example.com", Permanent: true, ReceivedAt: now,
	}
	recorded, err := store.IngestProviderEvent(ctx, permanent)
	if err != nil || !recorded {
		t.Fatalf("permanent bounce recorded=%v err=%v", recorded, err)
	}
	if ok, err := store.Suppressed(ctx, "gone@example.com"); err != nil || !ok {
		t.Fatalf("gone@example.com suppressed=%v err=%v", ok, err)
	}

	// Replaying the same event id applies nothing and reports recorded=false.
	recorded, err = store.IngestProviderEvent(ctx, permanent)
	if err != nil || recorded {
		t.Fatalf("replay recorded=%v err=%v, want false", recorded, err)
	}

	// A transient bounce is recorded but never suppresses.
	recorded, err = store.IngestProviderEvent(ctx, app.ProviderEvent{
		ID: "evt-2:full@example.com", Kind: app.EmailEventBounce,
		Address: "full@example.com", Permanent: false, ReceivedAt: now,
	})
	if err != nil || !recorded {
		t.Fatalf("transient recorded=%v err=%v", recorded, err)
	}
	if ok, _ := store.Suppressed(ctx, "full@example.com"); ok {
		t.Fatal("transient bounce suppressed the address")
	}

	// A complaint always suppresses.
	if _, err := store.IngestProviderEvent(ctx, app.ProviderEvent{
		ID: "evt-3:spam@example.com", Kind: app.EmailEventComplaint,
		Address: "spam@example.com", ReceivedAt: now,
	}); err != nil {
		t.Fatalf("complaint: %v", err)
	}
	if ok, _ := store.Suppressed(ctx, "spam@example.com"); !ok {
		t.Fatal("complaint did not suppress")
	}

	// An address never seen is not suppressed.
	if ok, _ := store.Suppressed(ctx, "unknown@example.com"); ok {
		t.Fatal("unknown address reported suppressed")
	}
}

// TestSuppressionsIngestConcurrent drives the same event id from many workers:
// exactly one insert wins, so a replay storm cannot apply the suppression twice
// or double-count it.
func TestSuppressionsIngestConcurrent(t *testing.T) {
	pool, ctx := testPool(t)
	resetSchema(t, ctx, pool)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewSuppressions(pool)
	now := time.Now().UTC()
	ev := app.ProviderEvent{
		ID: "evt-shared:dup@example.com", Kind: app.EmailEventComplaint,
		Address: "dup@example.com", ReceivedAt: now,
	}

	const n = 8
	var wg sync.WaitGroup
	recorded := make(chan bool, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := store.IngestProviderEvent(ctx, ev)
			recorded <- ok
			errs <- err
		}()
	}
	wg.Wait()
	close(recorded)
	close(errs)

	wins := 0
	for err := range errs {
		if err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	for ok := range recorded {
		if ok {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("recorded winners = %d, want 1", wins)
	}
}
