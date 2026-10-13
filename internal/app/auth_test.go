package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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

func TestAuthenticateRecordsLastUsed(t *testing.T) {
	store := newFakeStore()
	raw, err := newIssuer(store).IssueProjectToken(context.Background(), "ten_1", "axis")
	if err != nil {
		t.Fatal(err)
	}
	hash := fakeHasher{}.Hash(raw)
	auth := Authenticator{Store: store, Hasher: fakeHasher{}}
	if _, err := auth.Authenticate(context.Background(), raw, ""); err != nil {
		t.Fatal(err)
	}
	got, err := store.ByHash(context.Background(), hash)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastUsedAt.IsZero() {
		t.Fatal("last-used was not recorded on authentication")
	}
}

func TestAuthenticateThrottlesLastUsed(t *testing.T) {
	store := newFakeStore()
	raw, err := newIssuer(store).IssueProjectToken(context.Background(), "ten_1", "axis")
	if err != nil {
		t.Fatal(err)
	}
	hash := fakeHasher{}.Hash(raw)
	auth := Authenticator{Store: store, Hasher: fakeHasher{}}

	// Within the window: no write.
	fresh := store.byHash[hash]
	fresh.LastUsedAt = time.Now().Add(-time.Minute)
	store.byHash[hash] = fresh
	if _, err := auth.Authenticate(context.Background(), raw, ""); err != nil {
		t.Fatal(err)
	}
	if store.touches != 0 {
		t.Fatalf("within window: touches = %d, want 0", store.touches)
	}

	// Older than the window: write once.
	stale := store.byHash[hash]
	stale.LastUsedAt = time.Now().Add(-lastUsedWindow - time.Minute)
	store.byHash[hash] = stale
	if _, err := auth.Authenticate(context.Background(), raw, ""); err != nil {
		t.Fatal(err)
	}
	if store.touches != 1 {
		t.Fatalf("past window: touches = %d, want 1", store.touches)
	}
}

func TestAuthenticateLastUsedWriteFailureIsBestEffort(t *testing.T) {
	store := newFakeStore()
	raw, err := newIssuer(store).IssueProjectToken(context.Background(), "ten_1", "axis")
	if err != nil {
		t.Fatal(err)
	}
	store.touchErr = errors.New("db down")
	auth := Authenticator{Store: store, Hasher: fakeHasher{}}
	p, err := auth.Authenticate(context.Background(), raw, "")
	if err != nil {
		t.Fatalf("a failed telemetry write must not fail authentication: %v", err)
	}
	if p.TenantID != "ten_1" || p.ProjectID != "axis" {
		t.Fatalf("got %+v", p)
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
