package app

import (
	"context"

	"github.com/marcustut/herald/internal/domain"
)

// Issuer mints and revokes credentials. The raw token is returned exactly once,
// at issue time.
type Issuer struct {
	Store   CredentialStore
	Hasher  Hasher
	Secrets SecretGenerator
	Clock   Clock
}

// IssueProjectToken mints a credential scoped to a single project.
func (i Issuer) IssueProjectToken(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) (string, error) {
	if _, err := domain.NewPrincipal(tenant, project); err != nil {
		return "", err
	}
	raw, err := domain.NewProjectToken(project, i.Secrets.NewSecret())
	if err != nil {
		return "", err
	}
	return raw, i.put(ctx, tenant, domain.TokenProject, []domain.ProjectID{project}, raw)
}

// IssueScopedToken mints a credential authorized for a set of projects.
func (i Issuer) IssueScopedToken(ctx context.Context, tenant domain.TenantID, projects []domain.ProjectID) (string, error) {
	if len(projects) == 0 {
		return "", ErrInvalidInput
	}
	for _, p := range projects {
		if _, err := domain.NewPrincipal(tenant, p); err != nil {
			return "", err
		}
	}
	raw, err := domain.NewScopedToken(i.Secrets.NewSecret())
	if err != nil {
		return "", err
	}
	projects = append([]domain.ProjectID(nil), projects...)
	return raw, i.put(ctx, tenant, domain.TokenScoped, projects, raw)
}

// RevokeProject invalidates the project-scoped credential for project.
func (i Issuer) RevokeProject(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) error {
	cred, err := i.Store.ProjectCredential(ctx, tenant, project)
	if err != nil {
		return err
	}
	cred.RevokedAt = i.Clock.Now()
	return i.Store.Put(ctx, cred)
}

// RevokeCredential revokes the credential with token hash under tenant. A hash
// that belongs to another tenant reports ErrCredentialNotFound, so knowing a
// foreign hash is not enough to revoke it.
func (i Issuer) RevokeCredential(ctx context.Context, tenant domain.TenantID, hash string) error {
	cred, err := i.Store.ByHash(ctx, hash)
	if err != nil {
		return err
	}
	if cred.TenantID != tenant {
		return ErrCredentialNotFound
	}
	cred.RevokedAt = i.Clock.Now()
	return i.Store.Put(ctx, cred)
}

func (i Issuer) put(ctx context.Context, tenant domain.TenantID, kind domain.TokenKind, projects []domain.ProjectID, raw string) error {
	return i.Store.Put(ctx, Credential{
		Hash:      i.Hasher.Hash(raw),
		TenantID:  tenant,
		Kind:      kind,
		Projects:  projects,
		CreatedAt: i.Clock.Now(),
	})
}
