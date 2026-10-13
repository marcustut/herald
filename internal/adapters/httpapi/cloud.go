package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// SessionCookie is the browser session cookie. It is httpOnly so script cannot
// read it; the CSRF token travels separately.
const SessionCookie = "herald_session"

// CSRFHeader carries the synchronizer token on state-changing cloud requests.
const CSRFHeader = "X-CSRF-Token"

// Built-in sign-in throttling: every magic-link request consumes one unit for
// the client IP and one for the target email within the window.
const (
	DefaultAuthRequests = 10
	DefaultAuthWindow   = time.Minute
)

// Cloud is the hosted control-plane surface: email magic-link sign-in, session
// lifecycle, CSRF protection and account deletion. It is composed into the
// server only when [cloud] enabled is set, so a self-hosted deployment without
// it exposes no accounts, signup or sign-in routes.
type Cloud struct {
	Auth         app.Auth
	Projects     app.Projects
	Limiter      app.LoginLimiter
	CookieSecure bool
	SessionTTL   time.Duration
	Logger       *slog.Logger
}

func (c *Cloud) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /cloud/v1/auth/magic-link", c.handleMagicLink)
	mux.HandleFunc("GET /cloud/v1/auth/callback", c.handleCallback)
	mux.HandleFunc("POST /cloud/v1/auth/signout", c.handleSignOut)
	mux.HandleFunc("GET /cloud/v1/me", c.handleMe)
	mux.HandleFunc("DELETE /cloud/v1/account", c.handleDeleteAccount)
	c.registerProjects(mux)
}

type magicLinkPayload struct {
	Email string `json:"email"`
}

// handleMagicLink accepts a sign-in request. It answers 202 for any valid
// address whether or not it is registered, so the endpoint cannot enumerate
// accounts.
func (c *Cloud) handleMagicLink(w http.ResponseWriter, r *http.Request) {
	if !isJSON(r.Header.Get("Content-Type")) {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json", "")
		return
	}
	max := int64(defaultMaxBody)
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "could not read request body", "")
		return
	}
	var payload magicLinkPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "malformed JSON", "")
		return
	}
	email, err := domain.NormalizeEmail(payload.Email)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "a valid email is required", "email")
		return
	}
	// Limit per IP and per address: the first bounds a single client, the
	// second stops one mailbox being bombed from many clients.
	if !c.allow(w, r, "ip:"+clientIP(r)) {
		return
	}
	if !c.allow(w, r, "email:"+email) {
		return
	}
	if err := c.Auth.RequestMagicLink(r.Context(), email); err != nil {
		c.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "sent"})
}

// handleCallback redeems the single-use link, starts a session and returns the
// CSRF token. A replay or expiry is 401.
func (c *Cloud) handleCallback(w http.ResponseWriter, r *http.Request) {
	sess, err := c.Auth.ConsumeMagicLink(r.Context(), r.URL.Query().Get("token"))
	if err != nil {
		if errors.Is(err, app.ErrUnauthenticated) {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "invalid or expired link", "")
			return
		}
		c.serverError(w, err)
		return
	}
	c.setSessionCookie(w, sess.Raw)
	writeJSON(w, http.StatusOK, sessionDTO(sess))
}

// handleMe returns the identity behind the session cookie.
func (c *Cloud) handleMe(w http.ResponseWriter, r *http.Request) {
	sess, ok := c.session(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": userDTOFrom(sess.Identity)})
}

// handleSignOut revokes the session and clears the cookie. It requires the
// CSRF token so a cross-site form cannot sign the user out.
func (c *Cloud) handleSignOut(w http.ResponseWriter, r *http.Request) {
	sess, ok := c.session(w, r)
	if !ok {
		return
	}
	if !c.csrf(w, r, sess) {
		return
	}
	if err := c.Auth.SignOut(r.Context(), c.rawCookie(r)); err != nil {
		c.serverError(w, err)
		return
	}
	c.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteAccount deletes the account and everything it owns (users,
// tenants, projects, destinations and every session), then clears the cookie.
func (c *Cloud) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	sess, ok := c.session(w, r)
	if !ok {
		return
	}
	if !c.csrf(w, r, sess) {
		return
	}
	if err := c.Auth.DeleteAccount(r.Context(), c.rawCookie(r)); err != nil {
		c.serverError(w, err)
		return
	}
	c.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// session resolves the session cookie or writes 401.
func (c *Cloud) session(w http.ResponseWriter, r *http.Request) (app.Session, bool) {
	raw := c.rawCookie(r)
	if raw == "" {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "no session", "")
		return app.Session{}, false
	}
	sess, err := c.Auth.AuthenticateSession(r.Context(), raw)
	if err != nil {
		if errors.Is(err, app.ErrUnauthenticated) {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "invalid or expired session", "")
			return app.Session{}, false
		}
		c.serverError(w, err)
		return app.Session{}, false
	}
	return sess, true
}

// csrf enforces the synchronizer token on a state-changing request.
func (c *Cloud) csrf(w http.ResponseWriter, r *http.Request, sess app.Session) bool {
	if !c.Auth.VerifyCSRF(sess, r.Header.Get(CSRFHeader)) {
		writeError(w, http.StatusForbidden, "csrf_failed", "missing or invalid CSRF token", "")
		return false
	}
	return true
}

func (c *Cloud) allow(w http.ResponseWriter, r *http.Request, key string) bool {
	if c.Limiter == nil {
		return true
	}
	d, err := c.Limiter.Allow(r.Context(), key)
	if err != nil {
		c.serverError(w, err)
		return false
	}
	writeRateLimit(w, d)
	if !d.Allowed {
		w.Header().Set("Retry-After", seconds(d.RetryAfter))
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many sign-in attempts", "")
		return false
	}
	return true
}

func (c *Cloud) serverError(w http.ResponseWriter, err error) {
	if c.Logger != nil {
		// Log the error only; the magic-link token is never an error value and
		// is never a label.
		c.Logger.Error("cloud request failed", "error", err)
	}
	writeError(w, http.StatusServiceUnavailable, "unavailable", "service unavailable", "")
}

func (c *Cloud) rawCookie(r *http.Request) string {
	if cookie, err := r.Cookie(SessionCookie); err == nil {
		return cookie.Value
	}
	return ""
}

func (c *Cloud) sessionTTL() time.Duration {
	if c.SessionTTL <= 0 {
		return app.DefaultSessionTTL
	}
	return c.SessionTTL
}

func (c *Cloud) setSessionCookie(w http.ResponseWriter, raw string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    raw,
		Path:     "/",
		HttpOnly: true,
		Secure:   c.CookieSecure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(c.sessionTTL().Seconds()),
	})
}

func (c *Cloud) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   c.CookieSecure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// clientIP is the transport peer. A forwarded header is deliberately not
// trusted, so a caller cannot evade the per-IP limit by spoofing X-Forwarded-For.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

type userDTO struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	AccountID string `json:"accountId"`
	TenantID  string `json:"tenantId"`
}

type sessionDTOBody struct {
	CSRF string  `json:"csrfToken"`
	User userDTO `json:"user"`
}

func userDTOFrom(i app.Identity) userDTO {
	return userDTO{
		ID:        string(i.UserID),
		Email:     i.Email,
		AccountID: string(i.AccountID),
		TenantID:  string(i.TenantID),
	}
}

func sessionDTO(s app.Session) sessionDTOBody {
	return sessionDTOBody{CSRF: s.CSRF, User: userDTOFrom(s.Identity)}
}
