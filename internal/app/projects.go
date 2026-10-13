package app

import (
	"context"

	"github.com/marcustut/herald/internal/domain"
)

// Projects is the hosted project and token self-service use case behind the
// cloud surface. Every method takes the tenant the caller acts for — taken
// from the authenticated session by the transport, never from a request body —
// and derives ownership from it. A project or token that belongs to another
// tenant is reported not-found and is never read, mutated, or named by a newly
// issued token.
type Projects struct {
	Store       ProjectStore
	Credentials CredentialStore
	Issuer      Issuer
	Clock       Clock
}

// CreateProject creates (or renames) a project under tenant. The id is the
// project slug and is unique within the tenant.
func (p Projects) CreateProject(ctx context.Context, tenant domain.TenantID, id domain.ProjectID, name string) (domain.Project, error) {
	project, err := domain.NewProject(id, tenant, name, p.Clock.Now())
	if err != nil {
		return domain.Project{}, err
	}
	if err := p.Store.PutProject(ctx, project); err != nil {
		return domain.Project{}, err
	}
	return project, nil
}

// ListProjects returns every project owned by tenant, newest first.
func (p Projects) ListProjects(ctx context.Context, tenant domain.TenantID) ([]domain.Project, error) {
	return p.Store.Projects(ctx, tenant)
}

// DeleteProject removes the project and its destinations. A project owned by
// another tenant reports ErrProjectNotFound.
func (p Projects) DeleteProject(ctx context.Context, tenant domain.TenantID, id domain.ProjectID) error {
	return p.Store.DeleteProject(ctx, tenant, id)
}

// IssueProjectToken mints a project-scoped credential for a project tenant
// owns. The raw token is returned exactly once; only its hash is stored.
func (p Projects) IssueProjectToken(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) (string, error) {
	if err := p.requireProject(ctx, tenant, project); err != nil {
		return "", err
	}
	return p.Issuer.IssueProjectToken(ctx, tenant, project)
}

// IssueScopedToken mints a multi-project credential for projects tenant owns.
// Every named project must belong to tenant, so the token cannot be widened to
// a foreign project.
func (p Projects) IssueScopedToken(ctx context.Context, tenant domain.TenantID, projects []domain.ProjectID) (string, error) {
	projects = uniqueProjects(projects)
	for _, project := range projects {
		if err := p.requireProject(ctx, tenant, project); err != nil {
			return "", err
		}
	}
	return p.Issuer.IssueScopedToken(ctx, tenant, projects)
}

// RotateProjectToken issues a replacement project token and revokes the
// project's existing active token(s). The old hashes are snapshotted before the
// new token is minted, so the replacement is never the one revoked.
func (p Projects) RotateProjectToken(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) (string, error) {
	if err := p.requireProject(ctx, tenant, project); err != nil {
		return "", err
	}
	old, err := p.activeProjectTokens(ctx, tenant, project)
	if err != nil {
		return "", err
	}
	raw, err := p.Issuer.IssueProjectToken(ctx, tenant, project)
	if err != nil {
		return "", err
	}
	for _, hash := range old {
		if err := p.Issuer.RevokeCredential(ctx, tenant, hash); err != nil {
			return "", err
		}
	}
	return raw, nil
}

// ListTokens returns tenant's token metadata, newest first. Only hashes are
// stored, so a raw secret can never be returned here.
func (p Projects) ListTokens(ctx context.Context, tenant domain.TenantID) ([]Credential, error) {
	return p.Credentials.ByTenant(ctx, tenant)
}

// RevokeToken revokes the token with hash under tenant. A hash belonging to
// another tenant reports ErrCredentialNotFound.
func (p Projects) RevokeToken(ctx context.Context, tenant domain.TenantID, hash string) error {
	if hash == "" {
		return ErrCredentialNotFound
	}
	return p.Issuer.RevokeCredential(ctx, tenant, hash)
}

func (p Projects) requireProject(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) error {
	if _, err := domain.NewPrincipal(tenant, project); err != nil {
		return err
	}
	_, ok, err := p.Store.Project(ctx, tenant, project)
	if err != nil {
		return err
	}
	if !ok {
		return ErrProjectNotFound
	}
	return nil
}

func (p Projects) activeProjectTokens(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) ([]string, error) {
	creds, err := p.Credentials.ByTenant(ctx, tenant)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range creds {
		if c.Kind == domain.TokenProject && c.Allows(project) && !c.Revoked() {
			out = append(out, c.Hash)
		}
	}
	return out, nil
}

// uniqueProjects drops blanks and duplicate slugs so a token scope has no
// repeated or empty entries.
func uniqueProjects(projects []domain.ProjectID) []domain.ProjectID {
	seen := make(map[domain.ProjectID]bool, len(projects))
	out := make([]domain.ProjectID, 0, len(projects))
	for _, project := range projects {
		if project == "" || seen[project] {
			continue
		}
		seen[project] = true
		out = append(out, project)
	}
	return out
}
