package domain

import (
	"testing"
	"time"
)

func TestBackoffDelay(t *testing.T) {
	b := Backoff{Base: time.Second, Cap: time.Minute}
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{0, time.Second},
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 16 * time.Second},
		{6, 32 * time.Second},
		{7, time.Minute},
		{100, time.Minute},
	}
	for _, tt := range tests {
		if got := b.Delay(tt.attempt); got != tt.want {
			t.Errorf("Delay(%d) = %v, want %v", tt.attempt, got, tt.want)
		}
	}
}

func TestBackoffZeroBase(t *testing.T) {
	if got := (Backoff{}).Delay(3); got != 0 {
		t.Fatalf("got %v, want 0", got)
	}
}

func TestBackoffDefaultsCapToBase(t *testing.T) {
	b := Backoff{Base: 2 * time.Second}
	if got := b.Delay(10); got != 2*time.Second {
		t.Fatalf("got %v, want cap-at-base", got)
	}
}
