package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/adapters/httpapi"
	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// --- in-memory ownership doubles (the httpapi tests are black-box, so the
// package's own app fakes are not reachable). Projects reuse the fakeProjects
// store from cloud_projects_test.go ---

type memDestinations struct {
	byID map[domain.DestinationID]domain.Destination
}

func newMemDestinations() *memDestinations {
	return &memDestinations{byID: map[domain.DestinationID]domain.Destination{}}
}

func (s *memDestinations) PutDestination(_ context.Context, d domain.Destination) error {
	if existing, ok := s.byID[d.ID]; ok && !existing.OwnedBy(d.TenantID, d.ProjectID) {
		return app.ErrDestinationNotFound
	}
	s.byID[d.ID] = d
	return nil
}

func (s *memDestinations) Destination(_ context.Context, tenant domain.TenantID, project domain.ProjectID, id domain.DestinationID) (domain.Destination, bool, error) {
	d, ok := s.byID[id]
	if !ok || !d.OwnedBy(tenant, project) {
		return domain.Destination{}, false, nil
	}
	return d, true, nil
}

func (s *memDestinations) DestinationsByProject(_ context.Context, tenant domain.TenantID, project domain.ProjectID) ([]domain.Destination, error) {
	var out []domain.Destination
	for _, d := range s.byID {
		if d.OwnedBy(tenant, project) {
			out = append(out, d)
		}
	}
	return out, nil
}

func (s *memDestinations) DeleteDestination(_ context.Context, tenant domain.TenantID, project domain.ProjectID, id domain.DestinationID) error {
	d, ok := s.byID[id]
	if !ok || !d.OwnedBy(tenant, project) {
		return app.ErrDestinationNotFound
	}
	delete(s.byID, id)
	return nil
}

func (s *memDestinations) MarkVerified(_ context.Context, tenant domain.TenantID, project domain.ProjectID, id domain.DestinationID, now time.Time) error {
	d, ok := s.byID[id]
	if !ok || !d.OwnedBy(tenant, project) {
		return app.ErrDestinationNotFound
	}
	d.VerifiedAt = now
	s.byID[id] = d
	return nil
}

type memDestVerifications struct {
	byHash map[string]app.DestinationVerificationRecord
}

func newMemDestVerifications() *memDestVerifications {
	return &memDestVerifications{byHash: map[string]app.DestinationVerificationRecord{}}
}

func (s *memDestVerifications) PutDestinationVerification(_ context.Context, r app.DestinationVerificationRecord) error {
	s.byHash[r.Hash] = r
	return nil
}

func (s *memDestVerifications) ConsumeDestinationVerification(_ context.Context, hash string, now time.Time) (app.DestinationVerificationRecord, error) {
	r, ok := s.byHash[hash]
	if !ok || !r.UsedAt.IsZero() || !r.ExpiresAt.After(now) {
		return app.DestinationVerificationRecord{}, app.ErrDestinationVerificationNotFound
	}
	r.UsedAt = now
	s.byHash[hash] = r
	return r, nil
}

type destFixture struct {
	*cloudFixture
	projects *fakeProjects
	clock    *cloudClock
}

func newDestFixture(t *testing.T) *destFixture {
	t.Helper()
	clock := &cloudClock{t: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	auth := &app.Auth{
		Identities: newCloudIdentities(),
		Tokens:     newCloudTokens(),
		Sessions:   newCloudSessions(),
		Mailer:     &cloudMailer{},
		Secrets:    &fixedSecrets{},
		Hasher:     fixedHasher{},
		IDs:        &cloudIDs{},
		Clock:      clock,
		LinkBase:   "https://cloud.example.com",
	}
	mail := auth.Mailer.(*cloudMailer)
	projects := newFakeProjects()
	store := newMemDestinations()
	tokens := newMemDestVerifications()
	uc := &app.Destinations{
		Projects: projects,
		Store:    store,
		Tokens:   tokens,
		Mailer:   mail,
		Secrets:  &fixedSecrets{},
		Hasher:   fixedHasher{},
		IDs:      &cloudIDs{},
		Clock:    clock,
		LinkBase: "https://cloud.example.com",
	}
	base := &cloudFixture{mail: mail, auth: auth}
	api := &httpapi.Server{Cloud: &httpapi.Cloud{
		Auth:         *auth,
		Destinations: uc,
		CookieSecure: true,
		SessionTTL:   time.Hour,
	}}
	base.server = httptest.NewTLSServer(api.Handler())
	t.Cleanup(base.server.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	base.client = base.server.Client()
	base.client.Jar = jar

	return &destFixture{cloudFixture: base, projects: projects, clock: clock}
}

// tenantID reads the signed-in session's tenant so a test can seed a project
// the session owns.
func (f *destFixture) tenantID(t *testing.T) domain.TenantID {
	t.Helper()
	resp := f.do(t, http.MethodGet, "/v1/me", "")
	defer resp.Body.Close()
	var body struct {
		User struct {
			TenantID string `json:"tenantId"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return domain.TenantID(body.User.TenantID)
}

func (f *destFixture) send(t *testing.T, method, path, body, csrf string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, f.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		req.Header.Set(httpapi.CSRFHeader, csrf)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// verificationToken extracts the raw verification token from the last mailed
// link.
func (f *destFixture) verificationToken(t *testing.T) string {
	t.Helper()
	if len(f.mail.msgs) == 0 {
		t.Fatal("no mail sent")
	}
	body := f.mail.msgs[len(f.mail.msgs)-1].Text
	_, rest, ok := strings.Cut(body, "token=")
	if !ok {
		t.Fatalf("mail body = %q", body)
	}
	if i := strings.IndexAny(rest, " \t\r\n"); i >= 0 {
		rest = rest[:i]
	}
	raw, err := url.QueryUnescape(rest)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type destinationBody struct {
	Destination struct {
		ID       string `json:"id"`
		Kind     string `json:"kind"`
		Target   string `json:"target"`
		Enabled  bool   `json:"enabled"`
		Verified bool   `json:"verified"`
	} `json:"destination"`
}

func decodeDestination(t *testing.T, resp *http.Response) destinationBody {
	t.Helper()
	defer resp.Body.Close()
	var out destinationBody
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDestinationRoutesAbsentWithoutUseCase(t *testing.T) {
	// A Cloud with auth but no destination surface leaves the routes 404.
	api := &httpapi.Server{Cloud: &httpapi.Cloud{
		Auth: app.Auth{Mailer: &cloudMailer{}},
	}}
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/projects/alpha/destinations"},
		{http.MethodPost, "/v1/projects/alpha/destinations"},
		{http.MethodDelete, "/v1/projects/alpha/destinations/dst_x"},
		{http.MethodPost, "/v1/projects/alpha/destinations/dst_x/verify"},
		{http.MethodGet, "/v1/destinations/verify?token=x"},
	} {
		req, _ := http.NewRequest(tc.method, ts.URL+tc.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", tc.method, tc.path, resp.StatusCode)
		}
	}
}

func TestCloudDestinationLifecycle(t *testing.T) {
	f := newDestFixture(t)
	csrf := f.signIn(t, "alice@example.com")
	f.projects.PutProject(context.Background(), testHTTPProject("alpha", f.tenantID(t)))

	create := f.send(t, http.MethodPost, "/v1/projects/alpha/destinations",
		`{"kind":"email","target":"Ops@Example.COM"}`, csrf)
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", create.StatusCode)
	}
	dst := decodeDestination(t, create)
	if dst.Destination.Verified || !dst.Destination.Enabled || dst.Destination.Target != "Ops@example.com" {
		t.Fatalf("created = %+v, want enabled unverified normalized address", dst.Destination)
	}

	// List shows exactly the one destination.
	list := f.do(t, http.MethodGet, "/v1/projects/alpha/destinations", "")
	if list.StatusCode != http.StatusOK {
		t.Fatalf("list = %d", list.StatusCode)
	}
	var listing struct {
		Destinations []struct {
			Verified bool `json:"verified"`
		} `json:"destinations"`
	}
	if err := json.NewDecoder(list.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	list.Body.Close()
	if len(listing.Destinations) != 1 || listing.Destinations[0].Verified {
		t.Fatalf("listing = %+v", listing)
	}

	// Confirmation is token-only: a client with no session cookie can redeem it.
	raw := f.verificationToken(t)
	anon := f.server.Client()
	confirm, err := anon.Get(f.server.URL + "/v1/destinations/verify?token=" + url.QueryEscape(raw))
	if err != nil {
		t.Fatal(err)
	}
	if confirm.StatusCode != http.StatusOK {
		t.Fatalf("confirm = %d, want 200", confirm.StatusCode)
	}
	confirm.Body.Close()

	list2 := f.do(t, http.MethodGet, "/v1/projects/alpha/destinations", "")
	var after struct {
		Destinations []struct {
			Verified bool `json:"verified"`
		} `json:"destinations"`
	}
	if err := json.NewDecoder(list2.Body).Decode(&after); err != nil {
		t.Fatal(err)
	}
	list2.Body.Close()
	if len(after.Destinations) != 1 || !after.Destinations[0].Verified {
		t.Fatalf("after confirm = %+v", after)
	}

	// Delete removes it.
	del := f.send(t, http.MethodDelete, "/v1/projects/alpha/destinations/"+dst.Destination.ID, "", csrf)
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", del.StatusCode)
	}
	del.Body.Close()
}

func TestCloudDestinationRequiresSessionAndCSRF(t *testing.T) {
	f := newDestFixture(t)
	// No session.
	if resp := f.send(t, http.MethodPost, "/v1/projects/alpha/destinations", `{"kind":"email","target":"a@b.example"}`, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no session = %d, want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	csrf := f.signIn(t, "alice@example.com")
	f.projects.PutProject(context.Background(), testHTTPProject("alpha", f.tenantID(t)))
	// Session, no CSRF.
	if resp := f.send(t, http.MethodPost, "/v1/projects/alpha/destinations", `{"kind":"email","target":"a@b.example"}`, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no csrf = %d, want 403", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := f.send(t, http.MethodDelete, "/v1/projects/alpha/destinations/dst_x", "", ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("delete no csrf = %d, want 403", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// With the CSRF token the request reaches the use case.
	if resp := f.send(t, http.MethodPost, "/v1/projects/alpha/destinations", `{"kind":"email","target":"a@b.example"}`, csrf); resp.StatusCode != http.StatusCreated {
		t.Fatalf("valid csrf create = %d, want 201", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

func TestCloudDestinationOwnershipComesFromSession(t *testing.T) {
	f := newDestFixture(t)
	csrf := f.signIn(t, "alice@example.com")
	before := len(f.mail.msgs)
	// Seed "alpha" under a different tenant. The session cannot address it,
	// even though the slug matches.
	f.projects.PutProject(context.Background(), testHTTPProject("alpha", "ten_other"))

	resp := f.send(t, http.MethodPost, "/v1/projects/alpha/destinations",
		`{"kind":"email","target":"ops@example.com"}`, csrf)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign project create = %d, want 404", resp.StatusCode)
	}
	if len(f.mail.msgs) != before {
		t.Fatal("foreign project triggered mail")
	}
}

func TestCloudConfirmVerificationRejectsExpired(t *testing.T) {
	f := newDestFixture(t)
	csrf := f.signIn(t, "alice@example.com")
	f.projects.PutProject(context.Background(), testHTTPProject("alpha", f.tenantID(t)))
	create := f.send(t, http.MethodPost, "/v1/projects/alpha/destinations",
		`{"kind":"email","target":"ops@example.com"}`, csrf)
	create.Body.Close()
	raw := f.verificationToken(t)
	f.clock.t = f.clock.t.Add(app.DefaultVerificationTTL + time.Second)

	confirm, err := f.server.Client().Get(f.server.URL + "/v1/destinations/verify?token=" + url.QueryEscape(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer confirm.Body.Close()
	if confirm.StatusCode != http.StatusNotFound {
		t.Fatalf("expired confirm = %d, want 404", confirm.StatusCode)
	}
}

func testHTTPProject(id domain.ProjectID, tenant domain.TenantID) domain.Project {
	return domain.Project{ID: id, TenantID: tenant, Name: string(id), CreatedAt: time.Now().UTC()}
}
