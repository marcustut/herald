package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/marcustut/herald/internal/domain"
)

func TestAuthenticateProjectToken(t *testing.T) {
	store := newFakeStore()
	raw, err := newIssuer(store).IssueProjectToken(context.Background(), "ten_1", "axis")
	if err != nil {
		t.Fatal(err)
	}
	auth := Authenticator{Store: store, Hasher: fakeHasher{}}

	for _, requested := range []domain.ProjectID{"", "axis"} {
		p, err := auth.Authenticate(context.Background(), raw, requested)
		if err != nil {
			t.Fatalf("requested %q: %v", requested, err)
		}
		if p.TenantID != "ten_1" || p.ProjectID != "axis" {
			t.Fatalf("got %+v", p)
		}
	}

	if _, err := auth.Authenticate(context.Background(), raw, "other"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("mismatched project: err = %v", err)
	}
}

func TestAuthenticateRevoked(t *testing.T) {
	store := newFakeStore()
	iss := newIssuer(store)
	raw, err := iss.IssueProjectToken(context.Background(), "ten_1", "axis")
	if err != nil {
		t.Fatal(err)
	}
	if err := iss.RevokeProject(context.Background(), "ten_1", "axis"); err != nil {
		t.Fatal(err)
	}
	auth := Authenticator{Store: store, Hasher: fakeHasher{}}
	if _, err := auth.Authenticate(context.Background(), raw, ""); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked token: err = %v", err)
	}
}

func TestAuthenticateUnknownToken(t *testing.T) {
	auth := Authenticator{Store: newFakeStore(), Hasher: fakeHasher{}}
	raw := "herald_axis_" + strings.Repeat("s", domain.MinTokenSecretLen)
	if _, err := auth.Authenticate(context.Background(), raw, ""); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("unknown token: err = %v", err)
	}
}

func TestAuthenticateMalformedToken(t *testing.T) {
	auth := Authenticator{Store: newFakeStore(), Hasher: fakeHasher{}}
	if _, err := auth.Authenticate(context.Background(), "garbage", ""); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("malformed token: err = %v", err)
	}
}

func TestAuthenticateStoreErrorIsNotUnauthenticated(t *testing.T) {
	store := newFakeStore()
	raw, err := newIssuer(store).IssueProjectToken(context.Background(), "ten_1", "axis")
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("db down")
	store.err = boom
	auth := Authenticator{Store: store, Hasher: fakeHasher{}}
	if _, err := auth.Authenticate(context.Background(), raw, ""); !errors.Is(err, boom) {
		t.Fatalf("store error: err = %v", err)
	}
}

func TestAuthenticateScopedToken(t *testing.T) {
	store := newFakeStore()
	raw, err := newIssuer(store).IssueScopedToken(context.Background(), "ten_1", []domain.ProjectID{"axis", "herald"})
	if err != nil {
		t.Fatal(err)
	}
	auth := Authenticator{Store: store, Hasher: fakeHasher{}}

	p, err := auth.Authenticate(context.Background(), raw, "herald")
	if err != nil {
		t.Fatal(err)
	}
	if p.ProjectID != "herald" {
		t.Fatalf("got %+v", p)
	}

	if _, err := auth.Authenticate(context.Background(), raw, "beta"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("out of scope: err = %v", err)
	}
	if _, err := auth.Authenticate(context.Background(), raw, ""); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("missing project: err = %v", err)
	}
}
