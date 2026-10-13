package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/adapters/httpapi"
	"github.com/marcustut/herald/internal/adapters/memory"
	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// --- in-memory auth doubles (httpapi package is black-box, so the app fakes
// used by internal/app are not reachable here) ---

type cloudIdentities struct{ byEmail map[string]app.Identity }

func newCloudIdentities() *cloudIdentities {
	return &cloudIdentities{byEmail: map[string]app.Identity{}}
}

func (s *cloudIdentities) EnsureUser(_ context.Context, email string, ids app.NewIdentityIDs, _ time.Time) (app.Identity, error) {
	if id, ok := s.byEmail[email]; ok {
		return id, nil
	}
	id := app.Identity{UserID: ids.User, AccountID: ids.Account, Email: email, TenantID: ids.Tenant}
	s.byEmail[email] = id
	return id, nil
}

func (s *cloudIdentities) DeleteAccount(_ context.Context, account domain.AccountID) error {
	for email, id := range s.byEmail {
		if id.AccountID == account {
			delete(s.byEmail, email)
		}
	}
	return nil
}

type cloudSessions struct{ byHash map[string]app.SessionRecord }

func newCloudSessions() *cloudSessions {
	return &cloudSessions{byHash: map[string]app.SessionRecord{}}
}

func (s *cloudSessions) PutSession(_ context.Context, r app.SessionRecord) error {
	s.byHash[r.Hash] = r
	return nil
}

func (s *cloudSessions) SessionByHash(_ context.Context, hash string, now time.Time) (app.SessionRecord, error) {
	r, ok := s.byHash[hash]
	if !ok || !r.RevokedAt.IsZero() || !r.ExpiresAt.After(now) {
		return app.SessionRecord{}, app.ErrSessionNotFound
	}
	return r, nil
}

func (s *cloudSessions) RevokeSession(_ context.Context, hash string, now time.Time) error {
	if r, ok := s.byHash[hash]; ok {
		r.RevokedAt = now
		s.byHash[hash] = r
	}
	return nil
}

func (s *cloudSessions) RevokeAccountSessions(_ context.Context, account domain.AccountID, now time.Time) error {
	for h, r := range s.byHash {
		if r.AccountID == account {
			r.RevokedAt = now
			s.byHash[h] = r
		}
	}
	return nil
}

type cloudTokens struct {
	byHash map[string]app.LoginTokenRecord
}

func newCloudTokens() *cloudTokens { return &cloudTokens{byHash: map[string]app.LoginTokenRecord{}} }

func (s *cloudTokens) PutLoginToken(_ context.Context, t app.LoginTokenRecord) error {
	s.byHash[t.Hash] = t
	return nil
}

func (s *cloudTokens) ConsumeLoginToken(_ context.Context, hash string, now time.Time) (app.LoginTokenRecord, error) {
	r, ok := s.byHash[hash]
	if !ok || !r.UsedAt.IsZero() || !r.ExpiresAt.After(now) {
		return app.LoginTokenRecord{}, app.ErrLoginTokenNotFound
	}
	r.UsedAt = now
	s.byHash[hash] = r
	return r, nil
}

type cloudMailer struct{ msgs []app.MailMessage }

func (m *cloudMailer) SendMail(_ context.Context, msg app.MailMessage) error {
	m.msgs = append(m.msgs, msg)
	return nil
}

// lastToken extracts the raw magic-link token from the last message.
func (m *cloudMailer) lastToken(t *testing.T) string {
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

type cloudIDs struct{ n int }

func (g *cloudIDs) newID(prefix string) string {
	g.n++
	return prefix + string(rune('a'+g.n%26))
}
func (g *cloudIDs) NewEventID() domain.EventID { return domain.EventID(g.newID("evt_")) }
func (g *cloudIDs) NewDeliveryID() domain.DeliveryID {
	return domain.DeliveryID(g.newID("del_"))
}
func (g *cloudIDs) NewAccountID() domain.AccountID { return domain.AccountID(g.newID("acct_")) }
func (g *cloudIDs) NewUserID() domain.UserID       { return domain.UserID(g.newID("usr_")) }
func (g *cloudIDs) NewTenantID() domain.TenantID   { return domain.TenantID(g.newID("ten_")) }
func (g *cloudIDs) NewDestinationID() domain.DestinationID {
	return domain.DestinationID(g.newID("dst_"))
}

type cloudClock struct{ t time.Time }

func (c cloudClock) Now() time.Time { return c.t }

type cloudFixture struct {
	server   *httptest.Server
	client   *http.Client
	mail     *cloudMailer
	auth     *app.Auth
	creds    *memory.Credentials
	projects *fakeProjects
	logs     *bytes.Buffer
}

func newCloudFixture(t *testing.T, limiter app.LoginLimiter) *cloudFixture {
	t.Helper()
	auth := &app.Auth{
		Identities: newCloudIdentities(),
		Tokens:     newCloudTokens(),
		Sessions:   newCloudSessions(),
		Mailer:     &cloudMailer{},
		Secrets:    &fixedSecrets{},
		Hasher:     fixedHasher{},
		IDs:        &cloudIDs{},
		Clock:      cloudClock{t: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)},
		LinkBase:   "https://cloud.example.com",
	}
	mail := auth.Mailer.(*cloudMailer)
	creds := memory.NewCredentials()
	projects := newFakeProjects()
	logs := &bytes.Buffer{}
	api := &httpapi.Server{
		Cloud: &httpapi.Cloud{
			Auth:         *auth,
			Limiter:      limiter,
			CookieSecure: true,
			SessionTTL:   time.Hour,
			Logger:       slog.New(slog.NewTextHandler(logs, nil)),
			Projects: app.Projects{
				Store:       projects,
				Credentials: creds,
				Issuer: app.Issuer{
					Store:   creds,
					Hasher:  shaHasher{},
					Secrets: &fixedSecrets{},
					Clock:   cloudClock{t: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)},
				},
				Clock: cloudClock{t: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)},
			},
		},
	}
	ts := httptest.NewTLSServer(api.Handler())
	t.Cleanup(ts.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := ts.Client()
	client.Jar = jar
	return &cloudFixture{server: ts, client: client, mail: mail, auth: auth, creds: creds, projects: projects, logs: logs}
}

// fixedSecrets yields predictable, distinct, long-enough secrets.
type fixedSecrets struct{ n int }

func (s *fixedSecrets) NewSecret() string {
	s.n++
	return strings.Repeat(string(rune('a'+s.n%26)), 40)
}

type fixedHasher struct{}

func (fixedHasher) Hash(raw string) string { return "h:" + raw }

func (f *cloudFixture) post(t *testing.T, path, contentType, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (f *cloudFixture) do(t *testing.T, method, path, csrf string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, f.server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
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

// signIn runs the magic-link flow and returns the CSRF token.
func (f *cloudFixture) signIn(t *testing.T, email string) string {
	t.Helper()
	resp := f.post(t, "/v1/auth/magic-link", "application/json", `{"email":"`+email+`"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("magic-link status = %d", resp.StatusCode)
	}
	raw := f.mail.lastToken(t)
	cb, err := f.client.Get(f.server.URL + "/v1/auth/callback?token=" + url.QueryEscape(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Body.Close()
	if cb.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d", cb.StatusCode)
	}
	var out struct {
		CSRF string `json:"csrfToken"`
	}
	if err := json.NewDecoder(cb.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.CSRF == "" {
		t.Fatal("no csrf token returned")
	}
	return out.CSRF
}

func TestCloudRoutesAbsentWhenDisabled(t *testing.T) {
	api := &httpapi.Server{}
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/v1/auth/magic-link"},
		{http.MethodGet, "/v1/auth/callback"},
		{http.MethodPost, "/v1/auth/signout"},
		{http.MethodGet, "/v1/me"},
		{http.MethodDelete, "/v1/account"},
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

	if resp, err := http.Get(ts.URL + "/healthz"); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz broken when cloud disabled: %v %v", resp, err)
	} else {
		resp.Body.Close()
	}
}

func TestCloudSignInSessionAndSignOut(t *testing.T) {
	f := newCloudFixture(t, nil)

	// The callback must set a hardened cookie.
	resp := f.post(t, "/v1/auth/magic-link", "application/json", `{"email":"alice@example.com"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("magic-link status = %d", resp.StatusCode)
	}
	raw := f.mail.lastToken(t)
	cb, err := f.client.Get(f.server.URL + "/v1/auth/callback?token=" + url.QueryEscape(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Body.Close()
	if cb.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d", cb.StatusCode)
	}
	var sessionCookie *http.Cookie
	for _, c := range cb.Cookies() {
		if c.Name == httpapi.SessionCookie {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("no session cookie set")
	}
	if !sessionCookie.HttpOnly || !sessionCookie.Secure || sessionCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie = %+v, want HttpOnly+Secure+SameSite=Strict", sessionCookie)
	}
	if sessionCookie.Path != "/" {
		t.Fatalf("cookie path = %q", sessionCookie.Path)
	}
	var body struct {
		CSRF string `json:"csrfToken"`
		User struct {
			Email string `json:"email"`
		} `json:"user"`
	}
	if err := json.NewDecoder(cb.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.CSRF == "" || body.User.Email != "alice@example.com" {
		t.Fatalf("callback body = %+v", body)
	}

	// A replay of the same link is refused.
	replay, err := f.client.Get(f.server.URL + "/v1/auth/callback?token=" + url.QueryEscape(raw))
	if err != nil {
		t.Fatal(err)
	}
	replay.Body.Close()
	if replay.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replay status = %d, want 401", replay.StatusCode)
	}

	// The session reads /me.
	me := f.do(t, http.MethodGet, "/v1/me", "")
	if me.StatusCode != http.StatusOK {
		t.Fatalf("/me status = %d", me.StatusCode)
	}
	me.Body.Close()

	// Sign out requires the CSRF token.
	if resp := f.do(t, http.MethodPost, "/v1/auth/signout", ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("signout without CSRF = %d, want 403", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := f.do(t, http.MethodPost, "/v1/auth/signout", "wrong"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("signout with wrong CSRF = %d, want 403", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := f.do(t, http.MethodPost, "/v1/auth/signout", body.CSRF); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("signout = %d, want 204", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := f.do(t, http.MethodGet, "/v1/me", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/me after signout = %d, want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

func TestCloudDeleteAccount(t *testing.T) {
	f := newCloudFixture(t, nil)
	csrf := f.signIn(t, "alice@example.com")

	if resp := f.do(t, http.MethodDelete, "/v1/account", ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("delete without CSRF = %d, want 403", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := f.do(t, http.MethodDelete, "/v1/account", csrf); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := f.do(t, http.MethodGet, "/v1/me", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/me after delete = %d, want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

func TestCloudMeWithoutSession(t *testing.T) {
	f := newCloudFixture(t, nil)
	resp := f.do(t, http.MethodGet, "/v1/me", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/me = %d, want 401", resp.StatusCode)
	}
}

func TestCloudMagicLinkValidation(t *testing.T) {
	f := newCloudFixture(t, nil)
	if resp := f.post(t, "/v1/auth/magic-link", "text/plain", `{"email":"a@example.com"}`); resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("non-json = %d, want 415", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := f.post(t, "/v1/auth/magic-link", "application/json", `{"email":"nope"}`); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid email = %d, want 422", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := f.post(t, "/v1/auth/magic-link", "application/json", `{`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed json = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

func TestCloudMagicLinkRateLimited(t *testing.T) {
	limiter := memory.NewLoginLimiter(2, time.Minute, cloudClock{t: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)})
	f := newCloudFixture(t, limiter)
	for i := 0; i < 2; i++ {
		resp := f.post(t, "/v1/auth/magic-link", "application/json", `{"email":"a@example.com"}`)
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("request %d = %d", i, resp.StatusCode)
		}
	}
	third := f.post(t, "/v1/auth/magic-link", "application/json", `{"email":"a@example.com"}`)
	defer third.Body.Close()
	if third.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third = %d, want 429", third.StatusCode)
	}
	if third.Header.Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(third.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "rate_limited" {
		t.Fatalf("error code = %q", body.Error.Code)
	}
}
