package httpapi_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/marcustut/herald/internal/adapters/httpapi"
	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// shaHasher is a real one-way hash so the token list cannot accidentally
// contain the raw secret the way the predictable fixedHasher would.
type shaHasher struct{}

func (shaHasher) Hash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// fakeProjects is an in-memory app.ProjectStore for the black-box HTTP tests.
type fakeProjectKey struct {
	tenant  domain.TenantID
	project domain.ProjectID
}

type fakeProjects struct {
	mu    sync.Mutex
	byKey map[fakeProjectKey]domain.Project
}

func newFakeProjects() *fakeProjects {
	return &fakeProjects{byKey: map[fakeProjectKey]domain.Project{}}
}

func (s *fakeProjects) PutProject(_ context.Context, p domain.Project) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byKey[fakeProjectKey{p.TenantID, p.ID}] = p
	return nil
}

func (s *fakeProjects) Project(_ context.Context, tenant domain.TenantID, id domain.ProjectID) (domain.Project, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byKey[fakeProjectKey{tenant, id}]
	return p, ok, nil
}

func (s *fakeProjects) Projects(_ context.Context, tenant domain.TenantID) ([]domain.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Project
	for k, p := range s.byKey {
		if k.tenant == tenant {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *fakeProjects) DeleteProject(_ context.Context, tenant domain.TenantID, id domain.ProjectID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byKey[fakeProjectKey{tenant, id}]; !ok {
		return app.ErrProjectNotFound
	}
	delete(s.byKey, fakeProjectKey{tenant, id})
	return nil
}

// postJSON issues a JSON request with an optional CSRF header.
func (f *cloudFixture) postJSON(t *testing.T, method, path, csrf, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, f.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		req.Header.Set(httpapi.CSRFHeader, csrf)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (f *cloudFixture) tenant(t *testing.T) string {
	t.Helper()
	resp := f.do(t, http.MethodGet, "/v1/me", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/me = %d", resp.StatusCode)
	}
	var out struct {
		User struct {
			TenantID string `json:"tenantId"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.User.TenantID
}

func TestCloudProjectRoutesAbsentWhenDisabled(t *testing.T) {
	api := &httpapi.Server{}
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/dashboard"},
		{http.MethodGet, "/v1/projects"},
		{http.MethodPost, "/v1/projects"},
		{http.MethodGet, "/v1/tokens"},
	} {
		req, _ := http.NewRequest(tc.method, ts.URL+tc.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404 when cloud is disabled", tc.method, tc.path, resp.StatusCode)
		}
	}
}

func TestCloudProjectsRequireSessionAndCSRF(t *testing.T) {
	f := newCloudFixture(t, nil)

	// No session cookie: unauthenticated.
	resp := f.do(t, http.MethodGet, "/v1/projects", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("list without session = %d, want 401", resp.StatusCode)
	}

	csrf := f.signIn(t, "alice@example.com")

	// A mutation without the CSRF token is refused.
	resp = f.postJSON(t, http.MethodPost, "/v1/projects", "", `{"id":"alpha","name":"Alpha"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("create without CSRF = %d, want 403", resp.StatusCode)
	}
	resp = f.postJSON(t, http.MethodPost, "/v1/projects", "wrong", `{"id":"alpha","name":"Alpha"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("create with wrong CSRF = %d, want 403", resp.StatusCode)
	}
	// With the right token it succeeds, proving the session is usable.
	resp = f.postJSON(t, http.MethodPost, "/v1/projects", csrf, `{"id":"alpha","name":"Alpha"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create with CSRF = %d, want 201", resp.StatusCode)
	}
}

func TestCloudProjectsLifecycle(t *testing.T) {
	f := newCloudFixture(t, nil)
	csrf := f.signIn(t, "alice@example.com")
	tenant := f.tenant(t)

	// Create: ownership comes from the session, never the body. A body that
	// names another tenant must not move the project.
	resp := f.postJSON(t, http.MethodPost, "/v1/projects", csrf,
		`{"id":"alpha","name":"Alpha","tenantId":"ten_evil"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()
	if _, ok := f.projects.byKey[fakeProjectKey{tenant: "ten_evil", project: "alpha"}]; ok {
		t.Fatal("project was created under a client-supplied tenant")
	}
	if _, ok := f.projects.byKey[fakeProjectKey{tenant: domain.TenantID(tenant), project: "alpha"}]; !ok {
		t.Fatal("project was not created under the session tenant")
	}

	// Validation failure.
	resp = f.postJSON(t, http.MethodPost, "/v1/projects", csrf, `{"id":"Bad Slug","name":"x"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid slug = %d, want 422", resp.StatusCode)
	}

	// List.
	listResp := f.do(t, http.MethodGet, "/v1/projects", "")
	defer listResp.Body.Close()
	var list struct {
		Projects []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"projects"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Projects) != 1 || list.Projects[0].ID != "alpha" || list.Projects[0].Name != "Alpha" {
		t.Fatalf("projects = %+v", list.Projects)
	}

	// A project token is shown once.
	tokenResp := f.postJSON(t, http.MethodPost, "/v1/projects/alpha/token", csrf, "")
	var tokenOut struct {
		Token    string   `json:"token"`
		Kind     string   `json:"kind"`
		Projects []string `json:"projects"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tokenOut); err != nil {
		t.Fatal(err)
	}
	tokenResp.Body.Close()
	if tokenResp.StatusCode != http.StatusCreated || tokenOut.Token == "" || tokenOut.Kind != "project" {
		t.Fatalf("create token status=%d body=%+v", tokenResp.StatusCode, tokenOut)
	}
	if !strings.HasPrefix(tokenOut.Token, "herald_alpha_") {
		t.Fatalf("token = %q, want project token prefix", tokenOut.Token)
	}

	// The token list carries metadata only: no raw secret.
	tokensBody := f.listTokens(t)
	if strings.Contains(tokensBody, tokenOut.Token) {
		t.Fatalf("token list leaked the raw token: %s", tokensBody)
	}
	if !strings.Contains(tokensBody, `"kind":"project"`) || !strings.Contains(tokensBody, `"alpha"`) {
		t.Fatalf("token list missing metadata: %s", tokensBody)
	}

	// A live sender authenticates before rotation and after.
	auth := app.Authenticator{Store: f.creds, Hasher: shaHasher{}}
	if _, err := auth.Authenticate(context.Background(), tokenOut.Token, "alpha"); err != nil {
		t.Fatalf("token did not authenticate before rotation: %v", err)
	}

	// Rotate: the old secret stops working and only the new one does.
	rotResp := f.postJSON(t, http.MethodPost, "/v1/projects/alpha/token/rotate", csrf, "")
	var rotated struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(rotResp.Body).Decode(&rotated); err != nil {
		t.Fatal(err)
	}
	rotResp.Body.Close()
	if rotResp.StatusCode != http.StatusCreated || rotated.Token == "" || rotated.Token == tokenOut.Token {
		t.Fatalf("rotate status=%d body=%+v", rotResp.StatusCode, rotated)
	}
	if _, err := auth.Authenticate(context.Background(), tokenOut.Token, "alpha"); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("old token still authenticates after rotation: %v", err)
	}
	if _, err := auth.Authenticate(context.Background(), rotated.Token, "alpha"); err != nil {
		t.Fatalf("new token does not authenticate: %v", err)
	}

	// Revoke: the replacement stops working too.
	hash := shaHasher{}.Hash(rotated.Token)
	delResp := f.do(t, http.MethodDelete, "/v1/tokens/"+hash, csrf)
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke = %d, want 204", delResp.StatusCode)
	}
	if _, err := auth.Authenticate(context.Background(), rotated.Token, "alpha"); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("revoked token still authenticates: %v", err)
	}

	// Delete the project (needs CSRF).
	if r := f.do(t, http.MethodDelete, "/v1/projects/alpha", ""); r.StatusCode != http.StatusForbidden {
		t.Fatalf("delete without CSRF = %d, want 403", r.StatusCode)
	} else {
		r.Body.Close()
	}
	if r := f.do(t, http.MethodDelete, "/v1/projects/alpha", csrf); r.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", r.StatusCode)
	} else {
		r.Body.Close()
	}

	// Every mutation was audited with the actor and owning tenant, and the raw
	// secret never reached the log.
	logged := f.logs.String()
	for _, want := range []string{
		"action=project.create", "action=token.issue", "action=token.rotate",
		"action=token.revoke", "action=project.delete", "tenant=" + tenant,
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("audit log is missing %q:\n%s", want, logged)
		}
	}
	for _, secret := range []string{tokenOut.Token, rotated.Token} {
		if strings.Contains(logged, secret) {
			t.Fatalf("audit log leaked a raw token:\n%s", logged)
		}
	}
}

func TestCloudScopedTokenCannotEscalate(t *testing.T) {
	f := newCloudFixture(t, nil)
	csrf := f.signIn(t, "alice@example.com")

	if r := f.postJSON(t, http.MethodPost, "/v1/projects", csrf, `{"id":"alpha","name":"Alpha"}`); r.StatusCode != http.StatusCreated {
		t.Fatalf("create alpha = %d", r.StatusCode)
	} else {
		r.Body.Close()
	}

	// A scoped token naming a project the tenant does not own is refused, and
	// no credential is minted.
	resp := f.postJSON(t, http.MethodPost, "/v1/tokens", csrf, `{"projects":["alpha","foreign"]}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("escalating scoped token = %d, want 404", resp.StatusCode)
	}
	if creds, _ := f.creds.ByTenant(context.Background(), domain.TenantID(f.tenant(t))); len(creds) != 0 {
		t.Fatalf("a credential was minted for a foreign project: %+v", creds)
	}

	// The same request without the foreign project succeeds and the token is
	// scoped to exactly the named project.
	ok := f.postJSON(t, http.MethodPost, "/v1/tokens", csrf, `{"projects":["alpha"]}`)
	var out struct {
		Token    string   `json:"token"`
		Kind     string   `json:"kind"`
		Projects []string `json:"projects"`
	}
	if err := json.NewDecoder(ok.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	ok.Body.Close()
	if ok.StatusCode != http.StatusCreated || out.Kind != "scoped" {
		t.Fatalf("scoped create status=%d body=%+v", ok.StatusCode, out)
	}
	auth := app.Authenticator{Store: f.creds, Hasher: shaHasher{}}
	if _, err := auth.Authenticate(context.Background(), out.Token, "alpha"); err != nil {
		t.Fatalf("scoped token for alpha: %v", err)
	}
	if _, err := auth.Authenticate(context.Background(), out.Token, "half"); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("scoped token widened to an unlisted project: %v", err)
	}
}

func TestCloudProjectsPageRequiresSession(t *testing.T) {
	f := newCloudFixture(t, nil)

	resp := f.do(t, http.MethodGet, "/dashboard", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("page without session = %d, want 401", resp.StatusCode)
	}

	f.signIn(t, "alice@example.com")
	resp = f.do(t, http.MethodGet, "/dashboard", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content type = %q", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "Herald dashboard") {
		t.Fatal("page body is missing its heading")
	}
}

// listTokens returns the raw JSON body of the token listing.
func (f *cloudFixture) listTokens(t *testing.T) string {
	t.Helper()
	resp := f.do(t, http.MethodGet, "/v1/tokens", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list tokens = %d", resp.StatusCode)
	}
	var sb strings.Builder
	if _, err := io.Copy(&sb, resp.Body); err != nil {
		t.Fatal(err)
	}
	return sb.String()
}
