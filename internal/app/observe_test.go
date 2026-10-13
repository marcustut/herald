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
	deliveries   []capturedDelivery
	polls        int
	rejects      []string
	depths       []int
	emailEvents  []string
	suppressions []string
	funnel       []string
	activations  []time.Duration
}

func (o *captureObserver) ObserveHTTP(string, string, int, time.Duration) {}
func (o *captureObserver) ObserveDelivery(sink domain.SinkID, outcome DeliveryOutcome, _ time.Duration) {
	o.deliveries = append(o.deliveries, capturedDelivery{sink, outcome})
}
func (o *captureObserver) ObserveDispatchPoll(time.Time)       { o.polls++ }
func (o *captureObserver) ObserveSubmitRejection(scope string) { o.rejects = append(o.rejects, scope) }
func (o *captureObserver) ObserveQueueDepth(depth int)         { o.depths = append(o.depths, depth) }
func (o *captureObserver) ObserveEmailEvent(kind string)       { o.emailEvents = append(o.emailEvents, kind) }
func (o *captureObserver) ObserveSuppression(reason string) {
	o.suppressions = append(o.suppressions, reason)
}
func (o *captureObserver) ObserveFunnel(stage string) { o.funnel = append(o.funnel, stage) }
func (o *captureObserver) ObserveActivation(took time.Duration) {
	o.activations = append(o.activations, took)
}

type countingHeartbeat struct{ beats int }

func (h *countingHeartbeat) Beat(time.Time) { h.beats++ }

// fakeActivation returns a fixed first-delivery answer.
type fakeActivation struct {
	answer DeliveryActivation
	err    error
}

func (f fakeActivation) Activation(context.Context, domain.TenantID) (DeliveryActivation, error) {
	return f.answer, f.err
}

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

// A hard recipient rejection dead-letters at once and is counted as a bounce,
// not a generic dead letter.
func TestObserverCountsBounce(t *testing.T) {
	events := newFakeEvents()
	seedEvent(t, events, "evt_1")
	deliveries := newFakeDeliveries(domain.NewDelivery("del_1", "evt_1", "email", testNow))
	obs := &captureObserver{}
	uc := newDispatcher(deliveries, events, map[domain.SinkID]Sink{
		"email": &fakeSink{id: "email", err: Bounce(errors.New("550 no such user"))},
	})
	uc.Observer = obs

	if _, err := uc.ProcessBatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := deliveries.row("del_1").State; got != domain.DeliveryDead {
		t.Fatalf("state = %q, want dead", got)
	}
	if len(obs.deliveries) != 1 || obs.deliveries[0].outcome != OutcomeBounce {
		t.Fatalf("observed = %+v, want one bounce", obs.deliveries)
	}
}

// A delivery refused because its address is suppressed dead-letters at once and
// is counted as a suppression, separate from a provider bounce.
func TestObserverCountsSuppressed(t *testing.T) {
	events := newFakeEvents()
	seedEvent(t, events, "evt_1")
	deliveries := newFakeDeliveries(domain.NewDelivery("del_1", "evt_1", "email", testNow))
	obs := &captureObserver{}
	uc := newDispatcher(deliveries, events, map[domain.SinkID]Sink{
		"email": &fakeSink{id: "email", err: Suppressed(errors.New("recipient is on the suppression list"))},
	})
	uc.Observer = obs

	if _, err := uc.ProcessBatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := deliveries.row("del_1").State; got != domain.DeliveryDead {
		t.Fatalf("state = %q, want dead", got)
	}
	if len(obs.deliveries) != 1 || obs.deliveries[0].outcome != OutcomeSuppressed {
		t.Fatalf("observed = %+v, want one suppression", obs.deliveries)
	}
}

// Run samples the queue backlog for congestion metrics on each successful poll.
func TestRunObservesBacklog(t *testing.T) {
	deliveries := newFakeDeliveries()
	deliveries.backlog = 7
	obs := &captureObserver{}
	uc := newDispatcher(deliveries, newFakeEvents(), nil)
	uc.Observer = obs

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = uc.Run(ctx, time.Hour)
	if len(obs.depths) != 1 || obs.depths[0] != 7 {
		t.Fatalf("depths = %v, want [7]", obs.depths)
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

// The first delivered event for an account records the first-delivery stage and
// the account-created -> first-delivered setup time.
func TestObserverRecordsFirstDeliveryFunnel(t *testing.T) {
	events := newFakeEvents()
	seedEvent(t, events, "evt_1")
	deliveries := newFakeDeliveries(domain.NewDelivery("del_1", "evt_1", "email", testNow))
	obs := &captureObserver{}
	uc := newDispatcher(deliveries, events, map[domain.SinkID]Sink{"email": &fakeSink{id: "email"}})
	uc.Observer = obs
	uc.Activation = fakeActivation{answer: DeliveryActivation{
		Known: true, First: true, AccountCreatedAt: testNow.Add(-30 * time.Minute),
	}}

	if _, err := uc.ProcessBatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(obs.funnel) != 1 || obs.funnel[0] != string(FunnelFirstDelivered) {
		t.Fatalf("funnel = %v, want [first_delivered]", obs.funnel)
	}
	if len(obs.activations) != 1 || obs.activations[0] != 30*time.Minute {
		t.Fatalf("activations = %v, want [30m]", obs.activations)
	}
}

// A later delivered event records repeat_send and no setup time.
func TestObserverRecordsRepeatSendFunnel(t *testing.T) {
	events := newFakeEvents()
	seedEvent(t, events, "evt_1")
	deliveries := newFakeDeliveries(domain.NewDelivery("del_1", "evt_1", "email", testNow))
	obs := &captureObserver{}
	uc := newDispatcher(deliveries, events, map[domain.SinkID]Sink{"email": &fakeSink{id: "email"}})
	uc.Observer = obs
	uc.Activation = fakeActivation{answer: DeliveryActivation{Known: true, First: false, AccountCreatedAt: testNow}}

	if _, err := uc.ProcessBatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(obs.funnel) != 1 || obs.funnel[0] != string(FunnelRepeatSend) {
		t.Fatalf("funnel = %v, want [repeat_send]", obs.funnel)
	}
	if len(obs.activations) != 0 {
		t.Fatalf("activations = %v, want none", obs.activations)
	}
}

// A self-host tenant (no cloud account) records nothing.
func TestObserverSkipsUnknownTenantFunnel(t *testing.T) {
	events := newFakeEvents()
	seedEvent(t, events, "evt_1")
	deliveries := newFakeDeliveries(domain.NewDelivery("del_1", "evt_1", "email", testNow))
	obs := &captureObserver{}
	uc := newDispatcher(deliveries, events, map[domain.SinkID]Sink{"email": &fakeSink{id: "email"}})
	uc.Observer = obs
	uc.Activation = fakeActivation{answer: DeliveryActivation{Known: false}}

	if _, err := uc.ProcessBatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(obs.funnel) != 0 {
		t.Fatalf("funnel = %v, want none", obs.funnel)
	}
}
