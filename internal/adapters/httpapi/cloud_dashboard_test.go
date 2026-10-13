package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/adapters/httpapi"
	"github.com/marcustut/herald/internal/adapters/memory"
	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// fakeEventLog is an in-memory app.EventLog for the black-box HTTP tests. It
// scopes every query by the tenant argument (as the real store does) so a test
// that seeds a second tenant proves the handler cannot return its rows.
type fakeEventLog struct {
	events     []domain.Event
	deliveries []domain.Delivery

	lastTenant         domain.TenantID
	lastFilter         app.EventFilter
	lastDeliveryFilter app.DeliveryLogFilter
}

func (l *fakeEventLog) ListEvents(_ context.Context, tenant domain.TenantID, f app.EventFilter, limit, offset int) ([]domain.Event, error) {
	l.lastTenant = tenant
	l.lastFilter = f
	var out []domain.Event
	for _, e := range l.events {
		if e.TenantID != tenant {
			continue
		}
		if f.Project != "" && e.ProjectID != f.Project {
			continue
		}
		if f.Status != "" && e.Status != f.Status {
			continue
		}
		if !f.Since.IsZero() && e.CreatedAt.Before(f.Since) {
			continue
		}
		if !f.Until.IsZero() && !e.CreatedAt.Before(f.Until) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return pageOf(out, limit, offset), nil
}

func (l *fakeEventLog) ListDeliveryAttempts(_ context.Context, tenant domain.TenantID, f app.DeliveryLogFilter, limit, offset int) ([]domain.Delivery, error) {
	l.lastTenant = tenant
	l.lastDeliveryFilter = f
	byEvent := map[domain.EventID]domain.Event{}
	for _, e := range l.events {
		byEvent[e.ID] = e
	}
	var out []domain.Delivery
	for _, d := range l.deliveries {
		e, ok := byEvent[d.EventID]
		if !ok || e.TenantID != tenant {
			continue
		}
		if f.Project != "" && e.ProjectID != f.Project {
			continue
		}
		if f.State != "" && d.State != f.State {
			continue
		}
		if f.EventID != "" && d.EventID != f.EventID {
			continue
		}
		if !f.Since.IsZero() && d.CreatedAt.Before(f.Since) {
			continue
		}
		if !f.Until.IsZero() && !d.CreatedAt.Before(f.Until) {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return pageOf(out, limit, offset), nil
}

func pageOf[T any](rows []T, limit, offset int) []T {
	if offset > 0 {
		if offset >= len(rows) {
			return nil
		}
		rows = rows[offset:]
	}
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}

type dashboardFixture struct {
	*cloudFixture
	log *fakeEventLog
}

func newDashboardFixture(t *testing.T) *dashboardFixture {
	t.Helper()
	clock := cloudClock{t: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
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
	creds := memory.NewCredentials()
	log := &fakeEventLog{}
	base := &cloudFixture{mail: mail, auth: auth, creds: creds, projects: projects}
	api := &httpapi.Server{Cloud: &httpapi.Cloud{
		Auth:         *auth,
		Events:       log,
		CookieSecure: true,
		SessionTTL:   time.Hour,
		Projects: app.Projects{
			Store:       projects,
			Credentials: creds,
			Issuer: app.Issuer{
				Store:   creds,
				Hasher:  shaHasher{},
				Secrets: &fixedSecrets{},
				Clock:   clock,
			},
			Clock: clock,
		},
	}}
	base.server = httptest.NewTLSServer(api.Handler())
	t.Cleanup(base.server.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	base.client = base.server.Client()
	base.client.Jar = jar
	return &dashboardFixture{cloudFixture: base, log: log}
}

// seedEvent appends one event (and its delivery) to the fake log.
func (f *dashboardFixture) seedEvent(t *testing.T, tenant domain.TenantID, project domain.ProjectID, status domain.Status, at time.Time, title, lastError string) domain.Event {
	t.Helper()
	id := domain.EventID("evt_" + string(project) + "_" + string(status) + "_" + at.Format("150405.000"))
	e := domain.Event{
		ID: id, TenantID: tenant, ProjectID: project, AgentID: "herdr",
		Status: status, Title: title, Body: "body", CreatedAt: at,
	}
	d := domain.NewDelivery(domain.DeliveryID("del_"+string(id)), id, "email", at)
	d.LastError = lastError
	f.log.events = append(f.log.events, e)
	f.log.deliveries = append(f.log.deliveries, d)
	return e
}

func (f *dashboardFixture) getJSON(t *testing.T, path string, out any) int {
	t.Helper()
	resp := f.do(t, http.MethodGet, path, "")
	defer resp.Body.Close()
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

type eventsBody struct {
	Events []struct {
		ID      string `json:"id"`
		Project string `json:"project"`
		Status  string `json:"status"`
		Title   string `json:"title"`
	} `json:"events"`
	Page struct {
		Limit   int  `json:"limit"`
		Offset  int  `json:"offset"`
		HasMore bool `json:"hasMore"`
	} `json:"page"`
}

type deliveriesBody struct {
	Deliveries []struct {
		ID        string `json:"id"`
		EventID   string `json:"eventId"`
		State     string `json:"state"`
		Attempts  int    `json:"attempts"`
		LastError string `json:"lastError"`
	} `json:"deliveries"`
	Page struct {
		Limit   int  `json:"limit"`
		Offset  int  `json:"offset"`
		HasMore bool `json:"hasMore"`
	} `json:"page"`
}

func TestDashboardRoutesAbsentWithoutLog(t *testing.T) {
	api := &httpapi.Server{Cloud: &httpapi.Cloud{Auth: app.Auth{Mailer: &cloudMailer{}}}}
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()
	for _, path := range []string{"/cloud/v1/events", "/cloud/v1/deliveries"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 when no log is wired", path, resp.StatusCode)
		}
	}
}

func TestDashboardRequiresSession(t *testing.T) {
	f := newDashboardFixture(t)
	for _, path := range []string{"/cloud/v1/events", "/cloud/v1/deliveries"} {
		if code := f.getJSON(t, path, nil); code != http.StatusUnauthorized {
			t.Errorf("GET %s = %d, want 401", path, code)
		}
	}
}

func TestDashboardPageRendersReadOnlySections(t *testing.T) {
	f := newDashboardFixture(t)
	f.signIn(t, "alice@example.com")
	resp := f.do(t, http.MethodGet, "/cloud/projects", "")
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
	page := string(body)
	for _, want := range []string{
		"Recent events", "Delivery attempts", "Destinations",
		`<label for="ev-project">`, `<label for="del-state">`,
		`<caption>`, `class="tablewrap"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard page is missing %q", want)
		}
	}
}

func TestDashboardTenantIsolation(t *testing.T) {
	f := newDashboardFixture(t)
	f.signIn(t, "alice@example.com")
	tenant := domain.TenantID(f.tenant(t))
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

	mine := f.seedEvent(t, tenant, "alpha", domain.StatusSuccess, now, "mine", "")
	f.seedEvent(t, "ten_evil", "alpha", domain.StatusFailure, now.Add(-time.Hour), "theirs", "boom")

	// A client-supplied tenant and a project that belongs to another tenant
	// must not widen visibility.
	var body eventsBody
	code := f.getJSON(t, "/cloud/v1/events?tenant=ten_evil&project=alpha", &body)
	if code != http.StatusOK {
		t.Fatalf("events = %d", code)
	}
	if len(body.Events) != 1 || body.Events[0].ID != string(mine.ID) {
		t.Fatalf("events = %+v, want only the session tenant's row", body.Events)
	}

	var deliveries deliveriesBody
	code = f.getJSON(t, "/cloud/v1/deliveries?tenant=ten_evil", &deliveries)
	if code != http.StatusOK {
		t.Fatalf("deliveries = %d", code)
	}
	if len(deliveries.Deliveries) != 1 || deliveries.Deliveries[0].EventID != string(mine.ID) {
		t.Fatalf("deliveries = %+v, want only the session tenant's row", deliveries.Deliveries)
	}

	// The handler always scopes by the session tenant, never the query.
	if f.log.lastTenant != tenant {
		t.Fatalf("store tenant = %q, want session tenant %q", f.log.lastTenant, tenant)
	}
	if f.log.lastFilter.Project != "alpha" || f.log.lastFilter.Status != "" {
		t.Fatalf("store filter = %+v, want only the project narrowed", f.log.lastFilter)
	}
}

func TestDashboardFilters(t *testing.T) {
	f := newDashboardFixture(t)
	f.signIn(t, "alice@example.com")
	tenant := domain.TenantID(f.tenant(t))
	base := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

	f.seedEvent(t, tenant, "alpha", domain.StatusSuccess, base.Add(48*time.Hour), "ok", "")
	f.seedEvent(t, tenant, "alpha", domain.StatusFailure, base.Add(24*time.Hour), "bad", "smtp 550")
	f.seedEvent(t, tenant, "beta", domain.StatusSuccess, base, "old", "")

	var byStatus eventsBody
	f.getJSON(t, "/cloud/v1/events?status=failure", &byStatus)
	if len(byStatus.Events) != 1 || byStatus.Events[0].Status != "failure" {
		t.Fatalf("status filter = %+v", byStatus.Events)
	}

	var byProject eventsBody
	f.getJSON(t, "/cloud/v1/events?project=alpha", &byProject)
	if len(byProject.Events) != 2 {
		t.Fatalf("project filter = %+v", byProject.Events)
	}

	// A date-only until covers the whole day. Events at base and base+24h are
	// on 2026-10-10 and 2026-10-11.
	var byDate eventsBody
	f.getJSON(t, "/cloud/v1/events?since=2026-10-11&until=2026-10-12", &byDate)
	if len(byDate.Events) != 2 {
		t.Fatalf("date filter = %+v, want the 10-11 and 10-12 events", byDate.Events)
	}

	var deliveries deliveriesBody
	f.getJSON(t, "/cloud/v1/deliveries?state=dead", &deliveries)
	if len(deliveries.Deliveries) != 0 {
		t.Fatalf("delivery state filter = %+v, want none dead", deliveries.Deliveries)
	}
	if f.log.lastDeliveryFilter.State != domain.DeliveryDead {
		t.Fatalf("store saw state %q, want the requested state", f.log.lastDeliveryFilter.State)
	}
}

func TestDashboardPagination(t *testing.T) {
	f := newDashboardFixture(t)
	f.signIn(t, "alice@example.com")
	tenant := domain.TenantID(f.tenant(t))
	base := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		f.seedEvent(t, tenant, "alpha", domain.StatusSuccess, base.Add(time.Duration(i)*time.Hour), "e", "")
	}

	var first eventsBody
	f.getJSON(t, "/cloud/v1/events?limit=2&offset=0", &first)
	if len(first.Events) != 2 || !first.Page.HasMore {
		t.Fatalf("first page = %+v", first)
	}
	var second eventsBody
	f.getJSON(t, "/cloud/v1/events?limit=2&offset=2", &second)
	if len(second.Events) != 1 || second.Page.HasMore {
		t.Fatalf("second page = %+v", second)
	}
	if first.Events[0].ID == second.Events[0].ID {
		t.Fatal("pages overlap")
	}

	// limit is clamped, so an oversized page cannot exhaust the database.
	var clamped eventsBody
	f.getJSON(t, "/cloud/v1/events?limit=100000", &clamped)
	if clamped.Page.Limit != 100 {
		t.Fatalf("limit = %d, want 100", clamped.Page.Limit)
	}
}

func TestDashboardRedactsSecrets(t *testing.T) {
	f := newDashboardFixture(t)
	f.signIn(t, "alice@example.com")
	tenant := domain.TenantID(f.tenant(t))
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

	secret := "supersecretvalue123"
	f.seedEvent(t, tenant, "alpha", domain.StatusFailure, now,
		"deploy failed token: "+secret,
		"webhook https://user:hunter2@example.com failed api_key=abcd1234")

	var events eventsBody
	f.getJSON(t, "/cloud/v1/events", &events)
	if len(events.Events) != 1 {
		t.Fatalf("events = %+v", events.Events)
	}
	if strings.Contains(events.Events[0].Title, secret) || !strings.Contains(events.Events[0].Title, "[REDACTED]") {
		t.Fatalf("title not redacted: %q", events.Events[0].Title)
	}

	var deliveries deliveriesBody
	f.getJSON(t, "/cloud/v1/deliveries", &deliveries)
	if len(deliveries.Deliveries) != 1 {
		t.Fatalf("deliveries = %+v", deliveries.Deliveries)
	}
	last := deliveries.Deliveries[0].LastError
	for _, leaked := range []string{"hunter2", "abcd1234"} {
		if strings.Contains(last, leaked) {
			t.Fatalf("last error leaked %q: %q", leaked, last)
		}
	}
	if !strings.Contains(last, "[REDACTED]") {
		t.Fatalf("last error not redacted: %q", last)
	}
}

func TestDashboardRejectsUnknownFilters(t *testing.T) {
	f := newDashboardFixture(t)
	f.signIn(t, "alice@example.com")
	for _, path := range []string{
		"/cloud/v1/events?status=nope",
		"/cloud/v1/events?since=not-a-date",
		"/cloud/v1/events?until=2026-13-40",
		"/cloud/v1/deliveries?state=nope",
	} {
		if code := f.getJSON(t, path, nil); code != http.StatusUnprocessableEntity {
			t.Errorf("GET %s = %d, want 422", path, code)
		}
	}
}
