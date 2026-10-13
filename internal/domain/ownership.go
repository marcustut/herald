package domain

import (
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// maxNameLen bounds the display name of an account, tenant or project.
const maxNameLen = 128

// MaxDestinationTargetLen bounds a destination's address or URL.
const MaxDestinationTargetLen = 2048

// Account is the top-level ownership boundary: the billing and administrative
// unit a user signs up to. Every tenant and user belongs to one account.
type Account struct {
	ID        AccountID
	Name      string
	CreatedAt time.Time
}

func NewAccount(id AccountID, name string, createdAt time.Time) (Account, error) {
	if !validID(string(id)) {
		return Account{}, fmt.Errorf("%w: account id", ErrInvalidAccount)
	}
	name, err := validName(name, ErrInvalidAccount)
	if err != nil {
		return Account{}, err
	}
	if createdAt.IsZero() {
		return Account{}, fmt.Errorf("%w: created at", ErrInvalidAccount)
	}
	return Account{ID: id, Name: name, CreatedAt: createdAt}, nil
}

// User is a human identity inside an account. Users authenticate to the hosted
// control plane; agents authenticate with credentials instead.
type User struct {
	ID        UserID
	AccountID AccountID
	Email     string
	CreatedAt time.Time
}

func NewUser(id UserID, account AccountID, email string, createdAt time.Time) (User, error) {
	if !validID(string(id)) {
		return User{}, fmt.Errorf("%w: user id", ErrInvalidUser)
	}
	if !validID(string(account)) {
		return User{}, fmt.Errorf("%w: account id", ErrInvalidUser)
	}
	email, err := validEmail(email)
	if err != nil {
		return User{}, err
	}
	if createdAt.IsZero() {
		return User{}, fmt.Errorf("%w: created at", ErrInvalidUser)
	}
	return User{ID: id, AccountID: account, Email: email, CreatedAt: createdAt}, nil
}

// OwnedByAccount reports whether the user belongs to account.
func (u User) OwnedByAccount(account AccountID) bool { return u.AccountID == account }

// Tenant is an isolation boundary inside an account. Credentials, projects,
// events, deliveries and destinations all carry a tenant id, and one tenant can
// never observe another's rows.
type Tenant struct {
	ID        TenantID
	AccountID AccountID
	Name      string
	CreatedAt time.Time
}

func NewTenant(id TenantID, account AccountID, name string, createdAt time.Time) (Tenant, error) {
	if !validID(string(id)) {
		return Tenant{}, fmt.Errorf("%w: tenant id", ErrInvalidID)
	}
	if !validID(string(account)) {
		return Tenant{}, fmt.Errorf("%w: account id", ErrInvalidID)
	}
	name, err := validName(name, ErrInvalidID)
	if err != nil {
		return Tenant{}, err
	}
	if createdAt.IsZero() {
		return Tenant{}, fmt.Errorf("%w: created at", ErrInvalidID)
	}
	return Tenant{ID: id, AccountID: account, Name: name, CreatedAt: createdAt}, nil
}

// OwnedByAccount reports whether the tenant belongs to account.
func (t Tenant) OwnedByAccount(account AccountID) bool { return t.AccountID == account }

// Project is a named stream of events owned by exactly one tenant. Its slug is
// unique within the tenant, so two tenants may each own a project called
// "axis".
type Project struct {
	ID        ProjectID
	TenantID  TenantID
	Name      string
	CreatedAt time.Time
}

func NewProject(id ProjectID, tenant TenantID, name string, createdAt time.Time) (Project, error) {
	if !validSlug(string(id)) {
		return Project{}, fmt.Errorf("%w: project id", ErrInvalidID)
	}
	if !validID(string(tenant)) {
		return Project{}, fmt.Errorf("%w: tenant id", ErrInvalidID)
	}
	name, err := validName(name, ErrInvalidID)
	if err != nil {
		return Project{}, err
	}
	if createdAt.IsZero() {
		return Project{}, fmt.Errorf("%w: created at", ErrInvalidID)
	}
	return Project{ID: id, TenantID: tenant, Name: name, CreatedAt: createdAt}, nil
}

// OwnedBy reports whether the project belongs to tenant.
func (p Project) OwnedBy(tenant TenantID) bool { return p.TenantID == tenant }

// DestinationKind distinguishes an email target from an outbound webhook.
type DestinationKind string

const (
	DestinationEmail   DestinationKind = "email"
	DestinationWebhook DestinationKind = "webhook"
)

func (k DestinationKind) Valid() bool {
	return k == DestinationEmail || k == DestinationWebhook
}

// Destination is a delivery target owned by exactly one project. It is the
// durable form of a configured sink: the server resolves it from the credential's
// (tenant, project), never from a request body.
type Destination struct {
	ID        DestinationID
	TenantID  TenantID
	ProjectID ProjectID
	Kind      DestinationKind
	Target    string
	Enabled   bool
	// VerifiedAt records when ownership of the target was proven. Its zero
	// value means unverified, and an unverified destination is never delivered
	// to. A webhook proves ownership by being configured with the owner's
	// credentials, so it is verified at creation; an email target is verified
	// only when the mailbox owner redeems the verification link.
	VerifiedAt time.Time
	CreatedAt  time.Time
}

func NewDestination(in Destination) (Destination, error) {
	if !validID(string(in.ID)) {
		return Destination{}, fmt.Errorf("%w: destination id", ErrInvalidDestination)
	}
	if !validID(string(in.TenantID)) {
		return Destination{}, fmt.Errorf("%w: tenant id", ErrInvalidDestination)
	}
	if !validSlug(string(in.ProjectID)) {
		return Destination{}, fmt.Errorf("%w: project id", ErrInvalidDestination)
	}
	if !in.Kind.Valid() {
		return Destination{}, fmt.Errorf("%w: %q", ErrInvalidDestinationKind, in.Kind)
	}
	target := strings.TrimSpace(in.Target)
	if target == "" || len(target) > MaxDestinationTargetLen {
		return Destination{}, fmt.Errorf("%w: target", ErrInvalidDestination)
	}
	// Validate the target shape by kind so a hosted destination cannot be
	// stored as a file:/data: URL or a malformed mailbox. The delivery-time
	// SSRF guard remains the load-bearing control for webhooks.
	switch in.Kind {
	case DestinationEmail:
		email, err := validEmail(target)
		if err != nil {
			return Destination{}, fmt.Errorf("%w: %w", ErrInvalidDestination, err)
		}
		target = email
	case DestinationWebhook:
		u, err := url.Parse(target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Destination{}, fmt.Errorf("%w: target must be an absolute http(s) URL", ErrInvalidDestination)
		}
	}
	if in.CreatedAt.IsZero() {
		return Destination{}, fmt.Errorf("%w: created at", ErrInvalidDestination)
	}
	in.Target = target
	return in, nil
}

// OwnedBy reports whether the destination belongs to (tenant, project).
func (d Destination) OwnedBy(tenant TenantID, project ProjectID) bool {
	return d.TenantID == tenant && d.ProjectID == project
}

// Verified reports whether the destination's target ownership has been proven.
// An unverified destination must never be delivered to.
func (d Destination) Verified() bool { return !d.VerifiedAt.IsZero() }

func validName(s string, sentinel error) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || utf8.RuneCountInString(s) > maxNameLen {
		return "", fmt.Errorf("%w: name", sentinel)
	}
	return s, nil
}

// validEmail accepts a bare, lower-cased address within RFC 5321 length bounds.
// It is deliberately strict: one '@', a non-empty local part and a dotted
// domain, no whitespace. Domains are case-insensitive, so the domain is lowered.
func validEmail(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 254 || strings.ContainsAny(s, " \t\r\n") {
		return "", fmt.Errorf("%w: %q", ErrInvalidEmail, s)
	}
	at := strings.IndexByte(s, '@')
	if at <= 0 || at != strings.LastIndexByte(s, '@') || at >= len(s)-1 {
		return "", fmt.Errorf("%w: %q", ErrInvalidEmail, s)
	}
	local, domain := s[:at], s[at+1:]
	if len(local) > 64 || !strings.Contains(domain, ".") ||
		strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") ||
		strings.HasPrefix(domain, "-") || strings.HasSuffix(domain, "-") {
		return "", fmt.Errorf("%w: %q", ErrInvalidEmail, s)
	}
	return local + "@" + strings.ToLower(domain), nil
}
