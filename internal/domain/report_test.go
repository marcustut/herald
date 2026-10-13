package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var refTime = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

func validInput() NewReportInput {
	return NewReportInput{
		ID:             "rep_1",
		TenantID:       "ten_1",
		ProjectID:      "axis",
		AgentID:        "herdr-build",
		Status:         StatusSuccess,
		Title:          "deploy finished",
		Body:           "all green",
		Context:        Context{"host": "build-01"},
		IdempotencyKey: "run-1",
		CreatedAt:      refTime,
	}
}

func TestNewReport(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*NewReportInput)
		wantErr error
	}{
		{"valid", func(*NewReportInput) {}, nil},
		{"missing id", func(in *NewReportInput) { in.ID = "" }, ErrInvalidReport},
		{"missing tenant", func(in *NewReportInput) { in.TenantID = "  " }, ErrInvalidReport},
		{"missing project", func(in *NewReportInput) { in.ProjectID = "" }, ErrInvalidReport},
		{"malformed project", func(in *NewReportInput) { in.ProjectID = "Bad" }, ErrInvalidReport},
		{"invalid status", func(in *NewReportInput) { in.Status = "done" }, ErrInvalidStatus},
		{"missing agent", func(in *NewReportInput) { in.AgentID = "" }, ErrInvalidAgent},
		{"invalid agent", func(in *NewReportInput) { in.AgentID = "Bad Agent" }, ErrInvalidAgent},
		{"invalid context key", func(in *NewReportInput) { in.Context = Context{"bad key": "x"} }, ErrInvalidContext},
		{"reserved context key", func(in *NewReportInput) { in.Context = Context{"agent": "spoof"} }, ErrInvalidContext},
		{"empty title", func(in *NewReportInput) { in.Title = "" }, ErrTitleRequired},
		{"blank title", func(in *NewReportInput) { in.Title = "   " }, ErrTitleRequired},
		{"title at cap", func(in *NewReportInput) { in.Title = strings.Repeat("a", MaxTitleLen) }, nil},
		{"title over cap", func(in *NewReportInput) { in.Title = strings.Repeat("a", MaxTitleLen+1) }, ErrTitleTooLong},
		{"body at cap", func(in *NewReportInput) { in.Body = strings.Repeat("b", MaxBodyLen) }, nil},
		{"body over cap", func(in *NewReportInput) { in.Body = strings.Repeat("b", MaxBodyLen+1) }, ErrBodyTooLong},
		{"links at cap", func(in *NewReportInput) { in.Links = makeLinks(MaxLinks) }, nil},
		{"links over cap", func(in *NewReportInput) { in.Links = makeLinks(MaxLinks + 1) }, ErrTooManyLinks},
		{"missing idempotency key", func(in *NewReportInput) { in.IdempotencyKey = "" }, ErrInvalidIdempotencyKey},
		{"missing created at", func(in *NewReportInput) { in.CreatedAt = time.Time{} }, ErrInvalidReport},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validInput()
			tt.mutate(&in)
			_, err := NewReport(in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func makeLinks(n int) []Link {
	links := make([]Link, n)
	for i := range links {
		links[i] = Link{Label: "L", URL: "https://x.test"}
	}
	return links
}

func TestNewReportTrimsTitle(t *testing.T) {
	in := validInput()
	in.Title = "  spaced title  "
	got, err := NewReport(in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "spaced title" {
		t.Fatalf("title = %q", got.Title)
	}
}

func TestNewReportCopiesLinks(t *testing.T) {
	in := validInput()
	in.Links = makeLinks(1)
	got, err := NewReport(in)
	if err != nil {
		t.Fatal(err)
	}
	in.Links[0].Label = "mutated"
	if got.Links[0].Label != "L" {
		t.Fatal("report should own a copy of its links")
	}
}

func TestReportRedact(t *testing.T) {
	in := validInput()
	in.Title = "leaked token=supersecret"
	in.Body = "password: hunter2"
	in.Links = []Link{{Label: "db", URL: "postgres://u:p@host/db"}}
	in.Context = Context{"host": "build-01", "note": "secret=abc123"}
	rep, err := NewReport(in)
	if err != nil {
		t.Fatal(err)
	}

	red := rep.Redact()
	if strings.Contains(red.Title, "supersecret") {
		t.Errorf("title still leaks: %q", red.Title)
	}
	if strings.Contains(red.Body, "hunter2") {
		t.Errorf("body still leaks: %q", red.Body)
	}
	if strings.Contains(red.Links[0].URL, "p@host") {
		t.Errorf("link still leaks: %q", red.Links[0].URL)
	}
	if strings.Contains(red.Context["note"], "abc123") {
		t.Errorf("context still leaks: %q", red.Context["note"])
	}
	if red.Context.Host() != "build-01" {
		t.Errorf("non-secret context lost: %q", red.Context["host"])
	}

	if !strings.Contains(rep.Title, "supersecret") {
		t.Error("Redact must not mutate the receiver")
	}
	if rep.Links[0].URL != "postgres://u:p@host/db" {
		t.Error("Redact must not mutate receiver links")
	}
	if rep.Context["note"] != "secret=abc123" {
		t.Error("Redact must not mutate receiver context")
	}
}

func TestReportCarriesIdentity(t *testing.T) {
	rep, err := NewReport(validInput())
	if err != nil {
		t.Fatal(err)
	}
	if rep.AgentID != "herdr-build" {
		t.Fatalf("agent = %q", rep.AgentID)
	}
	if rep.Context.Host() != "build-01" {
		t.Fatalf("host = %q", rep.Context.Host())
	}
}
