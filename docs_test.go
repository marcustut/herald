package herald_test

// These tests keep the published v1 contracts (openapi.yaml and the docs under
// docs/) honest: the OpenAPI document must describe the routes and error codes
// the real httpapi server implements, and the public examples must not leak a
// personal host, email, or token.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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

type docsClock struct{}

func (docsClock) Now() time.Time { return time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC) }

type docsSinks struct{}

func (docsSinks) EnabledSinks(context.Context, domain.TenantID, domain.ProjectID) ([]domain.SinkID, error) {
	return []domain.SinkID{"email"}, nil
}

type allowLimiter struct{}

func (allowLimiter) Allow(context.Context, domain.TenantID, domain.ProjectID) (app.Decision, error) {
	return app.Decision{Allowed: true, Limit: 60, Remaining: 59, Reset: time.Unix(1760000060, 0)}, nil
}

type denyLimiter struct{}

func (denyLimiter) Allow(context.Context, domain.TenantID, domain.ProjectID) (app.Decision, error) {
	return app.Decision{
		Allowed:    false,
		Limit:      60,
		Remaining:  0,
		Reset:      time.Unix(1760000060, 0),
		RetryAfter: 17 * time.Second,
	}, nil
}

func newContractAPI(t *testing.T, limiter app.RateLimiter, ready func(context.Context) error) (*httptest.Server, string) {
	t.Helper()
	creds := memory.NewCredentials()
	events := memory.NewEvents()
	issuer := app.Issuer{Store: creds, Hasher: hash.SHA256{}, Secrets: secret.Generator{}, Clock: docsClock{}}
	api := &httpapi.Server{
		Auth:    app.Authenticator{Store: creds, Hasher: hash.SHA256{}},
		Submit:  app.SubmitEvent{Events: events, Sinks: docsSinks{}, IDs: id.Random{}, Clock: docsClock{}},
		Limiter: limiter,
		Ready:   ready,
	}
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	token, err := issuer.IssueProjectToken(context.Background(), "ten_1", "axis")
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return ts, token
}

const contractBody = `{"project":"axis","status":"success","title":"deploy","agent":"herdr-build","occurrenceId":"run-1"}`

func contractPost(t *testing.T, url, token, contentType, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func contractErrorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body struct {
		Error struct {
			Code  string `json:"code"`
			Field string `json:"field"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return body.Error.Code
}

// TestAPIContractMatchesDocs exercises the documented status codes against the
// real httpapi handler, so the promise in docs/http-api.md cannot drift from
// the implementation.
func TestAPIContractMatchesDocs(t *testing.T) {
	ts, token := newContractAPI(t, allowLimiter{}, nil)

	t.Run("accept returns 202 with an id", func(t *testing.T) {
		resp := contractPost(t, ts.URL+"/v1/events", token, "application/json", contractBody)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", resp.StatusCode)
		}
		var out struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.ID == "" {
			t.Fatal("no id in 202 response")
		}
	})

	t.Run("reports alias is deprecated but works", func(t *testing.T) {
		resp := contractPost(t, ts.URL+"/v1/reports", token, "application/json", contractBody)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", resp.StatusCode)
		}
		if got := resp.Header.Get("Deprecation"); got != "true" {
			t.Errorf("Deprecation = %q, want true", got)
		}
		if got := resp.Header.Get("Link"); !strings.Contains(got, `rel="successor-version"`) || !strings.Contains(got, "/v1/events") {
			t.Errorf("Link = %q, want successor-version to /v1/events", got)
		}
	})

	t.Run("missing token is 401 unauthenticated", func(t *testing.T) {
		resp := contractPost(t, ts.URL+"/v1/events", "", "application/json", contractBody)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		if code := contractErrorCode(t, resp); code != "unauthenticated" {
			t.Errorf("code = %q, want unauthenticated", code)
		}
	})

	t.Run("non-json content type is 415", func(t *testing.T) {
		resp := contractPost(t, ts.URL+"/v1/events", token, "text/plain", contractBody)
		if resp.StatusCode != http.StatusUnsupportedMediaType {
			t.Fatalf("status = %d, want 415", resp.StatusCode)
		}
		if code := contractErrorCode(t, resp); code != "unsupported_media_type" {
			t.Errorf("code = %q, want unsupported_media_type", code)
		}
	})

	t.Run("malformed json is 400 invalid_json", func(t *testing.T) {
		resp := contractPost(t, ts.URL+"/v1/events", token, "application/json", "{not json")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		if code := contractErrorCode(t, resp); code != "invalid_json" {
			t.Errorf("code = %q, want invalid_json", code)
		}
	})

	t.Run("missing title is 422 validation_failed with field", func(t *testing.T) {
		resp := contractPost(t, ts.URL+"/v1/events", token, "application/json", `{"project":"axis","status":"success","title":"","agent":"herdr"}`)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", resp.StatusCode)
		}
		var body struct {
			Error struct {
				Code  string `json:"code"`
				Field string `json:"field"`
			} `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Error.Code != "validation_failed" || body.Error.Field != "title" {
			t.Errorf("error = %+v, want validation_failed/title", body.Error)
		}
	})

	t.Run("health and readiness", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/healthz")
		if err != nil {
			t.Fatalf("healthz: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("healthz = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("rate limited is 429 with headers", func(t *testing.T) {
		deny, denyToken := newContractAPI(t, denyLimiter{}, nil)
		resp := contractPost(t, deny.URL+"/v1/events", denyToken, "application/json", contractBody)
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", resp.StatusCode)
		}
		if got := resp.Header.Get("Retry-After"); got != "17" {
			t.Errorf("Retry-After = %q, want 17", got)
		}
		if got := resp.Header.Get("X-RateLimit-Remaining"); got != "0" {
			t.Errorf("X-RateLimit-Remaining = %q, want 0", got)
		}
		if code := contractErrorCode(t, resp); code != "rate_limited" {
			t.Errorf("code = %q, want rate_limited", code)
		}
	})

	t.Run("successful submit advertises the rate limit budget", func(t *testing.T) {
		limit, limitToken := newContractAPI(t, allowLimiter{}, nil)
		resp := contractPost(t, limit.URL+"/v1/events", limitToken, "application/json", contractBody)
		if got := resp.Header.Get("X-RateLimit-Limit"); got != "60" {
			t.Errorf("X-RateLimit-Limit = %q, want 60", got)
		}
		if got := resp.Header.Get("X-RateLimit-Remaining"); got != "59" {
			t.Errorf("X-RateLimit-Remaining = %q, want 59", got)
		}
	})

	t.Run("unready is 503 unavailable", func(t *testing.T) {
		unready, _ := newContractAPI(t, nil, func(context.Context) error { return context.DeadlineExceeded })
		resp, err := http.Get(unready.URL + "/readyz")
		if err != nil {
			t.Fatalf("readyz: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", resp.StatusCode)
		}
		if code := contractErrorCode(t, resp); code != "unavailable" {
			t.Errorf("code = %q, want unavailable", code)
		}
	})
}

// TestOpenAPIDocumentsImplementedContract is an always-on drift check: the
// spec must name every route and every error code the server emits.
func TestOpenAPIDocumentsImplementedContract(t *testing.T) {
	data, err := os.ReadFile(repoFile(t, "openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	spec := string(data)
	for _, want := range []string{
		"openapi: 3.1.0",
		"/v1/events:",
		"/v1/reports:",
		"/v1/auth/magic-link:",
		"/v1/auth/callback:",
		"/v1/auth/signout:",
		"/v1/me:",
		"/v1/account:",
		"/v1/projects:",
		"/v1/projects/{project}:",
		"/v1/tokens:",
		"/v1/tokens/{id}:",
		"/v1/projects/{project}/token:",
		"/v1/projects/{project}/token/rotate:",
		"/v1/projects/{project}/destinations:",
		"/v1/projects/{project}/destinations/{id}:",
		"/v1/projects/{project}/destinations/{id}/verify:",
		"/v1/destinations/verify:",
		"/v1/deliveries:",
		"/v1/email/events:",
		"/healthz:",
		"/readyz:",
		"/metrics:",
		`Deprecation`,
		`successor-version`,
		`herald_session`,
		`X-CSRF-Token`,
	} {
		if !strings.Contains(spec, want) {
			t.Errorf("openapi.yaml is missing %q", want)
		}
	}
	for _, code := range []string{
		"invalid_json",
		"unauthenticated",
		"payload_too_large",
		"unsupported_media_type",
		"validation_failed",
		"rate_limited",
		"unavailable",
		"csrf_failed",
		"project_not_found",
		"token_not_found",
		"destination_not_found",
	} {
		if !strings.Contains(spec, code) {
			t.Errorf("openapi.yaml is missing error code %q", code)
		}
	}
}

// TestOpenAPIParses validates the document with a real YAML parser when one is
// available locally. CI has no YAML tool, so the check is skipped there; the
// structural test above still guards drift.
func TestOpenAPIParses(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	script := "import sys, yaml; yaml.safe_load(open(sys.argv[1], encoding='utf-8'))"
	if out, err := exec.Command(python, "-c", script, repoFile(t, "openapi.yaml")).CombinedOutput(); err != nil {
		t.Fatalf("openapi.yaml did not parse: %v\n%s", err, out)
	}
}

// TestPublicContractsHaveNoPersonalData enforces the issue's "no personal
// emails, hostnames or tokens in examples" requirement on the files this
// change owns.
func TestPublicContractsHaveNoPersonalData(t *testing.T) {
	files := []string{
		"openapi.yaml",
		"docs/http-api.md",
		"docs/cli.md",
		"docs/mcp.md",
		"docs/skill.md",
	}
	forbidden := []string{
		"marcustut",
		"marcus@",
		"@gmail.com",
		"@outlook.com",
		"@proton.me",
		"@yahoo.",
	}
	for _, name := range files {
		data, err := os.ReadFile(repoFile(t, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, bad := range forbidden {
			if strings.Contains(string(data), bad) {
				t.Errorf("%s contains personal data %q", name, bad)
			}
		}
	}
}
