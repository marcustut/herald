package domain

import (
	"errors"
	"testing"
)

func TestParseStatus(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Status
		wantErr error
	}{
		{"success", "success", StatusSuccess, nil},
		{"failure", "failure", StatusFailure, nil},
		{"blocked", "blocked", StatusBlocked, nil},
		{"mixed case", "Success", StatusSuccess, nil},
		{"surrounding whitespace", "  blocked\t", StatusBlocked, nil},
		{"empty", "", "", ErrInvalidStatus},
		{"unknown word", "done", "", ErrInvalidStatus},
		{"near miss", "successes", "", ErrInvalidStatus},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseStatus(tt.in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStatusValid(t *testing.T) {
	for _, s := range []Status{StatusSuccess, StatusFailure, StatusBlocked} {
		if !s.Valid() {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range []Status{"", "SUCCESS", "ok"} {
		if s.Valid() {
			t.Errorf("%q should be invalid", s)
		}
	}
}

func TestStatusString(t *testing.T) {
	if got := StatusFailure.String(); got != "failure" {
		t.Fatalf("got %q", got)
	}
}
