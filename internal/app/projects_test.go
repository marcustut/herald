package app

import (
	"context"
	"errors"
	"testing"

	"github.com/marcustut/herald/internal/domain"
)

type projectKey struct {
	tenant  domain.TenantID
	project domain.ProjectID
}

type fakeProjectStore struct {
	byKey map[projectKey]domain.Project
	err   error
}

func newFakeProjectStore() *fakeProjectStore {
	return &fakeProjectStore{byKey: map[projectKey]domain.Project{}}
}

func (s *fakeProjectStore) PutProject(_ context.Context, p domain.Project) error {
	if s.err != nil {
		return s.err
	}
	s.byKey[projectKey{p.TenantID, p.ID}] = p
	return nil
}

func (s *fakeProjectStore) Project(_ context.Context, tenant domain.TenantID, id domain.ProjectID) (domain.Project, bool, error) {
	if s.err != nil {
		return domain.Project{}, false, s.err
	}
	p, ok := s.byKey[projectKey{tenant, id}]
	return p, ok, nil
}

func (s *fakeProjectStore) Projects(_ context.Context, tenant domain.TenantID) ([]domain.Project, error) {
	if s.err != nil {
		return nil, s.err
	}
	var out []domain.Project
	for k, p := range s.byKey {
		if k.tenant == tenant {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *fakeProjectStore) DeleteProject(_ context.Context, tenant domain.TenantID, id domain.ProjectID) error {
	if s.err != nil {
		return s.err
	}
	if _, ok := s.byKey[projectKey{tenant, id}]; !ok {
		return ErrProjectNotFound
	}
	delete(s.byKey, projectKey{tenant, id})
	return nil
}

func newProjects(store *fakeProjectStore, creds *fakeStore) Projects {
	return Projects{
		Store:       store,
		Credentials: creds,
		Issuer:      Issuer{Store: creds, Hasher: fakeHasher{}, Secrets: &seqSecrets{}, Clock: fakeClock{testNow}},
		Clock:       fakeClock{testNow},
	}
}

func TestProjectsCreateListDelete(t *testing.T) {
	store := newFakeProjectStore()
	p := newProjects(store, newFakeStore())
	ctx := context.Background()

	if _, err := p.CreateProject(ctx, "ten_1", "alpha", "Alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateProject(ctx, "ten_1", "beta", "Beta"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateProject(ctx, "ten_2", "alpha", "Other"); err != nil {
		t.Fatal(err)
	}

	own, err := p.ListProjects(ctx, "ten_1")
	if err != nil || len(own) != 2 {
		t.Fatalf("tenant 1 projects = %+v, err %v", own, err)
	}
	// The same slug is a distinct row per tenant.
	if _, ok, _ := store.Project(ctx, "ten_2", "alpha"); !ok {
		t.Fatal("tenant 2 lost its own alpha")
	}

	if err := p.DeleteProject(ctx, "ten_1", "alpha"); err != nil {
		t.Fatal(err)
	}
	if after, _ := p.ListProjects(ctx, "ten_1"); len(after) != 1 {
		t.Fatalf("after delete = %+v", after)
	}
	// A foreign tenant cannot delete the project; it stays not-found.
	if err := p.DeleteProject(ctx, "ten_2", "beta"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("cross-tenant delete err = %v", err)
	}
	if _, ok, _ := store.Project(ctx, "ten_1", "beta"); !ok {
		t.Fatal("cross-tenant delete removed the project")
	}
}

func TestIssueProjectTokenRequiresOwnProject(t *testing.T) {
	store := newFakeProjectStore()
	creds := newFakeStore()
	p := newProjects(store, creds)
	ctx := context.Background()

	if _, err := p.CreateProject(ctx, "ten_1", "alpha", "Alpha"); err != nil {
		t.Fatal(err)
	}

	raw, err := p.IssueProjectToken(ctx, "ten_1", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := domain.ParseToken(raw); err != nil {
		t.Fatalf("issued token does not parse: %v", err)
	}

	// Unknown project under the tenant.
	if _, err := p.IssueProjectToken(ctx, "ten_1", "ghost"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("unknown project err = %v", err)
	}
	// The project exists, but under another tenant: it must not be addressable.
	if _, err := p.IssueProjectToken(ctx, "ten_2", "alpha"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("cross-tenant issue err = %v", err)
	}
	// Malformed slug is a validation error, not a not-found.
	if _, err := p.IssueProjectToken(ctx, "ten_1", "Bad Slug"); !errors.Is(err, domain.ErrInvalidID) {
		t.Fatalf("invalid slug err = %v", err)
	}
}

func TestIssueScopedTokenCannotEscalateScope(t *testing.T) {
	store := newFakeProjectStore()
	creds := newFakeStore()
	p := newProjects(store, creds)
	ctx := context.Background()

	for _, tc := range []struct {
		tenant  domain.TenantID
		project domain.ProjectID
	}{{"ten_1", "alpha"}, {"ten_1", "beta"}, {"ten_2", "secret"}} {
		if _, err := p.CreateProject(ctx, tc.tenant, tc.project, string(tc.project)); err != nil {
			t.Fatal(err)
		}
	}

	// A scope naming a foreign project is refused outright and mints nothing.
	if _, err := p.IssueScopedToken(ctx, "ten_1", []domain.ProjectID{"alpha", "secret"}); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("foreign project err = %v", err)
	}
	if len(creds.byHash) != 0 {
		t.Fatalf("a credential was minted for a rejected scope: %+v", creds.byHash)
	}

	raw, err := p.IssueScopedToken(ctx, "ten_1", []domain.ProjectID{"alpha", "beta", "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	cred, err := creds.ByHash(ctx, fakeHasher{}.Hash(raw))
	if err != nil {
		t.Fatal(err)
	}
	if cred.TenantID != "ten_1" || !cred.Allows("alpha") || !cred.Allows("beta") {
		t.Fatalf("stored scope = %+v", cred)
	}
	if len(cred.Projects) != 2 {
		t.Fatalf("duplicate project slugs were stored: %v", cred.Projects)
	}

	// The token resolves only its own tenant's projects.
	auth := Authenticator{Store: creds, Hasher: fakeHasher{}}
	if _, err := auth.Authenticate(ctx, raw, "alpha"); err != nil {
		t.Fatalf("scoped token for alpha: %v", err)
	}
	if _, err := auth.Authenticate(ctx, raw, "secret"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("scoped token widened to a foreign project: %v", err)
	}
}

func TestRotateProjectTokenRevokesPrevious(t *testing.T) {
	store := newFakeProjectStore()
	creds := newFakeStore()
	p := newProjects(store, creds)
	ctx := context.Background()

	if _, err := p.CreateProject(ctx, "ten_1", "alpha", "Alpha"); err != nil {
		t.Fatal(err)
	}
	first, err := p.IssueProjectToken(ctx, "ten_1", "alpha")
	if err != nil {
		t.Fatal(err)
	}

	second, err := p.RotateProjectToken(ctx, "ten_1", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("rotation reissued the same secret")
	}

	auth := Authenticator{Store: creds, Hasher: fakeHasher{}}
	if _, err := auth.Authenticate(ctx, first, "alpha"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("old token still authenticates after rotation: %v", err)
	}
	if _, err := auth.Authenticate(ctx, second, "alpha"); err != nil {
		t.Fatalf("new token does not authenticate: %v", err)
	}

	// Rotating a project the tenant does not own is refused.
	if _, err := p.RotateProjectToken(ctx, "ten_2", "alpha"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("cross-tenant rotate err = %v", err)
	}
}

func TestRevokeTokenRejectsForeignTenant(t *testing.T) {
	store := newFakeProjectStore()
	creds := newFakeStore()
	p := newProjects(store, creds)
	ctx := context.Background()

	if _, err := p.CreateProject(ctx, "ten_1", "alpha", "Alpha"); err != nil {
		t.Fatal(err)
	}
	raw, err := p.IssueProjectToken(ctx, "ten_1", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	hash := fakeHasher{}.Hash(raw)

	if err := p.RevokeToken(ctx, "ten_2", hash); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("cross-tenant revoke err = %v", err)
	}
	auth := Authenticator{Store: creds, Hasher: fakeHasher{}}
	if _, err := auth.Authenticate(ctx, raw, "alpha"); err != nil {
		t.Fatalf("token was revoked by a foreign tenant: %v", err)
	}

	if err := p.RevokeToken(ctx, "ten_1", hash); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Authenticate(ctx, raw, "alpha"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked token still authenticates: %v", err)
	}

	if err := p.RevokeToken(ctx, "ten_1", "unknown"); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("unknown hash err = %v", err)
	}
}

func TestListTokensIsTenantScoped(t *testing.T) {
	store := newFakeProjectStore()
	creds := newFakeStore()
	p := newProjects(store, creds)
	ctx := context.Background()

	for _, tc := range []struct {
		tenant  domain.TenantID
		project domain.ProjectID
	}{{"ten_1", "alpha"}, {"ten_2", "beta"}} {
		if _, err := p.CreateProject(ctx, tc.tenant, tc.project, string(tc.project)); err != nil {
			t.Fatal(err)
		}
		if _, err := p.IssueProjectToken(ctx, tc.tenant, tc.project); err != nil {
			t.Fatal(err)
		}
	}

	one, err := p.ListTokens(ctx, "ten_1")
	if err != nil || len(one) != 1 || one[0].TenantID != "ten_1" {
		t.Fatalf("tenant 1 tokens = %+v, err %v", one, err)
	}
}

// Creating a project records the project_created funnel stage.
func TestCreateProjectObservesFunnel(t *testing.T) {
	obs := &captureObserver{}
	p := newProjects(newFakeProjectStore(), newFakeStore())
	p.Observer = obs

	if _, err := p.CreateProject(context.Background(), "ten_1", "alpha", "Alpha"); err != nil {
		t.Fatal(err)
	}
	if len(obs.funnel) != 1 || obs.funnel[0] != string(FunnelProjectCreated) {
		t.Fatalf("funnel = %v, want [project_created]", obs.funnel)
	}
}
