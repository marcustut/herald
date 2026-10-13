package domain

import (
	"strings"
	"testing"
)

func TestRedactSecrets(t *testing.T) {
	tests := []struct {
		name string
		in   string
		// leak is a substring that must be gone after redaction.
		leak string
	}{
		{"aws access key", "cred AKIAIOSFODNN7EXAMPLE here", "AKIAIOSFODNN7EXAMPLE"},
		{"aws temp key", "ASIAIOSFODNN7EXAMPLE", "ASIAIOSFODNN7EXAMPLE"},
		{"github token", "ghp_0123456789abcdefghijklmnopqrstuvwxyz", "ghp_0123456789abcdefghijklmnopqrstuvwxyz"},
		{"slack token", "xoxb-1234567890-abcdefghij", "xoxb-1234567890-abcdefghij"},
		{"openai key", "sk-abcdefghijklmnopqrstuvwx", "sk-abcdefghijklmnopqrstuvwx"},
		{"jwt", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcdefghij", "eyJhbGciOiJIUzI1NiJ9"},
		{"url credentials", "postgres://user:s3cr3t@db.internal:5432/app", "s3cr3t"},
		{"password assignment", "password=hunter2", "hunter2"},
		{"password colon", "passwd: hunter2", "hunter2"},
		{"api key", `api_key: "abcdef123456"`, "abcdef123456"},
		{"authorization header", "Authorization: Bearer abcdef123456", "abcdef123456"},
		{"private key block", "-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----", "MIIEow"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Redact(tt.in)
			if strings.Contains(got, tt.leak) {
				t.Fatalf("secret leaked through: %q", got)
			}
			if !strings.Contains(got, RedactionMarker) {
				t.Fatalf("expected marker in %q", got)
			}
		})
	}
}

func TestRedactLeavesOrdinaryProse(t *testing.T) {
	inputs := []string{
		"deploy finished in 42s, all tests passed",
		"the token of appreciation goes to the team",
		"the secret to success is consistency",
		"see https://example.com/builds/123 for details",
		"status: success",
	}
	for _, in := range inputs {
		if got := Redact(in); got != in {
			t.Errorf("false positive: %q -> %q", in, got)
		}
	}
}

func TestRedactIsIdempotent(t *testing.T) {
	inputs := []string{
		"password=hunter2 and token: eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcdefghij",
		"postgres://user:s3cr3t@db.internal/app",
		"AKIAIOSFODNN7EXAMPLE",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----",
	}
	for _, in := range inputs {
		once := Redact(in)
		twice := Redact(once)
		if once != twice {
			t.Errorf("not idempotent for %q:\n once: %q\ntwice: %q", in, once, twice)
		}
	}
}

func TestRedactPreservesKeys(t *testing.T) {
	if got := Redact("password=hunter2"); got != "password="+RedactionMarker {
		t.Fatalf("got %q", got)
	}
	if got := Redact("postgres://user:pw@host/db"); got != "postgres://"+RedactionMarker+"@host/db" {
		t.Fatalf("got %q", got)
	}
}
