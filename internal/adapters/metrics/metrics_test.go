package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/adapters/metrics"
	"github.com/marcustut/herald/internal/app"
)

func scrape(t *testing.T, r *metrics.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q", ct)
	}
	return rec.Body.String()
}

func TestExposeHTTP(t *testing.T) {
	r := metrics.NewRegistry()
	r.ObserveHTTP("POST /v1/events", http.MethodPost, http.StatusAccepted, 20*time.Millisecond)
	r.ObserveHTTP("POST /v1/events", http.MethodPost, http.StatusAccepted, 20*time.Millisecond)
	r.ObserveHTTP("POST /v1/events", http.MethodPost, http.StatusUnauthorized, 0)

	body := scrape(t, r)
	for _, want := range []string{
		"# HELP herald_http_requests_total",
		"# TYPE herald_http_requests_total counter",
		`herald_http_requests_total{method="POST",route="/v1/events",status="202"} 2`,
		`herald_http_requests_total{method="POST",route="/v1/events",status="401"} 1`,
		"# TYPE herald_http_request_duration_seconds histogram",
		`herald_http_request_duration_seconds_count{method="POST",route="/v1/events"} 3`,
		`herald_http_request_duration_seconds_bucket{le="+Inf",method="POST",route="/v1/events"} 3`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output missing %q\n%s", want, body)
		}
	}
}

func TestObserveDelivery(t *testing.T) {
	r := metrics.NewRegistry()
	r.ObserveDelivery("email", app.OutcomeDelivered, time.Millisecond)
	r.ObserveDelivery("webhook", app.OutcomeRetry, time.Millisecond)
	r.ObserveDelivery("webhook", app.OutcomeDead, time.Millisecond)

	body := scrape(t, r)
	for _, want := range []string{
		`herald_delivery_attempts_total{sink="email"} 1`,
		`herald_delivery_attempts_total{sink="webhook"} 2`,
		`herald_delivery_outcomes_total{outcome="delivered",sink="email"} 1`,
		`herald_delivery_outcomes_total{outcome="dead",sink="webhook"} 1`,
		`herald_delivery_outcomes_total{outcome="retry",sink="webhook"} 1`,
		`herald_delivery_duration_seconds_count{outcome="delivered",sink="email"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output missing %q\n%s", want, body)
		}
	}
}

func TestObserveSubmitRejectionsAndBacklog(t *testing.T) {
	r := metrics.NewRegistry()
	r.ObserveSubmitRejection(app.ScopeQueue)
	r.ObserveSubmitRejection(app.ScopeRequests)
	r.ObserveSubmitRejection(app.ScopeQueue)
	r.ObserveSubmitRejection("not-a-scope")
	r.ObserveQueueDepth(42)

	body := scrape(t, r)
	for _, want := range []string{
		"# TYPE herald_submit_rejections_total counter",
		`herald_submit_rejections_total{scope="queue"} 2`,
		`herald_submit_rejections_total{scope="requests"} 1`,
		`herald_submit_rejections_total{scope="other"} 1`,
		"# TYPE herald_delivery_backlog gauge",
		"herald_delivery_backlog 42",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output missing %q\n%s", want, body)
		}
	}
	if strings.Contains(body, "not-a-scope") {
		t.Fatalf("unbounded scope leaked\n%s", body)
	}
}

func TestObserveBounceOutcome(t *testing.T) {
	r := metrics.NewRegistry()
	r.ObserveDelivery("email", app.OutcomeBounce, time.Millisecond)

	body := scrape(t, r)
	if !strings.Contains(body, `herald_delivery_outcomes_total{outcome="bounce",sink="email"} 1`) {
		t.Fatalf("bounce outcome missing\n%s", body)
	}
}

func TestDispatchPollGauge(t *testing.T) {
	r := metrics.NewRegistry()
	now := time.Unix(1_760_000_000, 0)
	r.ObserveDispatchPoll(now)

	body := scrape(t, r)
	if !strings.Contains(body, "# TYPE herald_dispatcher_last_poll_timestamp_seconds gauge") {
		t.Fatalf("missing gauge type\n%s", body)
	}
	if !strings.Contains(body, "herald_dispatcher_last_poll_timestamp_seconds 1760000000") {
		t.Fatalf("missing gauge value\n%s", body)
	}
}

// A client-supplied method or an unmatched route must not create an unbounded
// label value.
func TestLabelsAreBounded(t *testing.T) {
	r := metrics.NewRegistry()
	r.ObserveHTTP("", "FROBNICATE", http.StatusNotFound, 0)
	r.ObserveDelivery("", app.OutcomeDelivered, 0)

	body := scrape(t, r)
	if !strings.Contains(body, `method="other"`) {
		t.Fatalf("method not bounded\n%s", body)
	}
	if !strings.Contains(body, `route="other"`) {
		t.Fatalf("route not bounded\n%s", body)
	}
	if !strings.Contains(body, `sink="unknown"`) {
		t.Fatalf("sink not bounded\n%s", body)
	}
	if strings.Contains(body, "FROBNICATE") {
		t.Fatalf("unbounded label leaked\n%s", body)
	}
}

func TestConcurrentObserve(t *testing.T) {
	r := metrics.NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.ObserveHTTP("POST /v1/events", http.MethodPost, http.StatusAccepted, time.Millisecond)
			r.ObserveDelivery("email", app.OutcomeDelivered, time.Millisecond)
		}()
	}
	wg.Wait()

	body := scrape(t, r)
	if !strings.Contains(body, `herald_http_requests_total{method="POST",route="/v1/events",status="202"} 50`) {
		t.Fatalf("lost concurrent HTTP increments\n%s", body)
	}
	if !strings.Contains(body, `herald_delivery_attempts_total{sink="email"} 50`) {
		t.Fatalf("lost concurrent delivery increments\n%s", body)
	}
}

func TestDefaultSatisfiesObserver(t *testing.T) {
	var _ app.Observer = metrics.Default
	var _ http.Handler = metrics.Default
}
