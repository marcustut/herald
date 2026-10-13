package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/marcustut/herald/internal/domain"
)

var submitter = domain.Principal{TenantID: "ten_1", ProjectID: "axis"}

func validSubmitRequest() SubmitRequest {
	return SubmitRequest{
		Status:         "success",
		Title:          "deploy finished",
		Body:           "all green",
		Agent:          "herdr-build",
		IdempotencyKey: "run-1",
	}
}

func newSubmit() (SubmitEvent, *fakeEvents, *fakeSinks) {
	events := newFakeEvents()
	sinks := &fakeSinks{sinks: []domain.SinkID{"email"}}
	uc := SubmitEvent{Events: events, Sinks: sinks, IDs: &fakeIDs{}, Clock: fakeClock{testNow}}
	return uc, events, sinks
}

func TestSubmitHappyPath(t *testing.T) {
	uc, events, _ := newSubmit()
	id, err := uc.Handle(context.Background(), submitter, validSubmitRequest())
	if err != nil {
		t.Fatal(err)
	}
	if id != "evt_1" {
		t.Fatalf("id = %q", id)
	}
	rep, ok := events.byKey[eventKey{"ten_1", "axis", "run-1"}]
	if !ok {
		t.Fatal("event not saved")
	}
	if rep.TenantID != "ten_1" || rep.ProjectID != "axis" || rep.AgentID != "herdr-build" {
		t.Fatalf("event = %+v", rep)
	}
	if len(events.deliveries) != 1 || events.deliveries[0].SinkID != "email" || events.deliveries[0].EventID != "evt_1" {
		t.Fatalf("deliveries = %+v", events.deliveries)
	}
}

func TestSubmitRedactsBeforePersistence(t *testing.T) {
	uc, events, _ := newSubmit()
	req := validSubmitRequest()
	req.Body = "password=hunter2"
	if _, err := uc.Handle(context.Background(), submitter, req); err != nil {
		t.Fatal(err)
	}
	rep := events.byKey[eventKey{"ten_1", "axis", "run-1"}]
	if strings.Contains(rep.Body, "hunter2") {
		t.Fatalf("secret persisted: %q", rep.Body)
	}
	if !strings.Contains(rep.Body, domain.RedactionMarker) {
		t.Fatalf("expected marker: %q", rep.Body)
	}
}

func TestSubmitWithLinks(t *testing.T) {
	uc, events, _ := newSubmit()
	req := validSubmitRequest()
	req.Links = []LinkInput{{Label: "PR", URL: "https://github.com/x/y/pull/1"}}
	if _, err := uc.Handle(context.Background(), submitter, req); err != nil {
		t.Fatal(err)
	}
	rep := events.byKey[eventKey{"ten_1", "axis", "run-1"}]
	if len(rep.Links) != 1 || rep.Links[0].Label != "PR" {
		t.Fatalf("links = %+v", rep.Links)
	}
}

func TestSubmitIdempotentReplay(t *testing.T) {
	uc, events, _ := newSubmit()
	first, err := uc.Handle(context.Background(), submitter, validSubmitRequest())
	if err != nil {
		t.Fatal(err)
	}
	second, err := uc.Handle(context.Background(), submitter, validSubmitRequest())
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("first %q, second %q", first, second)
	}
	if len(events.byKey) != 1 {
		t.Fatalf("saved %d events", len(events.byKey))
	}
	if len(events.deliveries) != 1 {
		t.Fatalf("deliveries = %d, want no re-fan-out", len(events.deliveries))
	}
}

func TestSubmitIdenticalContentAreDistinctOccurrences(t *testing.T) {
	uc, events, _ := newSubmit()
	req := validSubmitRequest()
	req.IdempotencyKey = ""
	first, err := uc.Handle(context.Background(), submitter, req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := uc.Handle(context.Background(), submitter, req)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("identical content collapsed to one event: %q", first)
	}
	if len(events.byKey) != 2 || len(events.deliveries) != 2 {
		t.Fatalf("events=%d deliveries=%d, want 2 each", len(events.byKey), len(events.deliveries))
	}
}

func TestSubmitOccurrenceRetry(t *testing.T) {
	uc, events, _ := newSubmit()
	req := validSubmitRequest()
	req.IdempotencyKey = ""
	req.OccurrenceID = "deploy-42"
	first, err := uc.Handle(context.Background(), submitter, req)
	if err != nil {
		t.Fatal(err)
	}
	req.Body = "retry with changed text"
	second, err := uc.Handle(context.Background(), submitter, req)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("retry id = %q, want %q", second, first)
	}
	if len(events.byKey) != 1 || len(events.deliveries) != 1 {
		t.Fatalf("events=%d deliveries=%d, want no re-fan-out", len(events.byKey), len(events.deliveries))
	}
	if _, ok := events.byKey[eventKey{"ten_1", "axis", "occ_deploy-42"}]; !ok {
		t.Fatalf("occurrence key not stored: %+v", events.byKey)
	}
}

func TestSubmitKeysAreProjectScoped(t *testing.T) {
	uc, events, _ := newSubmit()
	req := validSubmitRequest()
	req.OccurrenceID = "shared-1"
	axis := domain.Principal{TenantID: "ten_1", ProjectID: "axis"}
	herald := domain.Principal{TenantID: "ten_1", ProjectID: "herald"}
	first, err := uc.Handle(context.Background(), axis, req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := uc.Handle(context.Background(), herald, req)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("the same key across two projects collapsed into one event")
	}
	if len(events.byKey) != 2 {
		t.Fatalf("saved %d events, want 2", len(events.byKey))
	}
}

func TestSubmitIdempotencyRace(t *testing.T) {
	racing := &racingEvents{existing: domain.Event{ID: "evt_existing"}}
	uc := SubmitEvent{Events: racing, Sinks: &fakeSinks{sinks: []domain.SinkID{"email"}}, IDs: &fakeIDs{}, Clock: fakeClock{testNow}}
	id, err := uc.Handle(context.Background(), submitter, validSubmitRequest())
	if err != nil {
		t.Fatal(err)
	}
	if id != "evt_existing" {
		t.Fatalf("id = %q", id)
	}
}

func TestSubmitIdempotencyRaceLookupError(t *testing.T) {
	boom := errors.New("lookup after conflict failed")
	racing := &racingEvents{lookupErr: boom}
	uc := SubmitEvent{Events: racing, Sinks: &fakeSinks{sinks: []domain.SinkID{"email"}}, IDs: &fakeIDs{}, Clock: fakeClock{testNow}}
	if _, err := uc.Handle(context.Background(), submitter, validSubmitRequest()); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestSubmitNoSinks(t *testing.T) {
	uc, events, _ := newSubmit()
	uc.Sinks = &fakeSinks{}
	if _, err := uc.Handle(context.Background(), submitter, validSubmitRequest()); err != nil {
		t.Fatal(err)
	}
	if len(events.deliveries) != 0 {
		t.Fatalf("deliveries = %d", len(events.deliveries))
	}
	if len(events.byKey) != 1 {
		t.Fatal("event not saved")
	}
}

func TestSubmitValidationErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*SubmitRequest)
	}{
		{"bad status", func(r *SubmitRequest) { r.Status = "done" }},
		{"missing agent", func(r *SubmitRequest) { r.Agent = "" }},
		{"bad occurrence id", func(r *SubmitRequest) { r.OccurrenceID = "a/b" }},
		{"bad idempotency key", func(r *SubmitRequest) { r.IdempotencyKey = "a b" }},
		{"bad link", func(r *SubmitRequest) { r.Links = []LinkInput{{Label: "", URL: "https://x.test"}} }},
		{"bad context", func(r *SubmitRequest) { r.Context = map[string]string{"agent": "spoof"} }},
		{"long title", func(r *SubmitRequest) { r.Title = strings.Repeat("a", domain.MaxTitleLen+1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uc, events, _ := newSubmit()
			req := validSubmitRequest()
			tt.mutate(&req)
			if _, err := uc.Handle(context.Background(), submitter, req); err == nil {
				t.Fatal("expected error")
			}
			if len(events.byKey) != 0 {
				t.Fatal("nothing should be saved")
			}
		})
	}
}

func TestSubmitSinkError(t *testing.T) {
	uc, events, sinks := newSubmit()
	boom := errors.New("sink lookup failed")
	sinks.err = boom
	if _, err := uc.Handle(context.Background(), submitter, validSubmitRequest()); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if len(events.byKey) != 0 {
		t.Fatal("event should not be saved")
	}
}

func TestSubmitLookupError(t *testing.T) {
	uc, events, _ := newSubmit()
	boom := errors.New("store down")
	events.lookupErr = boom
	if _, err := uc.Handle(context.Background(), submitter, validSubmitRequest()); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestSubmitSaveError(t *testing.T) {
	uc, events, _ := newSubmit()
	boom := errors.New("write failed")
	events.saveErr = boom
	if _, err := uc.Handle(context.Background(), submitter, validSubmitRequest()); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

type racingEvents struct {
	existing  domain.Event
	saved     bool
	lookupErr error
}

func (r *racingEvents) ByIdempotencyKey(context.Context, domain.TenantID, domain.ProjectID, domain.IdempotencyKey) (domain.Event, bool, error) {
	if !r.saved {
		return domain.Event{}, false, nil
	}
	if r.lookupErr != nil {
		return domain.Event{}, false, r.lookupErr
	}
	return r.existing, true, nil
}

func (r *racingEvents) Save(context.Context, domain.Event, []domain.Delivery) error {
	r.saved = true
	return ErrIdempotencyConflict
}

func (r *racingEvents) ByID(context.Context, domain.EventID) (domain.Event, bool, error) {
	return domain.Event{}, false, nil
}
