package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestParseAgentID(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    AgentID
		wantErr error
	}{
		{"valid", "herdr-build", "herdr-build", nil},
		{"single word", "ci", "ci", nil},
		{"trimmed", "  ci-runner  ", "ci-runner", nil},
		{"empty", "", "", ErrInvalidAgent},
		{"uppercase", "Herdr", "", ErrInvalidAgent},
		{"space", "a b", "", ErrInvalidAgent},
		{"underscore", "a_b", "", ErrInvalidAgent},
		{"trailing dash", "a-", "", ErrInvalidAgent},
		{"too long", strings.Repeat("a", maxSlugLen+1), "", ErrInvalidAgent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseAgentID(tt.in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAgentIDValid(t *testing.T) {
	if !AgentID("herdr-build").Valid() {
		t.Error("valid agent rejected")
	}
	if AgentID("Herdr").Valid() {
		t.Error("invalid agent accepted")
	}
}

func TestAgentIDString(t *testing.T) {
	if got := AgentID("herdr").String(); got != "herdr" {
		t.Fatalf("got %q", got)
	}
}
