package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/domain"
)

func seedReport(t *testing.T, reports *fakeReports, id domain.ReportID) {
	t.Helper()
	rep := domain.Report{
		ID:             id,
		TenantID:       "ten_1",
		ProjectID:      "axis",
		AgentID:        "herdr",
		Status:         domain.StatusSuccess,
		Title:          "t",
		IdempotencyKey: domain.IdempotencyKey("k-" + string(id)),
		CreatedAt:      testNow,
	}
	if err := reports.Save(context.Background(), rep, nil); err != nil {
		t.Fatal(err)
	}
}

func newDispatcher(deliveries *fakeDeliveries, reports *fakeReports, sinks map[domain.SinkID]Sink) Dispatcher {
	return Dispatcher{
		Deliveries:  deliveries,
		Reports:     reports,
		Sinks:       sinks,
		Clock:       fakeClock{testNow},
		Lease:       time.Minute,
		MaxAttempts: 3,
		Backoff:     domain.Backoff{Base: time.Second, Cap: time.Minute},
		Batch:       10,
	}
}

func TestDispatchDelivers(t *testing.T) {
	reports := newFakeReports()
	seedReport(t, reports, "rep_1")
	deliveries := newFakeDeliveries(domain.NewDelivery("del_1", "rep_1", "email", testNow))
	sink := &fakeSink{id: "email"}
	uc := newDispatcher(deliveries, reports, map[domain.SinkID]Sink{"email": sink})

	n, err := uc.ProcessBatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || sink.calls != 1 {
		t.Fatalf("n=%d calls=%d", n, sink.calls)
	}
	if sink.got.ID != "rep_1" {
		t.Fatalf("sink got %q", sink.got.ID)
	}
	if got := deliveries.row("del_1").State; got != domain.DeliveryDelivered {
		t.Fatalf("state = %q", got)
	}
}

func TestDispatchRetryableFailure(t *testing.T) {
	reports := newFakeReports()
	seedReport(t, reports, "rep_1")
	deliveries := newFakeDeliveries(domain.NewDelivery("del_1", "rep_1", "email", testNow))
	sink := &fakeSink{id: "email", err: Retryable(errors.New("503"))}
	uc := newDispatcher(deliveries, reports, map[domain.SinkID]Sink{"email": sink})

	if _, err := uc.ProcessBatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := deliveries.row("del_1")
	if got.State != domain.DeliveryPending || got.Attempts != 1 {
		t.Fatalf("got %+v", got)
	}
	if !got.NextAttemptAt.After(testNow) {
		t.Fatalf("next attempt not scheduled: %v", got.NextAttemptAt)
	}
	if got.LastError == "" {
		t.Fatal("last error not recorded")
	}
}

func TestDispatchPermanentFailure(t *testing.T) {
	reports := newFakeReports()
	seedReport(t, reports, "rep_1")
	deliveries := newFakeDeliveries(domain.NewDelivery("del_1", "rep_1", "email", testNow))
	sink := &fakeSink{id: "email", err: Permanent(errors.New("404"))}
	uc := newDispatcher(deliveries, reports, map[domain.SinkID]Sink{"email": sink})

	if _, err := uc.ProcessBatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := deliveries.row("del_1").State; got != domain.DeliveryDead {
		t.Fatalf("state = %q", got)
	}
}

func TestDispatchExhaustsAttempts(t *testing.T) {
	reports := newFakeReports()
	seedReport(t, reports, "rep_1")
	deliveries := newFakeDeliveries(domain.NewDelivery("del_1", "rep_1", "email", testNow))
	sink := &fakeSink{id: "email", err: Retryable(errors.New("503"))}
	uc := newDispatcher(deliveries, reports, map[domain.SinkID]Sink{"email": sink})
	uc.MaxAttempts = 1

	if _, err := uc.ProcessBatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := deliveries.row("del_1").State; got != domain.DeliveryDead {
		t.Fatalf("state = %q", got)
	}
}

func TestDispatchUnknownSink(t *testing.T) {
	reports := newFakeReports()
	seedReport(t, reports, "rep_1")
	deliveries := newFakeDeliveries(domain.NewDelivery("del_1", "rep_1", "nope", testNow))
	uc := newDispatcher(deliveries, reports, nil)

	if _, err := uc.ProcessBatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := deliveries.row("del_1").State; got != domain.DeliveryDead {
		t.Fatalf("state = %q", got)
	}
}

func TestDispatchMissingReport(t *testing.T) {
	deliveries := newFakeDeliveries(domain.NewDelivery("del_1", "rep_missing", "email", testNow))
	uc := newDispatcher(deliveries, newFakeReports(), map[domain.SinkID]Sink{"email": &fakeSink{id: "email"}})

	if _, err := uc.ProcessBatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := deliveries.row("del_1").State; got != domain.DeliveryDead {
		t.Fatalf("state = %q", got)
	}
}

func TestDispatchCanceledContextIsNotRetried(t *testing.T) {
	reports := newFakeReports()
	seedReport(t, reports, "rep_1")
	deliveries := newFakeDeliveries(domain.NewDelivery("del_1", "rep_1", "email", testNow))
	sink := &fakeSink{id: "email", err: Retryable(context.Canceled)}
	uc := newDispatcher(deliveries, reports, map[domain.SinkID]Sink{"email": sink})

	if _, err := uc.ProcessBatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := deliveries.row("del_1").State; got != domain.DeliveryDead {
		t.Fatalf("state = %q, want dead", got)
	}
}

func TestDispatchErrors(t *testing.T) {
	boom := errors.New("boom")

	claimErr := newDispatcher(&fakeDeliveries{claimErr: boom}, newFakeReports(), nil)
	if _, err := claimErr.ProcessBatch(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("claim err = %v", err)
	}

	reports := newFakeReports()
	seedReport(t, reports, "rep_1")
	reports.lookupErr = boom
	lookupErr := newDispatcher(newFakeDeliveries(domain.NewDelivery("del_1", "rep_1", "email", testNow)), reports, map[domain.SinkID]Sink{"email": &fakeSink{id: "email"}})
	if _, err := lookupErr.ProcessBatch(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("lookup err = %v", err)
	}

	reports2 := newFakeReports()
	seedReport(t, reports2, "rep_1")
	deliveries := newFakeDeliveries(domain.NewDelivery("del_1", "rep_1", "email", testNow))
	deliveries.updateErr = boom
	updateErr := newDispatcher(deliveries, reports2, map[domain.SinkID]Sink{"email": &fakeSink{id: "email"}})
	if _, err := updateErr.ProcessBatch(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("update err = %v", err)
	}
}

func TestDispatchRunStopsOnCancel(t *testing.T) {
	uc := newDispatcher(newFakeDeliveries(), newFakeReports(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := uc.Run(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestSinkErrorClassification(t *testing.T) {
	retryable := Retryable(errors.New("x"))
	if IsPermanent(retryable) {
		t.Fatal("retryable classified permanent")
	}
	if Retryable(errors.New("x")).Error() != "x" {
		t.Fatal("message lost")
	}
	permanent := Permanent(errors.New("y"))
	if !IsPermanent(permanent) {
		t.Fatal("permanent not detected")
	}
	cause := errors.New("y")
	if !errors.Is(Permanent(cause), cause) {
		t.Fatal("unwrap broken")
	}
}
