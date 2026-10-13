package httpapi_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marcustut/herald/internal/adapters/hash"
	"github.com/marcustut/herald/internal/adapters/httpapi"
	"github.com/marcustut/herald/internal/adapters/id"
	"github.com/marcustut/herald/internal/adapters/memory"
	"github.com/marcustut/herald/internal/adapters/metrics"
	"github.com/marcustut/herald/internal/adapters/secret"
	"github.com/marcustut/herald/internal/app"
)

func getBody(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func TestMetricsEndpointInstrumentsRequests(t *testing.T) {
	creds := memory.NewCredentials()
	events := memory.NewEvents()
	issuer := app.Issuer{Store: creds, Hasher: hash.SHA256{}, Secrets: secret.Generator{}, Clock: fixedClock{}}
	reg := metrics.NewRegistry()
	api := &httpapi.Server{
		Auth:     app.Authenticator{Store: creds, Hasher: hash.SHA256{}},
		Submit:   app.SubmitEvent{Events: events, Sinks: staticSinks{"email"}, IDs: id.Random{}, Clock: fixedClock{}},
		Observer: reg,
		Metrics:  reg,
	}
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	raw, err := issuer.IssueProjectToken(context.Background(), "ten_1", "axis")
	if err != nil {
		t.Fatal(err)
	}
	if got := postTo(t, ts.URL, raw, "application/json", validBody).StatusCode; got != http.StatusAccepted {
		t.Fatalf("accepted = %d", got)
	}
	if got := postTo(t, ts.URL, "", "application/json", validBody).StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("unauthorized = %d", got)
	}
	invalid := `{"project":"axis","status":"success","title":"","agent":"a","idempotencyKey":"k"}`
	if got := postTo(t, ts.URL, raw, "application/json", invalid).StatusCode; got != http.StatusUnprocessableEntity {
		t.Fatalf("validation = %d", got)
	}

	status, body := getBody(t, ts.URL+"/metrics")
	if status != http.StatusOK {
		t.Fatalf("metrics status = %d", status)
	}
	for _, want := range []string{
		`herald_http_requests_total{method="POST",route="/v1/events",status="202"} 1`,
		`herald_http_requests_total{method="POST",route="/v1/events",status="401"} 1`,
		`herald_http_requests_total{method="POST",route="/v1/events",status="422"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q\n%s", want, body)
		}
	}

	// No credential, project id, or event body may reach a label.
	for _, secret := range []string{raw, "all green", "axis"} {
		if strings.Contains(body, secret) {
			t.Fatalf("metrics leaked %q", secret)
		}
	}
}

func TestHealthzIgnoresReadiness(t *testing.T) {
	api := &httpapi.Server{
		Auth:  app.Authenticator{Store: memory.NewCredentials(), Hasher: hash.SHA256{}},
		Ready: func(context.Context) error { return errors.New("db down") },
	}
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	if status, _ := getBody(t, ts.URL+"/healthz"); status != http.StatusOK {
		t.Fatalf("healthz = %d, want 200 (liveness is independent of readiness)", status)
	}
	if status, _ := getBody(t, ts.URL+"/readyz"); status != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d, want 503", status)
	}
}

func TestReadyzReadyWhenProbePasses(t *testing.T) {
	api := &httpapi.Server{
		Auth:  app.Authenticator{Store: memory.NewCredentials(), Hasher: hash.SHA256{}},
		Ready: func(context.Context) error { return nil },
	}
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	if status, _ := getBody(t, ts.URL+"/readyz"); status != http.StatusOK {
		t.Fatalf("readyz = %d, want 200", status)
	}
}

func TestMetricsEndpointAvailableByDefault(t *testing.T) {
	api := &httpapi.Server{
		Auth: app.Authenticator{Store: memory.NewCredentials(), Hasher: hash.SHA256{}},
	}
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	if status, _ := getBody(t, ts.URL+"/healthz"); status != http.StatusOK {
		t.Fatalf("healthz = %d", status)
	}
	status, body := getBody(t, ts.URL+"/metrics")
	if status != http.StatusOK {
		t.Fatalf("metrics status = %d", status)
	}
	if !strings.Contains(body, "# TYPE herald_http_requests_total counter") {
		t.Fatalf("default metrics endpoint not wired\n%s", body)
	}
}
