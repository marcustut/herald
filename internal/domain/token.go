package domain

import (
	"fmt"
	"strings"
)

const (
	projectTokenPrefix = "herald_"
	scopedTokenPrefix  = "heraldacct_"
	MinTokenSecretLen  = 16
	maxSlugLen         = 64
)

// TokenKind distinguishes a single-project credential from a scoped one.
type TokenKind string

const (
	// TokenProject is herald_<project>_<secret>: the project is fixed by the token.
	TokenProject TokenKind = "project"
	// TokenScoped is heraldacct_<secret>: the project is chosen per request and
	// authorized against the credential's scope. The distinct prefix keeps it
	// from colliding with a project token for a project named "acct".
	TokenScoped TokenKind = "scoped"
)

// Token is a parsed bearer credential. The raw string is held only to be hashed
// by the authenticator; it must never be logged.
type Token struct {
	kind    TokenKind
	project ProjectID
	raw     string
}

func ParseToken(raw string) (Token, error) {
	raw = strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(raw, scopedTokenPrefix):
		if len(raw)-len(scopedTokenPrefix) < MinTokenSecretLen {
			return Token{}, fmt.Errorf("%w: scoped secret too short", ErrInvalidToken)
		}
		return Token{kind: TokenScoped, raw: raw}, nil
	case strings.HasPrefix(raw, projectTokenPrefix):
		rest := raw[len(projectTokenPrefix):]
		i := strings.IndexByte(rest, '_')
		if i <= 0 || i >= len(rest)-1 {
			return Token{}, fmt.Errorf("%w: malformed", ErrInvalidToken)
		}
		slug, secret := rest[:i], rest[i+1:]
		if !validSlug(slug) {
			return Token{}, fmt.Errorf("%w: project slug", ErrInvalidToken)
		}
		if len(secret) < MinTokenSecretLen {
			return Token{}, fmt.Errorf("%w: secret too short", ErrInvalidToken)
		}
		return Token{kind: TokenProject, project: ProjectID(slug), raw: raw}, nil
	default:
		return Token{}, fmt.Errorf("%w: missing prefix", ErrInvalidToken)
	}
}

// NewProjectToken builds herald_<project>_<secret>.
func NewProjectToken(project ProjectID, secret string) (string, error) {
	if !validSlug(string(project)) {
		return "", fmt.Errorf("%w: project slug", ErrInvalidToken)
	}
	if len(secret) < MinTokenSecretLen {
		return "", fmt.Errorf("%w: secret too short", ErrInvalidToken)
	}
	return projectTokenPrefix + string(project) + "_" + secret, nil
}

// NewScopedToken builds heraldacct_<secret>.
func NewScopedToken(secret string) (string, error) {
	if len(secret) < MinTokenSecretLen {
		return "", fmt.Errorf("%w: secret too short", ErrInvalidToken)
	}
	return scopedTokenPrefix + secret, nil
}

// Kind returns whether this token fixes a project or carries a scope.
func (t Token) Kind() TokenKind { return t.kind }

// Project returns the project hint carried by a project token; empty for scoped.
func (t Token) Project() ProjectID { return t.project }

// Raw returns the full token string for hashing. Treat as secret.
func (t Token) Raw() string { return t.raw }

func (t Token) IsZero() bool { return t.raw == "" }

// validSlug accepts lowercase alphanumerics with interior dashes.
func validSlug(s string) bool {
	if s == "" || len(s) > maxSlugLen {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' && i > 0 && i < len(s)-1:
		default:
			return false
		}
	}
	return true
}
