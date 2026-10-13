package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marcustut/herald/internal/adapters/hash"
	"github.com/marcustut/herald/internal/adapters/httpapi"
	"github.com/marcustut/herald/internal/adapters/id"
	"github.com/marcustut/herald/internal/adapters/memory"
	"github.com/marcustut/herald/internal/adapters/secret"
	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

type failingEvents struct{ err error }

func (f failingEvents) Save(context.Context, domain.Event, []domain.Delivery) error { return f.err }
func (f failingEvents) ByID(context.Context, domain.EventID) (domain.Event, bool, error) {
	return domain.Event{}, false, f.err
}
func (f failingEvents) ByIdempotencyKey(context.Context, domain.TenantID, domain.IdempotencyKey) (domain.Event, bool, error) {
	return domain.Event{}, false, f.err
}

func postTo(t *testing.T, url, token, contentType, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/v1/events", strings.NewReader(body))
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

func TestSubmitServerError(t *testing.T) {
	creds := memory.NewCredentials()
	issuer := app.Issuer{Store: creds, Hasher: hash.SHA256{}, Secrets: secret.Generator{}, Clock: fixedClock{}}
	raw, _ := issuer.IssueProjectToken(context.Background(), "ten_1", "axis")
	api := &httpapi.Server{
		Auth: app.Authenticator{Store: creds, Hasher: hash.SHA256{}},
		Submit: app.SubmitEvent{
			Events: failingEvents{err: errors.New("db down")},
			Sinks:  staticSinks{"email"},
			IDs:    id.Random{},
			Clock:  fixedClock{},
		},
	}
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()
	if got := postTo(t, ts.URL, raw, "application/json", validBody).StatusCode; got != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", got)
	}
}

func TestSubmitValidationFields(t *testing.T) {
	h := newHarness(t)
	token := h.token(t)
	tests := []struct {
		name  string
		body  string
		field string
	}{
		{"status", `{"project":"axis","status":"x","title":"t","agent":"a","idempotencyKey":"k"}`, "status"},
		{"agent", `{"project":"axis","status":"success","title":"t","agent":"","idempotencyKey":"k"}`, "agent"},
		{"links", `{"project":"axis","status":"success","title":"t","agent":"a","idempotencyKey":"k","links":[{"label":"","url":"https://x"}]}`, "links"},
		{"context", `{"project":"axis","status":"success","title":"t","agent":"a","idempotencyKey":"k","context":{"agent":"x"}}`, "context"},
		{"idempotencyKey", `{"project":"axis","status":"success","title":"t","agent":"a"}`, "idempotencyKey"},
		{"body", `{"project":"axis","status":"success","title":"t","agent":"a","idempotencyKey":"k","body":"` + strings.Repeat("a", domain.MaxBodyLen+1) + `"}`, "body"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := h.post(t, token, "application/json", tt.body)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			if got := decodeError(t, resp); got != "validation_failed:"+tt.field {
				t.Fatalf("error = %q, want field %q", got, tt.field)
			}
		})
	}
}

func TestSubmitWithLinks(t *testing.T) {
	h := newHarness(t)
	body := `{"project":"axis","status":"success","title":"t","agent":"a","idempotencyKey":"k","links":[{"label":"PR","url":"https://x.test/1"}]}`
	if got := h.post(t, h.token(t), "application/json", body).StatusCode; got != http.StatusAccepted {
		t.Fatalf("status = %d", got)
	}
}

func TestContentTypeParsing(t *testing.T) {
	h := newHarness(t)
	token := h.token(t)
	if got := h.post(t, token, "application/json; charset=utf-8", validBody).StatusCode; got != http.StatusAccepted {
		t.Fatalf("charset content type: %d", got)
	}
	if got := h.post(t, token, "application/json; charset", validBody).StatusCode; got != http.StatusUnsupportedMediaType {
		t.Fatalf("malformed content type: %d", got)
	}
}

func TestReadySuccess(t *testing.T) {
	creds := memory.NewCredentials()
	api := &httpapi.Server{
		Auth:  app.Authenticator{Store: creds, Hasher: hash.SHA256{}},
		Ready: func(context.Context) error { return nil },
	}
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz = %d", resp.StatusCode)
	}
}

func TestLegacyReportsAlias(t *testing.T) {
	h := newHarness(t)
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/v1/reports", strings.NewReader(validBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token(t))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp.Header.Get("Deprecation") != "true" {
		t.Fatal("missing Deprecation header on legacy alias")
	}
}
