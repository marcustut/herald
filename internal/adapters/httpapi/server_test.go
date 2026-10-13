package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) }

type staticSinks []domain.SinkID

func (s staticSinks) EnabledSinks(context.Context, domain.TenantID, domain.ProjectID) ([]domain.SinkID, error) {
	return s, nil
}

type harness struct {
	server *httptest.Server
	issuer app.Issuer
	events *memory.Events
}

func newHarness(t *testing.T) *harness {
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
	}
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	return &harness{server: ts, issuer: issuer, events: events}
}

func (h *harness) token(t *testing.T) string {
	t.Helper()
	raw, err := h.issuer.IssueProjectToken(context.Background(), "ten_1", "axis")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (h *harness) post(t *testing.T, token, contentType, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/v1/events", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

const validBody = `{"project":"axis","status":"success","title":"deploy","body":"all green","agent":"herdr-build","idempotencyKey":"run-1"}`

func decodeError(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body struct {
		Error struct {
			Code  string `json:"code"`
			Field string `json:"field"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Error.Code + ":" + body.Error.Field
}

func TestSubmitAccepted(t *testing.T) {
	h := newHarness(t)
	resp := h.post(t, h.token(t), "application/json", validBody)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.ID == "" {
		t.Fatal("missing id")
	}
	if _, ok := h.events.Event(domain.EventID(out.ID)); !ok {
		t.Fatal("event not stored")
	}
}

func TestSubmitIdempotentReplay(t *testing.T) {
	h := newHarness(t)
	token := h.token(t)
	id1 := decodeID(t, h.post(t, token, "application/json", validBody))
	id2 := decodeID(t, h.post(t, token, "application/json", validBody))
	if id1 != id2 {
		t.Fatalf("ids differ: %q vs %q", id1, id2)
	}
}

// Two identical completions with no occurrence id are two distinct events.
func TestSubmitIdenticalCompletionsAreDistinct(t *testing.T) {
	h := newHarness(t)
	token := h.token(t)
	const body = `{"project":"axis","status":"success","title":"deploy","body":"all green","agent":"herdr-build"}`
	id1 := decodeID(t, h.post(t, token, "application/json", body))
	id2 := decodeID(t, h.post(t, token, "application/json", body))
	if id1 == id2 {
		t.Fatalf("identical completions collapsed: %q", id1)
	}
}

func TestSubmitOccurrenceRetry(t *testing.T) {
	h := newHarness(t)
	token := h.token(t)
	const body = `{"project":"axis","status":"success","title":"deploy","agent":"herdr-build","occurrenceId":"run-42"}`
	id1 := decodeID(t, h.post(t, token, "application/json", body))
	id2 := decodeID(t, h.post(t, token, "application/json", body))
	if id1 != id2 {
		t.Fatalf("occurrence retry created a new event: %q vs %q", id1, id2)
	}
}

func TestSubmitAuthErrors(t *testing.T) {
	h := newHarness(t)
	if got := h.post(t, "", "application/json", validBody).StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("missing token: %d", got)
	}
	if got := h.post(t, "herald_axis_"+strings.Repeat("s", 16), "application/json", validBody).StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("unknown token: %d", got)
	}
}

func TestSubmitContentType(t *testing.T) {
	h := newHarness(t)
	resp := h.post(t, h.token(t), "text/plain", validBody)
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestSubmitMalformedJSON(t *testing.T) {
	h := newHarness(t)
	resp := h.post(t, h.token(t), "application/json", "{not json")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestSubmitValidation(t *testing.T) {
	h := newHarness(t)
	resp := h.post(t, h.token(t), "application/json", `{"project":"axis","status":"success","title":"","agent":"herdr","idempotencyKey":"k"}`)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := decodeError(t, resp); !strings.HasPrefix(got, "validation_failed:title") {
		t.Fatalf("error = %q", got)
	}
}

func TestSubmitPayloadTooLarge(t *testing.T) {
	creds := memory.NewCredentials()
	events := memory.NewEvents()
	issuer := app.Issuer{Store: creds, Hasher: hash.SHA256{}, Secrets: secret.Generator{}, Clock: fixedClock{}}
	api := &httpapi.Server{
		Auth:    app.Authenticator{Store: creds, Hasher: hash.SHA256{}},
		Submit:  app.SubmitEvent{Events: events, Sinks: staticSinks{"email"}, IDs: id.Random{}, Clock: fixedClock{}},
		MaxBody: 16,
	}
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()
	raw, _ := issuer.IssueProjectToken(context.Background(), "ten_1", "axis")
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/events", strings.NewReader(validBody))
	req.Header.Set("Authorization", "Bearer "+raw)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestScopedTokenProjectAuthorization(t *testing.T) {
	creds := memory.NewCredentials()
	events := memory.NewEvents()
	issuer := app.Issuer{Store: creds, Hasher: hash.SHA256{}, Secrets: secret.Generator{}, Clock: fixedClock{}}
	api := &httpapi.Server{
		Auth:   app.Authenticator{Store: creds, Hasher: hash.SHA256{}},
		Submit: app.SubmitEvent{Events: events, Sinks: staticSinks{"email"}, IDs: id.Random{}, Clock: fixedClock{}},
	}
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()
	scoped, err := issuer.IssueScopedToken(context.Background(), "ten_1", []domain.ProjectID{"axis", "herald"})
	if err != nil {
		t.Fatal(err)
	}

	post := func(body string) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/events", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+scoped)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	if got := post(validBody); got != http.StatusAccepted {
		t.Fatalf("in-scope project: %d", got)
	}
	if got := post(strings.Replace(validBody, `"axis"`, `"beta"`, 1)); got != http.StatusUnauthorized {
		t.Fatalf("out-of-scope project: %d", got)
	}
	if got := post(`{"status":"success","title":"t","agent":"a","idempotencyKey":"k"}`); got != http.StatusUnauthorized {
		t.Fatalf("missing project: %d", got)
	}
}

func TestHealthAndReady(t *testing.T) {
	h := newHarness(t)
	resp, err := http.Get(h.server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}

	creds := memory.NewCredentials()
	api := &httpapi.Server{
		Auth:  app.Authenticator{Store: creds, Hasher: hash.SHA256{}},
		Ready: func(context.Context) error { return context.DeadlineExceeded },
	}
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()
	readyResp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer readyResp.Body.Close()
	if readyResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d", readyResp.StatusCode)
	}
}

func decodeID(t *testing.T, resp *http.Response) string {
	t.Helper()
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.ID
}
