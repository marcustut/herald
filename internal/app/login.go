package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"html"
	"net/url"
	"strings"
	"time"

	"github.com/marcustut/herald/internal/domain"
)

const (
	// DefaultLinkTTL bounds how long a magic link may be redeemed. It is short
	// so a leaked mailbox or link is not a durable credential.
	DefaultLinkTTL = 15 * time.Minute
	// DefaultSessionTTL bounds a browser session.
	DefaultSessionTTL = 30 * 24 * time.Hour
)

// Session is an authenticated browser session. Raw and CSRF are the raw
// secrets returned to the client exactly once; only their hashes are stored.
type Session struct {
	Raw       string
	CSRF      string
	CSRFHash  string
	Identity  Identity
	ExpiresAt time.Time
}

// Auth is the hosted identity use case: email magic-link sign-in, session
// lifecycle, CSRF verification and account deletion. It is wired only when the
// cloud surface is enabled; a self-hosted deployment never constructs it.
//
// The magic link is a 256-bit random secret. Only its hash is stored, at rest
// the token is a single-use row with a short expiry, and consumption is an
// atomic mark-used so a replayed link cannot mint a second session. Sessions
// and their CSRF token follow the same hash-at-rest rule.
type Auth struct {
	Identities IdentityStore
	Tokens     LoginTokenStore
	Sessions   SessionStore
	Mailer     Mailer
	Secrets    SecretGenerator
	Hasher     Hasher
	IDs        IDGenerator
	Clock      Clock
	// LinkBase is the public base under which the API is served, e.g.
	// https://cloud.example.com or https://example.com/api. The callback
	// path is appended to it.
	LinkBase   string
	LinkTTL    time.Duration
	SessionTTL time.Duration
}

// RequestMagicLink mails a single-use sign-in link to email. It creates no
// account: the account/user/tenant rows are provisioned only when the link is
// redeemed, so a typo or an attacker-chosen address leaves nothing behind.
//
// The response is identical whether or not the address is already registered,
// so the endpoint cannot be used to enumerate accounts.
func (a Auth) RequestMagicLink(ctx context.Context, email string) error {
	email, err := domain.NormalizeEmail(email)
	if err != nil {
		return err
	}
	now := a.Clock.Now()
	raw := a.Secrets.NewSecret()
	if err := a.Tokens.PutLoginToken(ctx, LoginTokenRecord{
		Hash:      a.Hasher.Hash(raw),
		Email:     email,
		CreatedAt: now,
		ExpiresAt: now.Add(a.linkTTL()),
	}); err != nil {
		return err
	}
	link := a.magicLink(raw)
	return a.Mailer.SendMail(ctx, MailMessage{
		To:      email,
		Subject: "Your Herald sign-in link",
		Text:    magicLinkText(link),
		HTML:    magicLinkHTML(link),
	})
}

// ConsumeMagicLink redeems a link exactly once and starts a session. An
// unknown, expired or already-used token returns ErrUnauthenticated.
func (a Auth) ConsumeMagicLink(ctx context.Context, raw string) (Session, error) {
	if !tokenShapeOK(raw) {
		return Session{}, ErrUnauthenticated
	}
	now := a.Clock.Now()
	hash := a.Hasher.Hash(raw)
	rec, err := a.Tokens.ConsumeLoginToken(ctx, hash, now)
	if errors.Is(err, ErrLoginTokenNotFound) {
		return Session{}, ErrUnauthenticated
	}
	if err != nil {
		return Session{}, err
	}
	// Defence in depth: the store already matched by hash, but re-check in
	// constant time so a secret is never compared with a variable-time ==.
	if subtle.ConstantTimeCompare([]byte(rec.Hash), []byte(hash)) != 1 {
		return Session{}, ErrUnauthenticated
	}
	id, err := a.Identities.EnsureUser(ctx, rec.Email, a.newIDs(), now)
	if err != nil {
		return Session{}, err
	}
	sessionRaw := a.Secrets.NewSecret()
	csrfRaw := a.Secrets.NewSecret()
	expires := now.Add(a.sessionTTL())
	if err := a.Sessions.PutSession(ctx, SessionRecord{
		Hash:      a.Hasher.Hash(sessionRaw),
		UserID:    id.UserID,
		AccountID: id.AccountID,
		TenantID:  id.TenantID,
		Email:     id.Email,
		CSRFHash:  a.Hasher.Hash(csrfRaw),
		CreatedAt: now,
		ExpiresAt: expires,
	}); err != nil {
		return Session{}, err
	}
	return Session{Raw: sessionRaw, CSRF: csrfRaw, CSRFHash: a.Hasher.Hash(csrfRaw), Identity: id, ExpiresAt: expires}, nil
}

// AuthenticateSession resolves a session cookie to its identity. An unknown,
// expired or revoked session returns ErrUnauthenticated.
func (a Auth) AuthenticateSession(ctx context.Context, raw string) (Session, error) {
	if !tokenShapeOK(raw) {
		return Session{}, ErrUnauthenticated
	}
	now := a.Clock.Now()
	hash := a.Hasher.Hash(raw)
	rec, err := a.Sessions.SessionByHash(ctx, hash, now)
	if errors.Is(err, ErrSessionNotFound) {
		return Session{}, ErrUnauthenticated
	}
	if err != nil {
		return Session{}, err
	}
	if subtle.ConstantTimeCompare([]byte(rec.Hash), []byte(hash)) != 1 {
		return Session{}, ErrUnauthenticated
	}
	return Session{
		CSRFHash:  rec.CSRFHash,
		ExpiresAt: rec.ExpiresAt,
		Identity:  Identity{UserID: rec.UserID, AccountID: rec.AccountID, Email: rec.Email, TenantID: rec.TenantID},
	}, nil
}

// VerifyCSRF reports whether raw matches the session's synchronizer token, in
// constant time. A state-changing request must carry it so a cross-site form
// cannot act with the session cookie.
func (a Auth) VerifyCSRF(s Session, raw string) bool {
	if raw == "" || s.CSRFHash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a.Hasher.Hash(raw)), []byte(s.CSRFHash)) == 1
}

// SignOut revokes the session behind raw. It is idempotent: an unknown or
// already-revoked cookie revokes nothing and reports no error.
func (a Auth) SignOut(ctx context.Context, raw string) error {
	if !tokenShapeOK(raw) {
		return nil
	}
	return a.Sessions.RevokeSession(ctx, a.Hasher.Hash(raw), a.Clock.Now())
}

// DeleteAccount revokes every session in the session's account and then deletes
// the account, cascading users, tenants, projects and destinations.
func (a Auth) DeleteAccount(ctx context.Context, raw string) error {
	sess, err := a.AuthenticateSession(ctx, raw)
	if err != nil {
		return err
	}
	now := a.Clock.Now()
	if err := a.Sessions.RevokeAccountSessions(ctx, sess.Identity.AccountID, now); err != nil {
		return err
	}
	return a.Identities.DeleteAccount(ctx, sess.Identity.AccountID)
}

func (a Auth) linkTTL() time.Duration {
	if a.LinkTTL <= 0 {
		return DefaultLinkTTL
	}
	return a.LinkTTL
}

func (a Auth) sessionTTL() time.Duration {
	if a.SessionTTL <= 0 {
		return DefaultSessionTTL
	}
	return a.SessionTTL
}

func (a Auth) newIDs() NewIdentityIDs {
	return NewIdentityIDs{
		Account: a.IDs.NewAccountID(),
		User:    a.IDs.NewUserID(),
		Tenant:  a.IDs.NewTenantID(),
	}
}

func (a Auth) magicLink(raw string) string {
	return strings.TrimRight(a.LinkBase, "/") + "/v1/auth/callback?token=" + url.QueryEscape(raw)
}

// tokenShapeOK rejects a value that cannot be one of our secrets before it
// reaches a store, so a malformed cookie or link is cheap to refuse.
func tokenShapeOK(raw string) bool { return len(raw) >= domain.MinTokenSecretLen }

func magicLinkText(link string) string {
	return "Sign in to Herald by opening this single-use link:\n\n" + link +
		"\n\nIt expires shortly and can be used once. If you did not request it, ignore this email.\n"
}

func magicLinkHTML(link string) string {
	escaped := html.EscapeString(link)
	return fmt.Sprintf(
		`<p>Sign in to Herald by opening this single-use link:</p><p><a href="%s">Sign in</a></p>`+
			`<p>It expires shortly and can be used once. If you did not request it, ignore this email.</p>`,
		escaped)
}
