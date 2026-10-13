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
