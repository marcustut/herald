package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestParseToken(t *testing.T) {
	secret := strings.Repeat("s", MinTokenSecretLen)
	tests := []struct {
		name        string
		in          string
		wantKind    TokenKind
		wantProject ProjectID
		wantErr     error
	}{
		{"project", "herald_axis_" + secret, TokenProject, "axis", nil},
		{"dashed slug", "herald_my-app_" + secret, TokenProject, "my-app", nil},
		{"trimmed", "  herald_axis_" + secret + "  ", TokenProject, "axis", nil},
		{"secret with underscores", "herald_axis_aaa_bbb_ccc_ddd_eee", TokenProject, "axis", nil},
		{"missing prefix", "axis_" + secret, "", "", ErrInvalidToken},
		{"no separator", "herald_axis", "", "", ErrInvalidToken},
		{"empty slug", "herald__" + secret, "", "", ErrInvalidToken},
		{"empty secret", "herald_axis_", "", "", ErrInvalidToken},
		{"short secret", "herald_axis_short", "", "", ErrInvalidToken},
		{"uppercase slug", "herald_Axis_" + secret, "", "", ErrInvalidToken},
		{"leading dash slug", "herald_-axis-" + secret, "", "", ErrInvalidToken},
		{"empty", "", "", "", ErrInvalidToken},
		{"scoped", "heraldacct_" + secret, TokenScoped, "", nil},
		{"scoped trimmed", "  heraldacct_" + secret + " ", TokenScoped, "", nil},
		{"scoped too short", "heraldacct_short", "", "", ErrInvalidToken},
		{"scoped empty secret", "heraldacct_", "", "", ErrInvalidToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseToken(tt.in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			if got.Kind() != tt.wantKind {
				t.Fatalf("kind = %q, want %q", got.Kind(), tt.wantKind)
			}
			if got.Project() != tt.wantProject {
				t.Fatalf("project = %q, want %q", got.Project(), tt.wantProject)
			}
			if got.IsZero() || got.Raw() != strings.TrimSpace(tt.in) {
				t.Fatalf("raw = %q", got.Raw())
			}
		})
	}
}

func TestNewProjectToken(t *testing.T) {
	secret := strings.Repeat("s", MinTokenSecretLen)
	got, err := NewProjectToken("axis", secret)
	if err != nil {
		t.Fatal(err)
	}
	if want := "herald_axis_" + secret; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	if _, err := NewProjectToken("Bad", secret); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("invalid slug: err = %v", err)
	}
	if _, err := NewProjectToken("axis", "short"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("short secret: err = %v", err)
	}
}

func TestNewScopedToken(t *testing.T) {
	secret := strings.Repeat("s", MinTokenSecretLen)
	got, err := NewScopedToken(secret)
	if err != nil {
		t.Fatal(err)
	}
	if want := "heraldacct_" + secret; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if _, err := NewScopedToken("short"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("short secret: err = %v", err)
	}
}

func TestTokenRoundTrip(t *testing.T) {
	secret := strings.Repeat("s", MinTokenSecretLen)
	raw, _ := NewProjectToken("axis", secret)
	tok, err := ParseToken(raw)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Kind() != TokenProject || tok.Project() != "axis" {
		t.Fatalf("got %+v", tok)
	}
}

func TestTokenZeroValue(t *testing.T) {
	var tok Token
	if !tok.IsZero() {
		t.Fatal("zero token should event IsZero")
	}
}

func TestValidSlug(t *testing.T) {
	valid := []string{"a", "axis", "my-app", "app2", strings.Repeat("a", maxSlugLen)}
	for _, s := range valid {
		if !validSlug(s) {
			t.Errorf("%q should be valid", s)
		}
	}
	invalid := []string{"", "-a", "a-", "a_b", "A", "a b", strings.Repeat("a", maxSlugLen+1)}
	for _, s := range invalid {
		if validSlug(s) {
			t.Errorf("%q should be invalid", s)
		}
	}
}
