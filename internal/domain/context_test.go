package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestNewContext(t *testing.T) {
	tests := []struct {
		name    string
		in      map[string]string
		wantErr error
	}{
		{"nil is allowed", nil, nil},
		{"empty", map[string]string{}, nil},
		{"host", map[string]string{"host": "build-01"}, nil},
		{"run id", map[string]string{"run-id": "2026-10-12_01"}, nil},
		{"too many entries", manyEntries(MaxContextEntries + 1), ErrInvalidContext},
		{"empty key", map[string]string{"": "x"}, ErrInvalidContext},
		{"key too long", map[string]string{strings.Repeat("k", MaxContextKeyLen+1): "x"}, ErrInvalidContext},
		{"uppercase key", map[string]string{"Host": "x"}, ErrInvalidContext},
		{"reserved key", map[string]string{"agent": "spoof"}, ErrInvalidContext},
		{"value too long", map[string]string{"host": strings.Repeat("v", MaxContextValueLen+1)}, ErrInvalidContext},
		{"control char in value", map[string]string{"host": "a\x00b"}, ErrInvalidContext},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewContext(tt.in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			if len(got) != len(tt.in) {
				t.Fatalf("got %v, want %v", got, tt.in)
			}
		})
	}
}

func manyEntries(n int) map[string]string {
	m := make(map[string]string, n)
	for i := 0; i < n; i++ {
		m["k"+strings.Repeat("0", i)] = "v"
	}
	return m
}

func TestContextHost(t *testing.T) {
	ctx, err := NewContext(map[string]string{"host": "build-01", "run-id": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Host() != "build-01" {
		t.Fatalf("host = %q", ctx.Host())
	}
	empty, _ := NewContext(nil)
	if empty.Host() != "" {
		t.Fatalf("host = %q, want empty", empty.Host())
	}
}

func TestContextCopiesInput(t *testing.T) {
	in := map[string]string{"host": "build-01"}
	ctx, err := NewContext(in)
	if err != nil {
		t.Fatal(err)
	}
	in["host"] = "mutated"
	if ctx.Host() != "build-01" {
		t.Fatal("context should own a copy of its input")
	}
}
