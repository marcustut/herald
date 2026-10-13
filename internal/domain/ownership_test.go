package domain

import (
	"errors"
	"testing"
	"time"
)

var ownNow = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func TestNewAccount(t *testing.T) {
	tests := []struct {
		name    string
		id      AccountID
		label   string
		at      time.Time
		wantErr error
	}{
		{"valid", "acct_1", "Acme", ownNow, nil},
		{"missing id", "", "Acme", ownNow, ErrInvalidAccount},
		{"blank name", "acct_1", "   ", ownNow, ErrInvalidAccount},
		{"zero time", "acct_1", "Acme", time.Time{}, ErrInvalidAccount},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewAccount(tt.id, tt.label, tt.at)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && (got.ID != tt.id || got.Name != tt.label) {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestNewUser(t *testing.T) {
	tests := []struct {
		name    string
		id      UserID
		email   string
		want    string
		wantErr error
	}{
		{"valid", "usr_1", "owner@example.com", "owner@example.com", nil},
		{"trims and lowers domain", "usr_1", " owner@Example.COM ", "owner@example.com", nil},
		{"empty email", "usr_1", "", "", ErrInvalidEmail},
		{"no at", "usr_1", "ownerexample.com", "", ErrInvalidEmail},
		{"two at", "usr_1", "a@b@example.com", "", ErrInvalidEmail},
		{"no local", "usr_1", "@example.com", "", ErrInvalidEmail},
		{"no dotted domain", "usr_1", "owner@example", "", ErrInvalidEmail},
		{"trailing dot", "usr_1", "owner@example.", "", ErrInvalidEmail},
		{"space", "usr_1", "ow ner@example.com", "", ErrInvalidEmail},
		{"missing id", "", "owner@example.com", "", ErrInvalidUser},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewUser(tt.id, "acct_1", tt.email, ownNow)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && got.Email != tt.want {
				t.Fatalf("email = %q, want %q", got.Email, tt.want)
			}
		})
	}

	u, err := NewUser("usr_1", "acct_1", "owner@example.com", ownNow)
	if err != nil {
		t.Fatal(err)
	}
	if !u.OwnedByAccount("acct_1") || u.OwnedByAccount("acct_2") {
		t.Fatalf("OwnedByAccount = %v", u.OwnedByAccount("acct_1"))
	}
	if _, err := NewUser("usr_1", "", "owner@example.com", ownNow); !errors.Is(err, ErrInvalidUser) {
		t.Fatalf("empty account: err = %v", err)
	}
}

func TestNewTenant(t *testing.T) {
	if _, err := NewTenant("ten_1", "acct_1", "Acme Prod", ownNow); err != nil {
		t.Fatalf("valid: %v", err)
	}
	if _, err := NewTenant("", "acct_1", "Acme Prod", ownNow); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("missing id: err = %v", err)
	}
	if _, err := NewTenant("ten_1", "", "Acme Prod", ownNow); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("missing account: err = %v", err)
	}
	if _, err := NewTenant("ten_1", "acct_1", "", ownNow); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("blank name: err = %v", err)
	}
	tn, err := NewTenant("ten_1", "acct_1", "Acme Prod", ownNow)
	if err != nil {
		t.Fatal(err)
	}
	if !tn.OwnedByAccount("acct_1") || tn.OwnedByAccount("acct_2") {
		t.Fatal("OwnedByAccount")
	}
}

func TestNewProject(t *testing.T) {
	if _, err := NewProject("axis", "ten_1", "Axis", ownNow); err != nil {
		t.Fatalf("valid: %v", err)
	}
	if _, err := NewProject("Bad Project", "ten_1", "Axis", ownNow); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("bad slug: err = %v", err)
	}
	if _, err := NewProject("axis", "", "Axis", ownNow); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("missing tenant: err = %v", err)
	}
	p, err := NewProject("axis", "ten_1", "Axis", ownNow)
	if err != nil {
		t.Fatal(err)
	}
	if !p.OwnedBy("ten_1") || p.OwnedBy("ten_2") {
		t.Fatal("OwnedBy")
	}
}

func TestNewDestination(t *testing.T) {
	base := Destination{
		ID:        "dst_1",
		TenantID:  "ten_1",
		ProjectID: "axis",
		Kind:      DestinationEmail,
		Target:    "owner@example.com",
		Enabled:   true,
		CreatedAt: ownNow,
	}
	tests := []struct {
		name    string
		mutate  func(*Destination)
		wantErr error
	}{
		{"valid", func(*Destination) {}, nil},
		{"trims target", func(d *Destination) { d.Target = "  owner@example.com  " }, nil},
		{"missing id", func(d *Destination) { d.ID = "" }, ErrInvalidDestination},
		{"missing tenant", func(d *Destination) { d.TenantID = "" }, ErrInvalidDestination},
		{"bad project", func(d *Destination) { d.ProjectID = "Bad" }, ErrInvalidDestination},
		{"unknown kind", func(d *Destination) { d.Kind = "sms" }, ErrInvalidDestinationKind},
		{"blank target", func(d *Destination) { d.Target = "  " }, ErrInvalidDestination},
		{"email target must be an address", func(d *Destination) { d.Target = "not-an-address" }, ErrInvalidDestination},
		{"webhook target must be absolute", func(d *Destination) { d.Kind = DestinationWebhook; d.Target = "file:///etc/passwd" }, ErrInvalidDestination},
		{"webhook target needs a host", func(d *Destination) { d.Kind = DestinationWebhook; d.Target = "//evil.example/hook" }, ErrInvalidDestination},
		{"valid webhook", func(d *Destination) { d.Kind = DestinationWebhook; d.Target = "https://hooks.example.com/x" }, nil},
		{"zero time", func(d *Destination) { d.CreatedAt = time.Time{} }, ErrInvalidDestination},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := base
			tt.mutate(&in)
			got, err := NewDestination(in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			if !got.OwnedBy("ten_1", "axis") || got.OwnedBy("ten_2", "axis") || got.OwnedBy("ten_1", "other") {
				t.Fatalf("OwnedBy = %+v", got)
			}
		})
	}
}

func TestDestinationVerified(t *testing.T) {
	d := Destination{Kind: DestinationEmail}
	if d.Verified() {
		t.Fatal("email destination without verified_at reports verified")
	}
	d.VerifiedAt = ownNow
	if !d.Verified() {
		t.Fatal("email destination with verified_at reports unverified")
	}
}
