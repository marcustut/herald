package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/marcustut/herald/internal/adapters/hash"
	"github.com/marcustut/herald/internal/adapters/httpapi"
	"github.com/marcustut/herald/internal/client"
)

// postJSON sends a JSON request as one browser session.
func (h *cloudHarness) postJSON(t *testing.T, c *http.Client, method, path, csrf, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, h.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		req.Header.Set(httpapi.CSRFHeader, csrf)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// createProject creates a project through the session-scoped API and fails the
// test if it does not answer 201.
func (h *cloudHarness) createProject(t *testing.T, c *http.Client, csrf, id, name string) {
	t.Helper()
	resp := h.postJSON(t, c, http.MethodPost, "/v1/projects", csrf, `{"id":"`+id+`","name":"`+name+`"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create project %s = %d", id, resp.StatusCode)
	}
}

// issueToken creates a project token and returns the raw secret.
func (h *cloudHarness) issueToken(t *testing.T, c *http.Client, csrf, project string) string {
	t.Helper()
	resp := h.postJSON(t, c, http.MethodPost, "/v1/projects/"+project+"/token", csrf, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("issue token for %s = %d", project, resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Token == "" {
		t.Fatal("issued token is empty")
	}
	return out.Token
}

// submit sends an event with a bearer token and returns the HTTP error if any.
func (h *cloudHarness) submit(t *testing.T, raw, project string) error {
	t.Helper()
	cl := client.Client{Gateway: h.server.URL, Token: raw, HTTP: h.server.Client()}
	_, err := cl.Submit(context.Background(), client.Event{
		Project: project, Status: "success", Title: "t", Body: "b", Agent: "herdr",
	})
	return err
}

// TestIntegrationTokenRotationAndRevocationAffectLiveSender proves a token
// issued through the API drives a real submission, and that rotating then
// revoking it takes effect on the next request: the credential lookup that
// authenticates a sender sees the revocation.
func TestIntegrationTokenRotationAndRevocationAffectLiveSender(t *testing.T) {
	h := newCloudHarness(t)
	c := h.newClient(t)
	alice := h.signIn(t, c, "alice@example.com")
	h.createProject(t, c, alice.CSRF, "alpha", "Alpha")

	first := h.issueToken(t, c, alice.CSRF, "alpha")
	if err := h.submit(t, first, "alpha"); err != nil {
		t.Fatalf("live sender before rotation: %v", err)
	}

	// Rotate: a new secret is minted and the old one is immediately refused.
	rot := h.postJSON(t, c, http.MethodPost, "/v1/projects/alpha/token/rotate", alice.CSRF, "")
	defer rot.Body.Close()
	if rot.StatusCode != http.StatusCreated {
		t.Fatalf("rotate = %d", rot.StatusCode)
	}
	var rotated struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(rot.Body).Decode(&rotated); err != nil {
		t.Fatal(err)
	}
	if rotated.Token == first {
		t.Fatal("rotation returned the same secret")
	}
	if err := h.submit(t, first, "alpha"); !isUnauthorized(err) {
		t.Fatalf("old token after rotation: err = %v, want 401", err)
	}
	if err := h.submit(t, rotated.Token, "alpha"); err != nil {
		t.Fatalf("rotated token submit: %v", err)
	}

	// Revoke the replacement by its (one-way) hash; the sender stops working.
	hashHex := hash.SHA256{}.Hash(rotated.Token)
	del := h.postJSON(t, c, http.MethodDelete, "/v1/tokens/"+hashHex, alice.CSRF, "")
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke = %d, want 204", del.StatusCode)
	}
	if err := h.submit(t, rotated.Token, "alpha"); !isUnauthorized(err) {
		t.Fatalf("revoked token submit: err = %v, want 401", err)
	}
}

// TestIntegrationProjectTenantIsolation proves one signed-in user cannot see,
// delete, or mint a token for another user's project, even knowing its slug.
func TestIntegrationProjectTenantIsolation(t *testing.T) {
	h := newCloudHarness(t)
	aliceClient := h.newClient(t)
	bobClient := h.newClient(t)
	alice := h.signIn(t, aliceClient, "alice@example.com")
	bob := h.signIn(t, bobClient, "bob@example.com")
	h.createProject(t, aliceClient, alice.CSRF, "alpha", "Alpha")

	if alice.TenantID == bob.TenantID {
		t.Fatal("two users share a tenant")
	}

	// Bob's listing is empty: alice's project is not visible.
	list := h.get(t, bobClient, "/v1/projects")
	defer list.Body.Close()
	if list.StatusCode != http.StatusOK {
		t.Fatalf("bob list = %d", list.StatusCode)
	}
	var body struct {
		Projects []json.RawMessage `json:"projects"`
	}
	if err := json.NewDecoder(list.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Projects) != 0 {
		t.Fatalf("bob sees %d projects, want 0", len(body.Projects))
	}

	// Every cross-tenant action is not-found, never another tenant's row.
	for _, tc := range []struct{ method, path, payload string }{
		{http.MethodDelete, "/v1/projects/alpha", ""},
		{http.MethodPost, "/v1/projects/alpha/token", ""},
		{http.MethodPost, "/v1/tokens", `{"projects":["alpha"]}`},
	} {
		resp := h.postJSON(t, bobClient, tc.method, tc.path, bob.CSRF, tc.payload)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("bob %s %s = %d, want 404", tc.method, tc.path, resp.StatusCode)
		}
	}

	// Alice's project survives untouched and is still listed for her.
	aliceList := h.get(t, aliceClient, "/v1/projects")
	defer aliceList.Body.Close()
	var owned struct {
		Projects []json.RawMessage `json:"projects"`
	}
	if err := json.NewDecoder(aliceList.Body).Decode(&owned); err != nil {
		t.Fatal(err)
	}
	if len(owned.Projects) != 1 {
		t.Fatalf("alice sees %d projects, want 1", len(owned.Projects))
	}
}

// TestIntegrationTokenListHidesSecret proves the metadata listing never returns
// the raw secret and identifies a token only by its one-way hash.
func TestIntegrationTokenListHidesSecret(t *testing.T) {
	h := newCloudHarness(t)
	c := h.newClient(t)
	alice := h.signIn(t, c, "alice@example.com")
	h.createProject(t, c, alice.CSRF, "alpha", "Alpha")
	raw := h.issueToken(t, c, alice.CSRF, "alpha")

	resp := h.get(t, c, "/v1/tokens")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list tokens = %d", resp.StatusCode)
	}
	var sb strings.Builder
	if _, err := io.Copy(&sb, resp.Body); err != nil {
		t.Fatal(err)
	}
	listing := sb.String()
	if strings.Contains(listing, raw) {
		t.Fatalf("token list leaked the raw secret: %s", listing)
	}
	want := hash.SHA256{}.Hash(raw)
	if !strings.Contains(listing, want) {
		t.Fatalf("token list is missing the hashed id: %s", listing)
	}
}

func isUnauthorized(err error) bool {
	var httpErr *client.HTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusUnauthorized
}
