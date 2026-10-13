package render

import (
	"strings"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/domain"
)

func sample() domain.Report {
	return domain.Report{
		ID:        "rep_1",
		TenantID:  "ten_1",
		ProjectID: "axis",
		AgentID:   "herdr-build",
		Status:    domain.StatusSuccess,
		Title:     "deploy finished",
		Body:      "all green",
		Links:     []domain.Link{{Label: "PR", URL: "https://github.com/x/y/pull/1"}},
		Context:   domain.Context{"host": "build-01"},
		CreatedAt: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC),
	}
}

func TestReport(t *testing.T) {
	got, err := Report(sample())
	if err != nil {
		t.Fatal(err)
	}
	want := "[herald] success · axis · herdr-build@build-01 — deploy finished"
	if got.Subject != want {
		t.Fatalf("subject = %q", got.Subject)
	}
	for _, want := range []string{"deploy finished", "all green", "build-01", "https://github.com/x/y/pull/1"} {
		if !strings.Contains(got.HTML, want) {
			t.Errorf("html missing %q", want)
		}
		if !strings.Contains(got.Text, want) {
			t.Errorf("text missing %q", want)
		}
	}
}

func TestReportEscapesHTML(t *testing.T) {
	rep := sample()
	rep.Title = `<script>alert(1)</script>`
	rep.Body = `<img src=x onerror=alert(1)>`
	got, err := Report(rep)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.HTML, "<script>") || strings.Contains(got.HTML, "<img") {
		t.Fatalf("html not escaped: %q", got.HTML)
	}
	if !strings.Contains(got.HTML, "&lt;script&gt;") {
		t.Fatalf("expected escaped script: %q", got.HTML)
	}
}

func TestSubjectWithoutHost(t *testing.T) {
	got := Subject("failure", "axis", "herdr", "", "boom")
	if got != "[herald] failure · axis · herdr — boom" {
		t.Fatalf("subject = %q", got)
	}
}
