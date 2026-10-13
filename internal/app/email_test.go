package app

import (
	"context"
	"errors"
	"testing"
)

// fakeSuppressions is an in-memory app.SuppressionStore. It records the address
// and reason a suppression was applied with, and dedupes by event id, mirroring
// the SQL store's transaction.
type fakeSuppressions struct {
	suppressed map[string]string
	seen       map[string]bool
	err        error
}

func newFakeSuppressions() *fakeSuppressions {
	return &fakeSuppressions{suppressed: map[string]string{}, seen: map[string]bool{}}
}

func (s *fakeSuppressions) Suppressed(_ context.Context, address string) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	_, ok := s.suppressed[address]
	return ok, nil
}

func (s *fakeSuppressions) IngestProviderEvent(_ context.Context, e ProviderEvent) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	if s.seen[e.ID] {
		return false, nil
	}
	s.seen[e.ID] = true
	if e.Suppresses() {
		s.suppressed[e.Address] = e.Reason()
	}
	return true, nil
}

func TestEmailEventsIngestsPermanentBounce(t *testing.T) {
	store := newFakeSuppressions()
	obs := &captureObserver{}
	uc := EmailEvents{Suppressions: store, Clock: fakeClock{testNow}, Observer: obs}

	recorded, err := uc.Ingest(context.Background(), ProviderEvent{
		ID: "e1", Kind: EmailEventBounce, Address: "Ops@Example.COM", Permanent: true,
	})
	if err != nil || !recorded {
		t.Fatalf("recorded=%v err=%v", recorded, err)
	}
	// The address is canonicalized (domain lowered) before it is stored.
	if reason, ok := store.suppressed["Ops@example.com"]; !ok || reason != "bounce" {
		t.Fatalf("suppressed = %v, want bounce for Ops@example.com", store.suppressed)
	}
	if len(obs.emailEvents) != 1 || obs.emailEvents[0] != "bounce" {
		t.Fatalf("email events = %v", obs.emailEvents)
	}
	if len(obs.suppressions) != 1 || obs.suppressions[0] != "bounce" {
		t.Fatalf("suppressions = %v", obs.suppressions)
	}
}

// A transient bounce is recorded for the audit log but does not suppress the
// address: the mailbox may still be deliverable after a retry.
func TestEmailEventsTransientBounceDoesNotSuppress(t *testing.T) {
	store := newFakeSuppressions()
	obs := &captureObserver{}
	uc := EmailEvents{Suppressions: store, Clock: fakeClock{testNow}, Observer: obs}

	recorded, err := uc.Ingest(context.Background(), ProviderEvent{
		ID: "e2", Kind: EmailEventBounce, Address: "a@b.example", Permanent: false,
	})
	if err != nil || !recorded {
		t.Fatalf("recorded=%v err=%v", recorded, err)
	}
	if _, ok := store.suppressed["a@b.example"]; ok {
		t.Fatal("transient bounce suppressed the address")
	}
	if len(obs.suppressions) != 0 {
		t.Fatalf("suppressions = %v, want none", obs.suppressions)
	}
}

func TestEmailEventsComplaintSuppresses(t *testing.T) {
	store := newFakeSuppressions()
	obs := &captureObserver{}
	uc := EmailEvents{Suppressions: store, Clock: fakeClock{testNow}, Observer: obs}

	if _, err := uc.Ingest(context.Background(), ProviderEvent{
		ID: "e3", Kind: EmailEventComplaint, Address: "a@b.example",
	}); err != nil {
		t.Fatal(err)
	}
	if reason := store.suppressed["a@b.example"]; reason != "complaint" {
		t.Fatalf("reason = %q, want complaint", reason)
	}
	if len(obs.suppressions) != 1 || obs.suppressions[0] != "complaint" {
		t.Fatalf("suppressions = %v", obs.suppressions)
	}
}

// A replayed provider event is a no-op: it applies no second suppression and is
// not counted twice.
func TestEmailEventsReplayIsIdempotent(t *testing.T) {
	store := newFakeSuppressions()
	obs := &captureObserver{}
	uc := EmailEvents{Suppressions: store, Clock: fakeClock{testNow}, Observer: obs}
	ev := ProviderEvent{ID: "e4", Kind: EmailEventComplaint, Address: "a@b.example"}

	if _, err := uc.Ingest(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	recorded, err := uc.Ingest(context.Background(), ev)
	if err != nil || recorded {
		t.Fatalf("replay recorded=%v err=%v, want false", recorded, err)
	}
	if len(obs.emailEvents) != 1 || len(obs.suppressions) != 1 {
		t.Fatalf("replay doubled metrics: events=%v suppressions=%v", obs.emailEvents, obs.suppressions)
	}
}

// One notification about several recipients seeds one idempotency key each, so
// a replay dedupes per (event, address) and does not let the first recipient's
// key swallow the rest.
func TestEmailEventsScopesEventIDByAddress(t *testing.T) {
	store := newFakeSuppressions()
	uc := EmailEvents{Suppressions: store, Clock: fakeClock{testNow}, Observer: &captureObserver{}}

	for _, addr := range []string{"a@b.example", "c@d.example"} {
		recorded, err := uc.Ingest(context.Background(), ProviderEvent{
			ID: "shared", Kind: EmailEventBounce, Address: addr, Permanent: true,
		})
		if err != nil || !recorded {
			t.Fatalf("%s recorded=%v err=%v", addr, recorded, err)
		}
	}
	if len(store.suppressed) != 2 {
		t.Fatalf("suppressed = %v, want both recipients", store.suppressed)
	}
}

func TestEmailEventsRejectsInvalidInput(t *testing.T) {
	store := newFakeSuppressions()
	uc := EmailEvents{Suppressions: store, Clock: fakeClock{testNow}}
	for _, ev := range []ProviderEvent{
		{ID: "x", Kind: "unknown", Address: "a@b.example"},
		{ID: "y", Kind: EmailEventBounce, Address: "not an email"},
	} {
		if _, err := uc.Ingest(context.Background(), ev); !errors.Is(err, ErrInvalidEmailEvent) {
			t.Fatalf("Ingest(%+v) err = %v, want ErrInvalidEmailEvent", ev, err)
		}
	}
}
