package postgres_test

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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/marcustut/herald/internal/adapters/hash"
	"github.com/marcustut/herald/internal/adapters/httpapi"
	"github.com/marcustut/herald/internal/adapters/id"
	"github.com/marcustut/herald/internal/adapters/postgres"
	"github.com/marcustut/herald/internal/adapters/secret"
	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// authMailer captures magic-link mail so the integration tests can redeem the
// link without an SMTP server.
type authMailer struct{ msgs []app.MailMessage }

func (m *authMailer) SendMail(_ context.Context, msg app.MailMessage) error {
	m.msgs = append(m.msgs, msg)
	return nil
}

func (m *authMailer) lastToken(t *testing.T) string {
	t.Helper()
	if len(m.msgs) == 0 {
		t.Fatal("no magic link mailed")
	}
	_, rest, ok := strings.Cut(m.msgs[len(m.msgs)-1].Text, "token=")
	if !ok {
		t.Fatalf("mail body = %q", m.msgs[len(m.msgs)-1].Text)
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

// cloudHarness wires the real postgres auth stores and ownership store to the
// HTTP cloud surface, with a fake mailer standing in for SMTP. Auth and Submit
// are wired too so a token issued through the projects surface can drive a real
// event submission.
type cloudHarness struct {
	pool      *pgxpool.Pool
	server    *httptest.Server
	mail      *authMailer
	ownership *postgres.Ownership
	creds     *postgres.Credentials
}

func newCloudHarness(t *testing.T) *cloudHarness {
	t.Helper()
	pool, _ := openMigrated(t)
	mail := &authMailer{}
	clock := newFakeClock()
	creds := postgres.NewCredentials(pool)
	events := postgres.NewEvents(pool)
	auth := app.Auth{
		Identities: postgres.NewIdentities(pool),
		Tokens:     postgres.NewLoginTokens(pool),
		Sessions:   postgres.NewSessions(pool),
		Mailer:     mail,
		Secrets:    secret.Generator{},
		Hasher:     hash.SHA256{},
		IDs:        id.Random{},
		Clock:      clock,
		LinkBase:   "https://cloud.example.com",
	}
	api := &httpapi.Server{
		Auth: app.Authenticator{Store: creds, Hasher: hash.SHA256{}},
		Submit: app.SubmitEvent{
			Events: events,
			Sinks:  fixedSinks{"email"},
			IDs:    id.Random{},
			Clock:  clock,
		},
		Cloud: &httpapi.Cloud{
			Auth: auth,
			Projects: app.Projects{
				Store:       postgres.NewOwnership(pool),
				Credentials: creds,
				Issuer:      app.Issuer{Store: creds, Hasher: hash.SHA256{}, Secrets: secret.Generator{}, Clock: clock},
				Clock:       clock,
			},
			CookieSecure: true,
			SessionTTL:   time.Hour,
		},
	}
	ts := httptest.NewTLSServer(api.Handler())
	t.Cleanup(ts.Close)
	return &cloudHarness{
		pool:      pool,
		server:    ts,
		mail:      mail,
		ownership: postgres.NewOwnership(pool),
		creds:     creds,
	}
}

// newClient returns an independent cookie jar, so two users are two sessions.
func (h *cloudHarness) newClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: h.server.Client().Transport, Jar: jar}
}

type cloudIdentity struct {
	CSRF     string
	UserID   string
	Email    string
	Account  string
	TenantID string
}

// signIn runs the full magic-link round trip for one client.
func (h *cloudHarness) signIn(t *testing.T, client *http.Client, email string) cloudIdentity {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/auth/magic-link",
		strings.NewReader(`{"email":"`+email+`"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("magic-link status = %d", resp.StatusCode)
	}
	raw := h.mail.lastToken(t)
	cb, err := client.Get(h.server.URL + "/v1/auth/callback?token=" + url.QueryEscape(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Body.Close()
	if cb.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d", cb.StatusCode)
	}
	var out struct {
		CSRF string `json:"csrfToken"`
		User struct {
			ID        string `json:"id"`
			Email     string `json:"email"`
			AccountID string `json:"accountId"`
			TenantID  string `json:"tenantId"`
		} `json:"user"`
	}
	if err := json.NewDecoder(cb.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.CSRF == "" || out.User.TenantID == "" {
		t.Fatalf("callback body = %+v", out)
	}
	return cloudIdentity{
		CSRF:     out.CSRF,
		UserID:   out.User.ID,
		Email:    out.User.Email,
		Account:  out.User.AccountID,
		TenantID: out.User.TenantID,
	}
}

func (h *cloudHarness) get(t *testing.T, client *http.Client, path string) *http.Response {
	t.Helper()
	resp, err := client.Get(h.server.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestIntegrationMagicLinkSessionLifecycle drives register/sign-in/sign-out
// against real PostgreSQL: a link works once, starts a hardened session, and a
// replay is refused.
func TestIntegrationMagicLinkSessionLifecycle(t *testing.T) {
	h := newCloudHarness(t)
	client := h.newClient(t)
	id := h.signIn(t, client, "alice@example.com")

	me := h.get(t, client, "/v1/me")
	me.Body.Close()
	if me.StatusCode != http.StatusOK {
		t.Fatalf("/me = %d, want 200", me.StatusCode)
	}

	// The same link cannot be redeemed twice.
	raw := h.mail.lastToken(t)
	replay, err := client.Get(h.server.URL + "/v1/auth/callback?token=" + url.QueryEscape(raw))
	if err != nil {
		t.Fatal(err)
	}
	replay.Body.Close()
	if replay.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replay = %d, want 401", replay.StatusCode)
	}

	// Sign out revokes the session server-side.
	outReq, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/auth/signout", nil)
	outReq.Header.Set(httpapi.CSRFHeader, id.CSRF)
	out, err := client.Do(outReq)
	if err != nil {
		t.Fatal(err)
	}
	out.Body.Close()
	if out.StatusCode != http.StatusNoContent {
		t.Fatalf("signout = %d, want 204", out.StatusCode)
	}
	after := h.get(t, client, "/v1/me")
	after.Body.Close()
	if after.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/me after signout = %d, want 401", after.StatusCode)
	}
}

// TestIntegrationAccountDeletion isolates one account and deletes it: the
// session is revoked and every owned row cascades away, while a later sign-in
// with the same address provisions a fresh account.
func TestIntegrationAccountDeletion(t *testing.T) {
	h := newCloudHarness(t)
	client := h.newClient(t)
	id := h.signIn(t, client, "alice@example.com")

	project, err := domain.NewProject("alpha", domain.TenantID(id.TenantID), "Alpha", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.ownership.PutProject(context.Background(), project); err != nil {
		t.Fatalf("put project: %v", err)
	}

	req, _ := http.NewRequest(http.MethodDelete, h.server.URL+"/v1/account", nil)
	req.Header.Set(httpapi.CSRFHeader, id.CSRF)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", resp.StatusCode)
	}

	ctx := context.Background()
	for _, table := range []string{"accounts", "users", "tenants", "sessions", "projects"} {
		var count int
		if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s has %d rows after account deletion, want 0", table, count)
		}
	}

	// Signing in again recreates the identity rather than resurrecting the old
	// rows.
	again := h.newClient(t)
	id2 := h.signIn(t, again, "alice@example.com")
	if id2.Account == id.Account {
		t.Fatal("account id was reused after deletion")
	}
}

// TestIntegrationAuthorizationAcrossUsersAndProjects proves the isolation
// boundary end to end: two users land in different accounts and tenants, and
// one cannot read or delete the other's project even knowing its id.
func TestIntegrationAuthorizationAcrossUsersAndProjects(t *testing.T) {
	h := newCloudHarness(t)
	alice := h.signIn(t, h.newClient(t), "alice@example.com")
	bob := h.signIn(t, h.newClient(t), "bob@example.com")

	if alice.Account == bob.Account || alice.TenantID == bob.TenantID {
		t.Fatalf("users share an account/tenant: alice=%+v bob=%+v", alice, bob)
	}

	ctx := context.Background()
	secret, err := domain.NewProject("secret", domain.TenantID(alice.TenantID), "Secret", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.ownership.PutProject(ctx, secret); err != nil {
		t.Fatalf("put project: %v", err)
	}
	bobProject, err := domain.NewProject("bob-only", domain.TenantID(bob.TenantID), "Bob", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.ownership.PutProject(ctx, bobProject); err != nil {
		t.Fatalf("put bob project: %v", err)
	}

	// Alice owns secret; bob's tenant cannot see it.
	if _, ok, err := h.ownership.Project(ctx, domain.TenantID(alice.TenantID), "secret"); err != nil || !ok {
		t.Fatalf("alice project read: ok=%v err=%v", ok, err)
	}
	if _, ok, err := h.ownership.Project(ctx, domain.TenantID(bob.TenantID), "secret"); err != nil || ok {
		t.Fatalf("bob read alice's project: ok=%v err=%v", ok, err)
	}
	if err := h.ownership.DeleteProject(ctx, domain.TenantID(bob.TenantID), "secret"); err == nil {
		t.Fatal("bob deleted alice's project")
	}
	if _, ok, _ := h.ownership.Project(ctx, domain.TenantID(alice.TenantID), "secret"); !ok {
		t.Fatal("alice's project disappeared after bob's cross-tenant delete")
	}
}
