package app

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/domain"
)

type stepClock struct{ t time.Time }

func (c *stepClock) Now() time.Time          { return c.t }
func (c *stepClock) advance(d time.Duration) { c.t = c.t.Add(d) }

type fakeIdentities struct {
	byEmail map[string]Identity
	deleted []domain.AccountID
}

func newFakeIdentities() *fakeIdentities {
	return &fakeIdentities{byEmail: map[string]Identity{}}
}

func (s *fakeIdentities) EnsureUser(_ context.Context, email string, ids NewIdentityIDs, _ time.Time) (Identity, error) {
	if id, ok := s.byEmail[email]; ok {
		return id, nil
	}
	id := Identity{UserID: ids.User, AccountID: ids.Account, Email: email, TenantID: ids.Tenant}
	s.byEmail[email] = id
	return id, nil
}

func (s *fakeIdentities) UserByEmail(_ context.Context, email string) (Identity, bool, error) {
	id, ok := s.byEmail[email]
	return id, ok, nil
}

func (s *fakeIdentities) DeleteAccount(_ context.Context, account domain.AccountID) error {
	s.deleted = append(s.deleted, account)
	for email, id := range s.byEmail {
		if id.AccountID == account {
			delete(s.byEmail, email)
		}
	}
	return nil
}

type fakeSessions struct{ byHash map[string]SessionRecord }

func newFakeSessions() *fakeSessions { return &fakeSessions{byHash: map[string]SessionRecord{}} }

func (s *fakeSessions) PutSession(_ context.Context, r SessionRecord) error {
	s.byHash[r.Hash] = r
	return nil
}

func (s *fakeSessions) SessionByHash(_ context.Context, hash string, now time.Time) (SessionRecord, error) {
	r, ok := s.byHash[hash]
	if !ok || !r.RevokedAt.IsZero() || !r.ExpiresAt.After(now) {
		return SessionRecord{}, ErrSessionNotFound
	}
	return r, nil
}

func (s *fakeSessions) RevokeSession(_ context.Context, hash string, now time.Time) error {
	if r, ok := s.byHash[hash]; ok {
		r.RevokedAt = now
		s.byHash[hash] = r
	}
	return nil
}

func (s *fakeSessions) RevokeAccountSessions(_ context.Context, account domain.AccountID, now time.Time) error {
	for h, r := range s.byHash {
		if r.AccountID == account {
			r.RevokedAt = now
			s.byHash[h] = r
		}
	}
	return nil
}

type fakeLoginTokens struct{ byHash map[string]LoginTokenRecord }

func newFakeLoginTokens() *fakeLoginTokens {
	return &fakeLoginTokens{byHash: map[string]LoginTokenRecord{}}
}

func (s *fakeLoginTokens) PutLoginToken(_ context.Context, t LoginTokenRecord) error {
	s.byHash[t.Hash] = t
	return nil
}

func (s *fakeLoginTokens) ConsumeLoginToken(_ context.Context, hash string, now time.Time) (LoginTokenRecord, error) {
	r, ok := s.byHash[hash]
	if !ok || !r.UsedAt.IsZero() || !r.ExpiresAt.After(now) {
		return LoginTokenRecord{}, ErrLoginTokenNotFound
	}
	r.UsedAt = now
	s.byHash[hash] = r
	return r, nil
}

type fakeMailer struct {
	msgs []MailMessage
	err  error
}

func (m *fakeMailer) SendMail(_ context.Context, msg MailMessage) error {
	if m.err != nil {
		return m.err
	}
	m.msgs = append(m.msgs, msg)
	return nil
}

// token extracts the raw magic-link token from the last message sent.
func (m *fakeMailer) token(t *testing.T) string {
	t.Helper()
	if len(m.msgs) == 0 {
		t.Fatal("no mail was sent")
	}
	body := m.msgs[len(m.msgs)-1].Text
	_, rest, ok := strings.Cut(body, "token=")
	if !ok {
		t.Fatalf("mail body has no token: %q", body)
	}
	rest = strings.TrimRight(rest, "\r\n")
	if i := strings.IndexAny(rest, " \t\r\n"); i >= 0 {
		rest = rest[:i]
	}
	raw, err := url.QueryUnescape(rest)
	if err != nil {
		t.Fatalf("unescape token: %v", err)
	}
	return raw
}

func newAuth(t *testing.T) (Auth, *stepClock, *fakeIdentities, *fakeSessions, *fakeLoginTokens, *fakeMailer) {
	t.Helper()
	ids := newFakeIdentities()
	sessions := newFakeSessions()
	tokens := newFakeLoginTokens()
	mail := &fakeMailer{}
	clock := &stepClock{t: testNow}
	return Auth{
		Identities: ids,
		Tokens:     tokens,
		Sessions:   sessions,
		Mailer:     mail,
		Secrets:    &seqSecrets{},
		Hasher:     fakeHasher{},
		IDs:        &fakeIDs{},
		Clock:      clock,
		LinkBase:   "https://cloud.example.com",
	}, clock, ids, sessions, tokens, mail
}

func TestRequestMagicLinkStoresOnlyHash(t *testing.T) {
	a, _, _, _, tokens, mail := newAuth(t)
	if err := a.RequestMagicLink(context.Background(), "User@Example.COM"); err != nil {
		t.Fatalf("request: %v", err)
	}
	if len(tokens.byHash) != 1 {
		t.Fatalf("stored tokens = %d, want 1", len(tokens.byHash))
	}
	var rec LoginTokenRecord
	for _, r := range tokens.byHash {
		rec = r
	}
	if rec.Email != "User@example.com" {
		t.Fatalf("stored email = %q, want local preserved and domain lowered", rec.Email)
	}
	if want := testNow.Add(DefaultLinkTTL); !rec.ExpiresAt.Equal(want) {
		t.Fatalf("expiry = %v, want %v", rec.ExpiresAt, want)
	}
	raw := mail.token(t)
	if rec.Hash != a.Hasher.Hash(raw) {
		t.Fatal("stored hash does not match the link token")
	}
	if rec.Hash == raw {
		t.Fatal("token stored in the clear")
	}
	if len(mail.msgs) != 1 || mail.msgs[0].To != "User@example.com" {
		t.Fatalf("mailed to %+v", mail.msgs)
	}
	if !strings.Contains(mail.msgs[0].Text, "https://cloud.example.com/v1/auth/callback?token=") {
		t.Fatalf("mail text = %q", mail.msgs[0].Text)
	}
}

func TestRequestMagicLinkSignupOnProvisionsOnRedeem(t *testing.T) {
	a, _, ids, _, _, mail := newAuth(t)
	// Signup is on by default (zero value).
	if a.DisableSignup {
		t.Fatal("signup should default on")
	}
	if err := a.RequestMagicLink(context.Background(), "fresh@example.com"); err != nil {
		t.Fatal(err)
	}
	if len(mail.msgs) != 1 {
		t.Fatalf("mailed = %d, want 1", len(mail.msgs))
	}
	if len(ids.byEmail) != 0 {
		t.Fatal("provisioned before redeeming the link")
	}
	if _, err := a.ConsumeMagicLink(context.Background(), mail.token(t)); err != nil {
		t.Fatal(err)
	}
	if len(ids.byEmail) != 1 {
		t.Fatalf("identities = %d, want 1 after redeem", len(ids.byEmail))
	}
}

func TestRequestMagicLinkSignupOff(t *testing.T) {
	a, _, ids, _, tokens, mail := newAuth(t)
	a.DisableSignup = true

	// A new address is silently accepted: no mail, no token row, no account.
	if err := a.RequestMagicLink(context.Background(), "fresh@example.com"); err != nil {
		t.Fatal(err)
	}
	if len(mail.msgs) != 0 {
		t.Fatalf("mailed a new address with signup off: %+v", mail.msgs)
	}
	if len(tokens.byHash) != 0 {
		t.Fatalf("stored a login token for a new address: %+v", tokens.byHash)
	}
	if len(ids.byEmail) != 0 {
		t.Fatalf("provisioned a new address with signup off: %+v", ids.byEmail)
	}

	// An existing address still receives a link.
	if _, err := ids.EnsureUser(context.Background(), "known@example.com", a.newIDs(), testNow); err != nil {
		t.Fatal(err)
	}
	if err := a.RequestMagicLink(context.Background(), "known@example.com"); err != nil {
		t.Fatal(err)
	}
	if len(mail.msgs) != 1 || mail.msgs[0].To != "known@example.com" {
		t.Fatalf("existing address not mailed: %+v", mail.msgs)
	}
	if len(tokens.byHash) != 1 {
		t.Fatalf("login tokens = %d, want 1", len(tokens.byHash))
	}
}

func TestRequestMagicLinkInvalidEmail(t *testing.T) {
	a, _, _, _, _, _ := newAuth(t)
	if err := a.RequestMagicLink(context.Background(), "not-an-email"); !errors.Is(err, domain.ErrInvalidEmail) {
		t.Fatalf("err = %v, want ErrInvalidEmail", err)
	}
}

func TestRequestMagicLinkMailerFailureSurfaces(t *testing.T) {
	a, _, _, _, _, mail := newAuth(t)
	mail.err = errors.New("smtp down")
	if err := a.RequestMagicLink(context.Background(), "u@example.com"); err == nil {
		t.Fatal("want mailer error")
	}
}

func TestConsumeMagicLinkIsSingleUse(t *testing.T) {
	a, _, _, _, _, mail := newAuth(t)
	if err := a.RequestMagicLink(context.Background(), "u@example.com"); err != nil {
		t.Fatal(err)
	}
	raw := mail.token(t)

	sess, err := a.ConsumeMagicLink(context.Background(), raw)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if sess.Raw == "" || sess.CSRF == "" || sess.Identity.Email != "u@example.com" {
		t.Fatalf("session = %+v", sess)
	}
	if _, err := a.ConsumeMagicLink(context.Background(), raw); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("replay err = %v, want ErrUnauthenticated", err)
	}
}

func TestConsumeMagicLinkExpires(t *testing.T) {
	a, clock, _, _, _, mail := newAuth(t)
	if err := a.RequestMagicLink(context.Background(), "u@example.com"); err != nil {
		t.Fatal(err)
	}
	raw := mail.token(t)
	clock.advance(DefaultLinkTTL + time.Second)
	if _, err := a.ConsumeMagicLink(context.Background(), raw); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expired err = %v, want ErrUnauthenticated", err)
	}
}

func TestConsumeUnknownMagicLink(t *testing.T) {
	a, _, _, _, _, _ := newAuth(t)
	if _, err := a.ConsumeMagicLink(context.Background(), strings.Repeat("a", 32)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
	if _, err := a.ConsumeMagicLink(context.Background(), "short"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("short token err = %v, want ErrUnauthenticated", err)
	}
}

func TestSessionLifecycleAndCSRF(t *testing.T) {
	a, clock, _, _, _, mail := newAuth(t)
	if err := a.RequestMagicLink(context.Background(), "u@example.com"); err != nil {
		t.Fatal(err)
	}
	sess, err := a.ConsumeMagicLink(context.Background(), mail.token(t))
	if err != nil {
		t.Fatal(err)
	}

	got, err := a.AuthenticateSession(context.Background(), sess.Raw)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if got.Identity.Email != "u@example.com" {
		t.Fatalf("identity = %+v", got.Identity)
	}
	if !a.VerifyCSRF(got, sess.CSRF) {
		t.Fatal("valid CSRF token rejected")
	}
	if a.VerifyCSRF(got, "wrong") || a.VerifyCSRF(got, "") {
		t.Fatal("invalid CSRF token accepted")
	}

	if err := a.SignOut(context.Background(), sess.Raw); err != nil {
		t.Fatalf("sign out: %v", err)
	}
	if _, err := a.AuthenticateSession(context.Background(), sess.Raw); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("after signout err = %v, want ErrUnauthenticated", err)
	}
	// Sign out is idempotent and a second call reports no error.
	if err := a.SignOut(context.Background(), sess.Raw); err != nil {
		t.Fatalf("second sign out: %v", err)
	}

	// A session expires on its own.
	if err := a.RequestMagicLink(context.Background(), "u@example.com"); err != nil {
		t.Fatal(err)
	}
	sess2, err := a.ConsumeMagicLink(context.Background(), mail.token(t))
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(DefaultSessionTTL + time.Second)
	if _, err := a.AuthenticateSession(context.Background(), sess2.Raw); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expired session err = %v, want ErrUnauthenticated", err)
	}
}

func TestDeleteAccountRevokesSessions(t *testing.T) {
	a, _, ids, _, _, mail := newAuth(t)
	if err := a.RequestMagicLink(context.Background(), "u@example.com"); err != nil {
		t.Fatal(err)
	}
	sess, err := a.ConsumeMagicLink(context.Background(), mail.token(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteAccount(context.Background(), sess.Raw); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := a.AuthenticateSession(context.Background(), sess.Raw); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("after delete err = %v, want ErrUnauthenticated", err)
	}
	if len(ids.deleted) != 1 || ids.deleted[0] != sess.Identity.AccountID {
		t.Fatalf("deleted accounts = %v, want %s", ids.deleted, sess.Identity.AccountID)
	}
}

func TestEnsureUserIsStableAcrossSignIns(t *testing.T) {
	a, _, ids, _, _, mail := newAuth(t)
	for i := 0; i < 2; i++ {
		if err := a.RequestMagicLink(context.Background(), "u@example.com"); err != nil {
			t.Fatal(err)
		}
		if _, err := a.ConsumeMagicLink(context.Background(), mail.token(t)); err != nil {
			t.Fatal(err)
		}
	}
	if len(ids.byEmail) != 1 {
		t.Fatalf("identities = %d, want 1", len(ids.byEmail))
	}
}
