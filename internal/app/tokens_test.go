package app

import (
	"context"
	"errors"
	"testing"

	"github.com/marcustut/herald/internal/domain"
)

func TestIssueProjectTokenStoresHashedCredential(t *testing.T) {
	store := newFakeStore()
	raw, err := newIssuer(store).IssueProjectToken(context.Background(), "ten_1", "axis")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := domain.ParseToken(raw); err != nil {
		t.Fatalf("issued token does not parse: %v", err)
	}
	cred, err := store.ByHash(context.Background(), fakeHasher{}.Hash(raw))
	if err != nil {
		t.Fatal(err)
	}
	if cred.TenantID != "ten_1" || cred.Kind != domain.TokenProject {
		t.Fatalf("stored credential = %+v", cred)
	}
	if !cred.Allows("axis") || cred.Allows("herald") {
		t.Fatalf("projects = %v", cred.Projects)
	}
	if cred.Revoked() {
		t.Fatal("fresh credential must not be revoked")
	}
	if !cred.CreatedAt.Equal(testNow) {
		t.Fatalf("created at = %v", cred.CreatedAt)
	}
	if _, ok := store.byHash[raw]; ok {
		t.Fatal("raw token must not be stored")
	}
}

func TestIssueProjectTokenRejectsInvalidProject(t *testing.T) {
	_, err := newIssuer(newFakeStore()).IssueProjectToken(context.Background(), "ten_1", "Bad")
	if !errors.Is(err, domain.ErrInvalidID) {
		t.Fatalf("err = %v", err)
	}
}

func TestIssueProjectTokenRejectsShortSecret(t *testing.T) {
	iss := Issuer{Store: newFakeStore(), Hasher: fakeHasher{}, Secrets: shortSecrets{}, Clock: fakeClock{testNow}}
	if _, err := iss.IssueProjectToken(context.Background(), "ten_1", "axis"); !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("err = %v", err)
	}
}

func TestIssueScopedToken(t *testing.T) {
	store := newFakeStore()
	raw, err := newIssuer(store).IssueScopedToken(context.Background(), "ten_1", []domain.ProjectID{"axis", "herald"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := domain.ParseToken(raw)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Kind() != domain.TokenScoped {
		t.Fatalf("kind = %q", tok.Kind())
	}
	cred, err := store.ByHash(context.Background(), fakeHasher{}.Hash(raw))
	if err != nil {
		t.Fatal(err)
	}
	if cred.Kind != domain.TokenScoped {
		t.Fatalf("kind = %q", cred.Kind)
	}
	if !cred.Allows("axis") || !cred.Allows("herald") || cred.Allows("beta") {
		t.Fatalf("projects = %v", cred.Projects)
	}
}

func TestIssueScopedTokenRejectsEmpty(t *testing.T) {
	_, err := newIssuer(newFakeStore()).IssueScopedToken(context.Background(), "ten_1", nil)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err = %v", err)
	}
}

func TestIssueScopedTokenRejectsInvalidProject(t *testing.T) {
	_, err := newIssuer(newFakeStore()).IssueScopedToken(context.Background(), "ten_1", []domain.ProjectID{"axis", "Bad"})
	if !errors.Is(err, domain.ErrInvalidID) {
		t.Fatalf("err = %v", err)
	}
}

func TestIssueScopedTokenRejectsShortSecret(t *testing.T) {
	iss := Issuer{Store: newFakeStore(), Hasher: fakeHasher{}, Secrets: shortSecrets{}, Clock: fakeClock{testNow}}
	if _, err := iss.IssueScopedToken(context.Background(), "ten_1", []domain.ProjectID{"axis"}); !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("err = %v", err)
	}
}

func TestRevokeProject(t *testing.T) {
	store := newFakeStore()
	iss := newIssuer(store)
	raw, err := iss.IssueProjectToken(context.Background(), "ten_1", "axis")
	if err != nil {
		t.Fatal(err)
	}
	if err := iss.RevokeProject(context.Background(), "ten_1", "axis"); err != nil {
		t.Fatal(err)
	}
	cred, err := store.ByHash(context.Background(), fakeHasher{}.Hash(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !cred.Revoked() {
		t.Fatal("credential should be revoked")
	}
}

func TestRevokeProjectNotFound(t *testing.T) {
	err := newIssuer(newFakeStore()).RevokeProject(context.Background(), "ten_1", "axis")
	if !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// A scoped token that merely covers a project must not be revoked by
// RevokeProject, which targets the project token for that project.
func TestRevokeProjectIgnoresScopedToken(t *testing.T) {
	store := newFakeStore()
	iss := newIssuer(store)
	if _, err := iss.IssueScopedToken(context.Background(), "ten_1", []domain.ProjectID{"axis", "herald"}); err != nil {
		t.Fatal(err)
	}
	if err := iss.RevokeProject(context.Background(), "ten_1", "axis"); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("err = %v, want not found", err)
	}
}

func TestCredentialAllows(t *testing.T) {
	cred := Credential{Projects: []domain.ProjectID{"axis"}}
	if cred.Allows("") {
		t.Error("empty project should never be allowed")
	}
	if !cred.Allows("axis") {
		t.Error("listed project should be allowed")
	}
	if cred.Allows("herald") {
		t.Error("unlisted project should not be allowed")
	}
}

func TestCredentialRevoked(t *testing.T) {
	if (Credential{}).Revoked() {
		t.Error("zero credential should be active")
	}
	if !(Credential{RevokedAt: testNow}).Revoked() {
		t.Error("credential with revoked at should be revoked")
	}
}
