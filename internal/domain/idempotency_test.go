package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestParseIdempotencyKey(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr error
	}{
		{"plain", "abc123", nil},
		{"with separators", "run:2026-10-12_01.2", nil},
		{"trimmed", "  key-1  ", nil},
		{"empty", "", ErrInvalidIdempotencyKey},
		{"whitespace only", "   ", ErrInvalidIdempotencyKey},
		{"too long", strings.Repeat("a", MaxIdempotencyKeyLen+1), ErrInvalidIdempotencyKey},
		{"space inside", "a b", ErrInvalidIdempotencyKey},
		{"slash", "a/b", ErrInvalidIdempotencyKey},
		{"newline", "a\nb", ErrInvalidIdempotencyKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseIdempotencyKey(tt.in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && got != IdempotencyKey(strings.TrimSpace(tt.in)) {
				t.Fatalf("got %q", got)
			}
		})
	}
}

func TestParseOccurrenceID(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr error
	}{
		{"plain", "deploy-42", nil},
		{"trimmed", "  run:1  ", nil},
		{"empty", "", ErrInvalidOccurrenceID},
		{"whitespace only", "   ", ErrInvalidOccurrenceID},
		{"too long", strings.Repeat("a", MaxOccurrenceIDLen+1), ErrInvalidOccurrenceID},
		{"slash", "a/b", ErrInvalidOccurrenceID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseOccurrenceID(tt.in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && got != OccurrenceID(strings.TrimSpace(tt.in)) {
				t.Fatalf("got %q", got)
			}
		})
	}
}

func TestOccurrenceKeyIsNamespaced(t *testing.T) {
	if got := OccurrenceKey("shared"); got != "occ_shared" {
		t.Fatalf("key = %q", got)
	}
	// An occurrence cannot collide with a raw idempotency key of the same name.
	if OccurrenceKey("shared") == IdempotencyKey("shared") {
		t.Fatal("occurrence key must be namespaced")
	}
	if len(OccurrenceKey(OccurrenceID(strings.Repeat("a", MaxOccurrenceIDLen)))) > MaxIdempotencyKeyLen {
		t.Fatal("occurrence key exceeds max length")
	}
}
