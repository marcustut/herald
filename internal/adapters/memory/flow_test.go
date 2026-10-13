package memory_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/adapters/hash"
	"github.com/marcustut/herald/internal/adapters/id"
	"github.com/marcustut/herald/internal/adapters/memory"
	"github.com/marcustut/herald/internal/adapters/secret"
	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// The full path with the real crypto adapters: issue a token, authenticate it,
// submit a report, and confirm it is redacted and fanned out to a delivery row.
func TestReportFlowEndToEnd(t *testing.T) {
	ctx := context.Background()
	creds := memory.NewCredentials()
	reports := memory.NewReports()

	issuer := app.Issuer{Store: creds, Hasher: hash.SHA256{}, Secrets: secret.Generator{}, Clock: fixedClock{}}
	raw, err := issuer.IssueProjectToken(ctx, "ten_1", "axis")
	if err != nil {
		t.Fatal(err)
	}

	auth := app.Authenticator{Store: creds, Hasher: hash.SHA256{}}
	principal, err := auth.Authenticate(ctx, raw, "")
	if err != nil {
		t.Fatal(err)
	}

	submit := app.SubmitReport{Reports: reports, Sinks: staticSinks{"email"}, IDs: id.Random{}, Clock: fixedClock{}}
	req := app.SubmitRequest{
		Status:         "success",
		Title:          "deploy finished",
		Body:           "password=hunter2",
		Agent:          "herdr-build",
		IdempotencyKey: "run-1",
	}
	id, err := submit.Handle(ctx, principal, req)
	if err != nil {
		t.Fatal(err)
	}

	rep, ok := reports.Report(id)
	if !ok {
		t.Fatal("report not found")
	}
	if strings.Contains(rep.Body, "hunter2") {
		t.Fatalf("secret persisted: %q", rep.Body)
	}
	if rep.ProjectID != "axis" || rep.AgentID != "herdr-build" {
		t.Fatalf("report = %+v", rep)
	}
	if got := reports.Deliveries(); len(got) != 1 || got[0].SinkID != "email" || got[0].ReportID != id {
		t.Fatalf("deliveries = %+v", got)
	}

	// Replaying the key returns the same id and does not fan out again.
	req.Body = "changed"
	again, err := submit.Handle(ctx, principal, req)
	if err != nil {
		t.Fatal(err)
	}
	if again != id {
		t.Fatalf("replay id = %q, want %q", again, id)
	}
	if got := reports.Deliveries(); len(got) != 1 {
		t.Fatalf("replay re-fanned out: %d deliveries", len(got))
	}
}

type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) }

type staticSinks []domain.SinkID

func (s staticSinks) EnabledSinks(context.Context, domain.TenantID, domain.ProjectID) ([]domain.SinkID, error) {
	return s, nil
}
