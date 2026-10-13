package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/domain"
)

type capturedDelivery struct {
	sink    domain.SinkID
	outcome DeliveryOutcome
}

type captureObserver struct {
	deliveries []capturedDelivery
	polls      int
}

func (o *captureObserver) ObserveHTTP(string, string, int, time.Duration) {}
func (o *captureObserver) ObserveDelivery(sink domain.SinkID, outcome DeliveryOutcome, _ time.Duration) {
	o.deliveries = append(o.deliveries, capturedDelivery{sink, outcome})
}
func (o *captureObserver) ObserveDispatchPoll(time.Time) { o.polls++ }

type countingHeartbeat struct{ beats int }

func (h *countingHeartbeat) Beat(time.Time) { h.beats++ }

func TestObserverRecordsOutcomes(t *testing.T) {
	events := newFakeEvents()
	seedEvent(t, events, "evt_1")
	deliveries := newFakeDeliveries(
		domain.NewDelivery("del_1", "evt_1", "email", testNow),
		domain.NewDelivery("del_2", "evt_1", "webhook", testNow),
		domain.NewDelivery("del_3", "evt_1", "slack", testNow),
	)
	obs := &captureObserver{}
	uc := newDispatcher(deliveries, events, map[domain.SinkID]Sink{
		"email":   &fakeSink{id: "email"},
		"webhook": &fakeSink{id: "webhook", err: Retryable(errors.New("503"))},
		"slack":   &fakeSink{id: "slack", err: Permanent(errors.New("404"))},
	})
	uc.Observer = obs

	if _, err := uc.ProcessBatch(context.Background()); err != nil {
		t.Fatal(err)
	}

	want := map[domain.SinkID]DeliveryOutcome{
		"email":   OutcomeDelivered,
		"webhook": OutcomeRetry,
		"slack":   OutcomeDead,
	}
	if len(obs.deliveries) != len(want) {
		t.Fatalf("observed %d deliveries, want %d", len(obs.deliveries), len(want))
	}
	for _, got := range obs.deliveries {
		if want[got.sink] != got.outcome {
			t.Fatalf("sink %q outcome = %q, want %q", got.sink, got.outcome, want[got.sink])
		}
	}
}

// A dispatcher with no explicit observer must not panic (the default is a
// no-op outside a process that links the metrics adapter).
func TestObserverDefaultsToNoop(t *testing.T) {
	if _, ok := newDispatcher(newFakeDeliveries(), newFakeEvents(), nil).observer().(NoopObserver); !ok {
		t.Fatal("default observer is not the no-op")
	}
}

func TestRunBeatsAndObservesPoll(t *testing.T) {
	obs := &captureObserver{}
	hb := &countingHeartbeat{}
	uc := newDispatcher(newFakeDeliveries(), newFakeEvents(), nil)
	uc.Observer = obs
	uc.Heartbeat = hb

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := uc.Run(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if obs.polls != 1 || hb.beats != 1 {
		t.Fatalf("polls=%d beats=%d, want 1 and 1", obs.polls, hb.beats)
	}
}

func TestRunDoesNotBeatOnClaimError(t *testing.T) {
	obs := &captureObserver{}
	hb := &countingHeartbeat{}
	uc := newDispatcher(&fakeDeliveries{claimErr: errors.New("db down")}, newFakeEvents(), nil)
	uc.Observer = obs
	uc.Heartbeat = hb

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = uc.Run(ctx, time.Hour)
	if obs.polls != 0 || hb.beats != 0 {
		t.Fatalf("polls=%d beats=%d, want 0 and 0", obs.polls, hb.beats)
	}
}
