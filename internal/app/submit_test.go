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

func newSubmit() (SubmitReport, *fakeReports, *fakeSinks) {
	reports := newFakeReports()
	sinks := &fakeSinks{sinks: []domain.SinkID{"email"}}
	uc := SubmitReport{Reports: reports, Sinks: sinks, IDs: &fakeIDs{}, Clock: fakeClock{testNow}}
	return uc, reports, sinks
}

func TestSubmitHappyPath(t *testing.T) {
	uc, reports, _ := newSubmit()
	id, err := uc.Handle(context.Background(), submitter, validSubmitRequest())
	if err != nil {
		t.Fatal(err)
	}
	if id != "rep_1" {
		t.Fatalf("id = %q", id)
	}
	rep, ok := reports.byKey[reportKey{"ten_1", "run-1"}]
	if !ok {
		t.Fatal("report not saved")
	}
	if rep.TenantID != "ten_1" || rep.ProjectID != "axis" || rep.AgentID != "herdr-build" {
		t.Fatalf("report = %+v", rep)
	}
	if len(reports.deliveries) != 1 || reports.deliveries[0].SinkID != "email" || reports.deliveries[0].ReportID != "rep_1" {
		t.Fatalf("deliveries = %+v", reports.deliveries)
	}
}

func TestSubmitRedactsBeforePersistence(t *testing.T) {
	uc, reports, _ := newSubmit()
	req := validSubmitRequest()
	req.Body = "password=hunter2"
	if _, err := uc.Handle(context.Background(), submitter, req); err != nil {
		t.Fatal(err)
	}
	rep := reports.byKey[reportKey{"ten_1", "run-1"}]
	if strings.Contains(rep.Body, "hunter2") {
		t.Fatalf("secret persisted: %q", rep.Body)
	}
	if !strings.Contains(rep.Body, domain.RedactionMarker) {
		t.Fatalf("expected marker: %q", rep.Body)
	}
}

func TestSubmitWithLinks(t *testing.T) {
	uc, reports, _ := newSubmit()
	req := validSubmitRequest()
	req.Links = []LinkInput{{Label: "PR", URL: "https://github.com/x/y/pull/1"}}
	if _, err := uc.Handle(context.Background(), submitter, req); err != nil {
		t.Fatal(err)
	}
	rep := reports.byKey[reportKey{"ten_1", "run-1"}]
	if len(rep.Links) != 1 || rep.Links[0].Label != "PR" {
		t.Fatalf("links = %+v", rep.Links)
	}
}

func TestSubmitIdempotentReplay(t *testing.T) {
	uc, reports, _ := newSubmit()
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
	if len(reports.byKey) != 1 {
		t.Fatalf("saved %d reports", len(reports.byKey))
	}
	if len(reports.deliveries) != 1 {
		t.Fatalf("deliveries = %d, want no re-fan-out", len(reports.deliveries))
	}
}

func TestSubmitIdempotencyRace(t *testing.T) {
	racing := &racingReports{existing: domain.Report{ID: "rep_existing"}}
	uc := SubmitReport{Reports: racing, Sinks: &fakeSinks{sinks: []domain.SinkID{"email"}}, IDs: &fakeIDs{}, Clock: fakeClock{testNow}}
	id, err := uc.Handle(context.Background(), submitter, validSubmitRequest())
	if err != nil {
		t.Fatal(err)
	}
	if id != "rep_existing" {
		t.Fatalf("id = %q", id)
	}
}

func TestSubmitIdempotencyRaceLookupError(t *testing.T) {
	boom := errors.New("lookup after conflict failed")
	racing := &racingReports{lookupErr: boom}
	uc := SubmitReport{Reports: racing, Sinks: &fakeSinks{sinks: []domain.SinkID{"email"}}, IDs: &fakeIDs{}, Clock: fakeClock{testNow}}
	if _, err := uc.Handle(context.Background(), submitter, validSubmitRequest()); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestSubmitNoSinks(t *testing.T) {
	uc, reports, _ := newSubmit()
	uc.Sinks = &fakeSinks{}
	if _, err := uc.Handle(context.Background(), submitter, validSubmitRequest()); err != nil {
		t.Fatal(err)
	}
	if len(reports.deliveries) != 0 {
		t.Fatalf("deliveries = %d", len(reports.deliveries))
	}
	if len(reports.byKey) != 1 {
		t.Fatal("report not saved")
	}
}

func TestSubmitValidationErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*SubmitRequest)
	}{
		{"bad status", func(r *SubmitRequest) { r.Status = "done" }},
		{"missing agent", func(r *SubmitRequest) { r.Agent = "" }},
		{"missing key", func(r *SubmitRequest) { r.IdempotencyKey = "" }},
		{"bad link", func(r *SubmitRequest) { r.Links = []LinkInput{{Label: "", URL: "https://x.test"}} }},
		{"bad context", func(r *SubmitRequest) { r.Context = map[string]string{"agent": "spoof"} }},
		{"long title", func(r *SubmitRequest) { r.Title = strings.Repeat("a", domain.MaxTitleLen+1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uc, reports, _ := newSubmit()
			req := validSubmitRequest()
			tt.mutate(&req)
			if _, err := uc.Handle(context.Background(), submitter, req); err == nil {
				t.Fatal("expected error")
			}
			if len(reports.byKey) != 0 {
				t.Fatal("nothing should be saved")
			}
		})
	}
}

func TestSubmitSinkError(t *testing.T) {
	uc, reports, sinks := newSubmit()
	boom := errors.New("sink lookup failed")
	sinks.err = boom
	if _, err := uc.Handle(context.Background(), submitter, validSubmitRequest()); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if len(reports.byKey) != 0 {
		t.Fatal("report should not be saved")
	}
}

func TestSubmitLookupError(t *testing.T) {
	uc, reports, _ := newSubmit()
	boom := errors.New("store down")
	reports.lookupErr = boom
	if _, err := uc.Handle(context.Background(), submitter, validSubmitRequest()); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestSubmitSaveError(t *testing.T) {
	uc, reports, _ := newSubmit()
	boom := errors.New("write failed")
	reports.saveErr = boom
	if _, err := uc.Handle(context.Background(), submitter, validSubmitRequest()); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

type racingReports struct {
	existing  domain.Report
	saved     bool
	lookupErr error
}

func (r *racingReports) ByIdempotencyKey(context.Context, domain.TenantID, domain.IdempotencyKey) (domain.Report, bool, error) {
	if !r.saved {
		return domain.Report{}, false, nil
	}
	if r.lookupErr != nil {
		return domain.Report{}, false, r.lookupErr
	}
	return r.existing, true, nil
}

func (r *racingReports) Save(context.Context, domain.Report, []domain.Delivery) error {
	r.saved = true
	return ErrIdempotencyConflict
}

func (r *racingReports) ByID(context.Context, domain.ReportID) (domain.Report, bool, error) {
	return domain.Report{}, false, nil
}
