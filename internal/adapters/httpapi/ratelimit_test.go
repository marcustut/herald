package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/adapters/hash"
	"github.com/marcustut/herald/internal/adapters/httpapi"
	"github.com/marcustut/herald/internal/adapters/id"
	"github.com/marcustut/herald/internal/adapters/memory"
	"github.com/marcustut/herald/internal/adapters/secret"
	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

type limitedHarness struct {
	server *httptest.Server
	issuer app.Issuer
}

// newLimitedHarness wires the HTTP server to an in-memory limiter.
func newLimitedHarness(t *testing.T, limits app.Limits) *limitedHarness {
	t.Helper()
	creds := memory.NewCredentials()
	events := memory.NewEvents()
	issuer := app.Issuer{Store: creds, Hasher: hash.SHA256{}, Secrets: secret.Generator{}, Clock: fixedClock{}}
	api := &httpapi.Server{
		Auth: app.Authenticator{Store: creds, Hasher: hash.SHA256{}},
		Submit: app.SubmitEvent{
			Events: events,
			Sinks:  staticSinks{"email"},
			IDs:    id.Random{},
			Clock:  fixedClock{},
		},
		Limiter: memory.NewRateLimiter(limits, fixedClock{}, nil),
	}
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	return &limitedHarness{server: ts, issuer: issuer}
}

func (h *limitedHarness) token(t *testing.T, project string) string {
	t.Helper()
	raw, err := h.issuer.IssueProjectToken(context.Background(), "ten_1", domain.ProjectID(project))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (h *limitedHarness) post(t *testing.T, path, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// requestBudget is the global budget every (tenant, project) shares in these
// tests.
func requestBudget() app.Limits {
	return app.Limits{Requests: 2, Window: time.Minute, Daily: 1000, Monthly: 1000}
}

func TestSubmitRateLimitedWithHeaders(t *testing.T) {
	h := newLimitedHarness(t, requestBudget())
	token := h.token(t, "axis")

	first := h.post(t, "/v1/events", token, validBody)
	if first.StatusCode != http.StatusAccepted {
		t.Fatalf("first status = %d", first.StatusCode)
	}
	if got := first.Header.Get("X-RateLimit-Limit"); got != "2" {
		t.Fatalf("X-RateLimit-Limit = %q", got)
	}
	if got := first.Header.Get("X-RateLimit-Remaining"); got != "1" {
		t.Fatalf("X-RateLimit-Remaining = %q", got)
	}
	if first.Header.Get("X-RateLimit-Reset") == "" {
		t.Fatal("missing X-RateLimit-Reset")
	}

	if got := h.post(t, "/v1/events", token, validBody).StatusCode; got != http.StatusAccepted {
		t.Fatalf("second status = %d", got)
	}

	third := h.post(t, "/v1/events", token, validBody)
	if third.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third status = %d, want 429", third.StatusCode)
	}
	if got := third.Header.Get("X-RateLimit-Remaining"); got != "0" {
		t.Fatalf("X-RateLimit-Remaining = %q", got)
	}
	retry := third.Header.Get("Retry-After")
	secs, err := strconv.Atoi(retry)
	if err != nil || secs < 1 {
		t.Fatalf("Retry-After = %q", retry)
	}
	if got := decodeError(t, third); got != "rate_limited:" {
		t.Fatalf("error = %q", got)
	}
}

// A rejected (invalid) token must stay 401 even when the project budget is gone,
// so 429 cannot be used to probe for valid tokens.
func TestRateLimitDoesNotLeakTokenExistence(t *testing.T) {
	h := newLimitedHarness(t, requestBudget())
	token := h.token(t, "axis")
	for i := 0; i < 2; i++ {
		h.post(t, "/v1/events", token, validBody)
	}
	if got := h.post(t, "/v1/events", token, validBody).StatusCode; got != http.StatusTooManyRequests {
		t.Fatalf("budget not exhausted: %d", got)
	}
	if got := h.post(t, "/v1/events", "", validBody).StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("missing token = %d, want 401", got)
	}
	if got := h.post(t, "/v1/events", "herald_axis_"+strings.Repeat("s", 16), validBody).StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("unknown token = %d, want 401", got)
	}
}

func TestRateLimitAppliesToLegacyAlias(t *testing.T) {
	h := newLimitedHarness(t, requestBudget())
	token := h.token(t, "axis")
	for i := 0; i < 2; i++ {
		if got := h.post(t, "/v1/reports", token, validBody).StatusCode; got != http.StatusAccepted {
			t.Fatalf("call %d status = %d", i, got)
		}
	}
	if got := h.post(t, "/v1/reports", token, validBody).StatusCode; got != http.StatusTooManyRequests {
		t.Fatalf("legacy alias status = %d, want 429", got)
	}
}

// stubQueue is a QueueGauge with a fixed depth, used to drive backpressure.
type stubQueue struct{ depth int }

func (q stubQueue) Outstanding(context.Context, domain.TenantID, domain.ProjectID) (int, error) {
	return q.depth, nil
}

// A project whose durable queue is congested gets 503 queue_full with a
// Retry-After, distinct from a 429 rate limit.
func TestSubmitQueueFullIs503(t *testing.T) {
	creds := memory.NewCredentials()
	events := memory.NewEvents()
	issuer := app.Issuer{Store: creds, Hasher: hash.SHA256{}, Secrets: secret.Generator{}, Clock: fixedClock{}}
	api := &httpapi.Server{
		Auth: app.Authenticator{Store: creds, Hasher: hash.SHA256{}},
		Submit: app.SubmitEvent{
			Events: events,
			Sinks:  staticSinks{"email"},
			IDs:    id.Random{},
			Clock:  fixedClock{},
		},
		Limiter: memory.NewRateLimiter(app.Limits{
			Requests: 100, Window: time.Minute, Daily: 1000, Monthly: 1000, Queued: 1,
		}, fixedClock{}, stubQueue{depth: 5}),
	}
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)

	raw, err := issuer.IssueProjectToken(context.Background(), "ten_1", "axis")
	if err != nil {
		t.Fatal(err)
	}
	resp := postTo(t, ts.URL, raw, "application/json", validBody)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := decodeError(t, resp); got != "queue_full:" {
		t.Fatalf("error = %q, want queue_full", got)
	}
	if retry := resp.Header.Get("Retry-After"); retry == "" {
		t.Fatal("missing Retry-After")
	}
}

func TestRateLimitPartitionsProjects(t *testing.T) {
	h := newLimitedHarness(t, requestBudget())
	axis := h.token(t, "axis")
	finance := h.token(t, "finance")
	financeBody := strings.Replace(validBody, `"axis"`, `"finance"`, 1)

	for i := 0; i < 2; i++ {
		h.post(t, "/v1/events", axis, validBody)
	}
	if got := h.post(t, "/v1/events", axis, validBody).StatusCode; got != http.StatusTooManyRequests {
		t.Fatalf("axis not exhausted: %d", got)
	}
	if got := h.post(t, "/v1/events", finance, financeBody).StatusCode; got != http.StatusAccepted {
		t.Fatalf("finance blocked by axis budget: %d", got)
	}
}
