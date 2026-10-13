package domain

import (
	"errors"
	"testing"
)

func TestNewPrincipal(t *testing.T) {
	tests := []struct {
		name    string
		tenant  TenantID
		project ProjectID
		wantErr error
	}{
		{"valid", "ten_1", "axis", nil},
		{"missing tenant", "", "axis", ErrInvalidID},
		{"blank tenant", "   ", "axis", ErrInvalidID},
		{"missing project", "ten_1", "", ErrInvalidID},
		{"malformed project", "ten_1", "Bad Project", ErrInvalidID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewPrincipal(tt.tenant, tt.project)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			if got.TenantID != tt.tenant || got.ProjectID != tt.project {
				t.Fatalf("got %+v", got)
			}
		})
	}
}
