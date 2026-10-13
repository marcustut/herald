package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/adapters/hash"
	"github.com/marcustut/herald/internal/adapters/httpapi"
	"github.com/marcustut/herald/internal/adapters/id"
	"github.com/marcustut/herald/internal/adapters/postgres"
	"github.com/marcustut/herald/internal/adapters/secret"
	"github.com/marcustut/herald/internal/adapters/sink"
	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/config"
	"github.com/marcustut/herald/internal/domain"
)

// acceptanceHarness is the M2 acceptance surface: the cloudHarness sign-in and
// session machinery, extended with the #31 destination use case and a #32
// DB-backed SinkSource, so a token-submitted event is dispatched as a real
// report email through a recording transport. It reuses cloudHarness's signIn,
// newClient and get unchanged by embedding it.
type acceptanceHarness struct {
	*cloudHarness
	events    *postgres.Events
	clock     *fakeClock
	transport *recordingTransport
	source    app.SinkSource
}

func newAcceptanceHarness(t *testing.T) *acceptanceHarness {
	t.Helper()
	pool, _ := openMigrated(t)
	mail := &authMailer{}
	clock := newFakeClock()
	store := postgres.NewOwnership(pool)
	events := postgres.NewEvents(pool)
	creds := postgres.NewCredentials(pool)

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
	destinations := &app.Destinations{
		Projects: store,
		Store:    store,
		Tokens:   postgres.NewDestinationVerifications(pool),
		Mailer:   mail,
		Secrets:  secret.Generator{},
		Hasher:   hash.SHA256{},
		IDs:      id.Random{},
		Clock:    clock,
		LinkBase: "https://cloud.example.com",
	}
	api := &httpapi.Server{
		Auth:   app.Authenticator{Store: creds, Hasher: hash.SHA256{}},
		Submit: app.SubmitEvent{Events: events, Sinks: store, IDs: id.Random{}, Clock: clock},
		Cloud: &httpapi.Cloud{
			Auth: auth,
			Projects: app.Projects{
				Store:       store,
				Credentials: creds,
				Issuer:      app.Issuer{Store: creds, Hasher: hash.SHA256{}, Secrets: secret.Generator{}, Clock: clock},
				Clock:       clock,
			},
			Destinations: destinations,
			CookieSecure: true,
			SessionTTL:   time.Hour,
		},
	}
	ts := httptest.NewTLSServer(api.Handler())
	t.Cleanup(ts.Close)

	transport := &recordingTransport{}
	cfg := config.Gateway{SMTP: config.SMTP{Addr: "127.0.0.1:25", From: "herald@example.com"}}
	source := sink.NewDestinationSource(store, cfg, "", "").
		WithMailTransport(transport).
		WithSuppressions(postgres.NewSuppressions(pool))

	return &acceptanceHarness{
		cloudHarness: &cloudHarness{server: ts, mail: mail},
		events:       events,
		clock:        clock,
		transport:    transport,
		source:       source,
	}
}

// json drives one cloud request. A non-zero csrf adds the synchronizer header;
// a non-empty body is sent as JSON. The response is decoded into out on success.
func (h *acceptanceHarness) json(t *testing.T, client *http.Client, method, path, csrf, body string, out any) int {
	t.Helper()
	req, err := http.NewRequest(method, h.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		req.Header.Set(httpapi.CSRFHeader, csrf)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

func (h *acceptanceHarness) createProject(t *testing.T, client *http.Client, csrf, id, name string) {
	t.Helper()
	var out struct {
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	}
	got := h.json(t, client, http.MethodPost, "/v1/projects", csrf,
		fmt.Sprintf(`{"id":%q,"name":%q}`, id, name), &out)
	if got != http.StatusCreated || out.Project.ID != id {
		t.Fatalf("create project %s = %d id=%q, want 201", id, got, out.Project.ID)
	}
}

func (h *acceptanceHarness) mintToken(t *testing.T, client *http.Client, csrf, project string) string {
	t.Helper()
	var out struct {
		Token string `json:"token"`
	}
	got := h.json(t, client, http.MethodPost, "/v1/projects/"+project+"/token", csrf, "", &out)
	if got != http.StatusCreated || out.Token == "" {
		t.Fatalf("mint token for %s = %d token=%q, want 201", project, got, out.Token)
	}
	return out.Token
}

func (h *acceptanceHarness) createDestination(t *testing.T, client *http.Client, csrf, project, kind, target string) domain.DestinationID {
	t.Helper()
	var out struct {
		Destination struct {
			ID string `json:"id"`
		} `json:"destination"`
	}
	got := h.json(t, client, http.MethodPost, "/v1/projects/"+project+"/destinations", csrf,
		fmt.Sprintf(`{"kind":%q,"target":%q}`, kind, target), &out)
	if got != http.StatusCreated || out.Destination.ID == "" {
		t.Fatalf("create %s destination for %s = %d id=%q, want 201", kind, project, got, out.Destination.ID)
	}
	return domain.DestinationID(out.Destination.ID)
}

// verifyLastDestination redeems the verification link the fake mailer captured
// for the most recently created email destination.
func (h *acceptanceHarness) verifyLastDestination(t *testing.T, client *http.Client) {
	t.Helper()
	raw := h.mail.lastToken(t)
	resp := h.get(t, client, "/v1/destinations/verify?token="+url.QueryEscape(raw))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("verify destination = %d, want 200", resp.StatusCode)
	}
}

// submitWithToken posts a real event through the HTTP surface, authenticated by
// the freshly minted project token, and returns the accepted event id.
func (h *acceptanceHarness) submitWithToken(t *testing.T, client *http.Client, token, project string) domain.EventID {
	t.Helper()
	body := fmt.Sprintf(`{"project":%q,"status":"success","title":"deploy","body":"done","agent":"herdr","occurrenceId":%q}`,
		project, "occ-"+project)
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/v1/events", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit to %s = %d, want 202", project, resp.StatusCode)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.ID == "" {
		t.Fatal("submit returned an empty event id")
	}
	return domain.EventID(out.ID)
}

func (h *acceptanceHarness) deliveries(t *testing.T) []domain.Delivery {
	t.Helper()
	rows, err := h.events.ListDeliveries(context.Background(), app.DeliveryFilter{}, 0)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	return rows
}

func countRecipient(got []string, want string) int {
	n := 0
	for _, x := range got {
		if x == want {
			n++
		}
	}
	return n
}

// TestIntegrationM2CloudAcceptance is the M2 milestone acceptance artifact. It
// drives the whole hosted flow against real PostgreSQL with no network: tenant A
// signs in through the magic-link round trip, creates a project, mints a project
// token, creates an email destination and verifies it, submits an event with the
// token, and the dispatcher delivers the report email to the verified recipient
// only. Tenant B proves the isolation boundary: it cannot read or mutate A's
// project or token, and its submission never reaches A's destination. A's
// unverified destination receives nothing.
func TestIntegrationM2CloudAcceptance(t *testing.T) {
	h := newAcceptanceHarness(t)
	ctx := context.Background()

	// Tenant A: signup -> project -> token -> destination -> event -> email.
	alice := h.newClient(t)
	aliceID := h.signIn(t, alice, "alice@example.com")

	h.createProject(t, alice, aliceID.CSRF, "axis", "Axis")
	aliceToken := h.mintToken(t, alice, aliceID.CSRF, "axis")

	// An unverified destination is created and left unconfirmed; it must never
	// be routed to. Created first so the verified one's link is mailed last.
	unverifiedID := h.createDestination(t, alice, aliceID.CSRF, "axis", "email", "never@example.com")
	verifiedID := h.createDestination(t, alice, aliceID.CSRF, "axis", "email", "ok@example.com")
	h.verifyLastDestination(t, alice)

	h.submitWithToken(t, alice, aliceToken, "axis")

	if n, err := hostedDispatcher(h.events, h.source, h.clock).ProcessBatch(ctx); err != nil || n != 1 {
		t.Fatalf("alice dispatch n=%d err=%v, want 1", n, err)
	}
	if got := h.transport.recipients(); len(got) != 1 || got[0] != "ok@example.com" {
		t.Fatalf("alice recipients = %v, want [ok@example.com]", got)
	}
	aliceDeliveries := h.deliveries(t)
	if len(aliceDeliveries) != 1 {
		t.Fatalf("alice deliveries = %+v, want exactly one", aliceDeliveries)
	}
	if d := aliceDeliveries[0]; d.SinkID != domain.SinkID(verifiedID) || d.State != domain.DeliveryDelivered {
		t.Fatalf("alice delivery = %+v, want the verified destination delivered", d)
	}

	// Tenant B: a fresh identity, isolated from A.
	bob := h.newClient(t)
	bobID := h.signIn(t, bob, "bob@example.com")
	if bobID.TenantID == aliceID.TenantID || bobID.Account == aliceID.Account {
		t.Fatalf("bob shares a tenant/account with alice: bob=%+v alice=%+v", bobID, aliceID)
	}

	// B cannot read A's destinations, mint a token for A's project, delete A's
	// project, or revoke A's token: every one is a 404, never another's row.
	if got := h.json(t, bob, http.MethodGet, "/v1/projects/axis/destinations", "", "", nil); got != http.StatusNotFound {
		t.Fatalf("bob read alice destinations = %d, want 404", got)
	}
	if got := h.json(t, bob, http.MethodPost, "/v1/projects/axis/token", bobID.CSRF, "", nil); got != http.StatusNotFound {
		t.Fatalf("bob mint alice token = %d, want 404", got)
	}
	if got := h.json(t, bob, http.MethodDelete, "/v1/projects/axis", bobID.CSRF, "", nil); got != http.StatusNotFound {
		t.Fatalf("bob delete alice project = %d, want 404", got)
	}
	aliceTokenID := hash.SHA256{}.Hash(aliceToken)
	if got := h.json(t, bob, http.MethodDelete, "/v1/tokens/"+aliceTokenID, bobID.CSRF, "", nil); got != http.StatusNotFound {
		t.Fatalf("bob revoke alice token = %d, want 404", got)
	}

	// B gets its own project, token and verified destination, so its submission
	// really queues a delivery: the tenant boundary must keep it off A's address.
	h.createProject(t, bob, bobID.CSRF, "beta", "Beta")
	bobToken := h.mintToken(t, bob, bobID.CSRF, "beta")
	h.createDestination(t, bob, bobID.CSRF, "beta", "email", "bob@example.com")
	h.verifyLastDestination(t, bob)
	h.submitWithToken(t, bob, bobToken, "beta")

	// B's token is scoped to beta, so it cannot even name A's project.
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/v1/events",
		strings.NewReader(`{"project":"axis","status":"success","title":"x","agent":"herdr"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bobToken)
	resp, err := bob.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bob submit to alice project = %d, want 401", resp.StatusCode)
	}

	if n, err := hostedDispatcher(h.events, h.source, h.clock).ProcessBatch(ctx); err != nil || n != 1 {
		t.Fatalf("bob dispatch n=%d err=%v, want 1", n, err)
	}
	got := h.transport.recipients()
	if countRecipient(got, "ok@example.com") != 1 || countRecipient(got, "bob@example.com") != 1 {
		t.Fatalf("recipients = %v, want exactly one each to ok@example.com and bob@example.com", got)
	}
	// A's unverified destination was never routed to, so nothing was ever sent
	// to its address.
	if countRecipient(got, "never@example.com") != 0 {
		t.Fatalf("unverified destination received mail: %v", got)
	}
	for _, d := range h.deliveries(t) {
		if d.SinkID == domain.SinkID(unverifiedID) {
			t.Fatalf("unverified destination %s was routed to: %+v", unverifiedID, d)
		}
	}
}
