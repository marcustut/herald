package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestNewLink(t *testing.T) {
	tests := []struct {
		name    string
		label   string
		url     string
		wantErr error
	}{
		{"https", "PR", "https://github.com/x/y/pull/1", nil},
		{"http", "CI", "http://ci.internal/run/2", nil},
		{"query and fragment", "log", "https://x.test/a?b=c#d", nil},
		{"empty label", "", "https://x.test", ErrInvalidLink},
		{"blank label", "   ", "https://x.test", ErrInvalidLink},
		{"label too long", strings.Repeat("a", MaxLinkLabelLen+1), "https://x.test", ErrInvalidLink},
		{"empty url", "PR", "", ErrInvalidLink},
		{"relative url", "PR", "/pull/1", ErrInvalidLink},
		{"protocol relative", "PR", "//evil.test", ErrInvalidLink},
		{"javascript scheme", "click", "javascript:alert(1)", ErrInvalidLink},
		{"data scheme", "click", "data:text/html,<script>", ErrInvalidLink},
		{"missing host", "PR", "https://", ErrInvalidLink},
		{"control character", "PR", "https://a\x7fb", ErrInvalidLink},
		{"url too long", "PR", "https://x.test/" + strings.Repeat("a", MaxLinkURLLen), ErrInvalidLink},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewLink(tt.label, tt.url)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			if got.Label != strings.TrimSpace(tt.label) || got.URL != tt.url {
				t.Fatalf("got %+v", got)
			}
		})
	}
}
