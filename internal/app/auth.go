package app

import (
	"context"
	"errors"

	"github.com/marcustut/herald/internal/domain"
)

// Authenticator resolves bearer tokens to principals.
type Authenticator struct {
	Store  CredentialStore
	Hasher Hasher
}

// Authenticate resolves bearer to a principal acting for requestedProject.
//
//   - A project token fixes the project; requestedProject, if supplied, must
//     agree with it.
//   - A scoped token requires requestedProject and authorizes it against the
//     credential's projects.
//
// Every credential failure returns ErrUnauthenticated. A store failure other
// than "not found" is returned as-is so the caller can answer 503, not 401.
func (a Authenticator) Authenticate(ctx context.Context, bearer string, requestedProject domain.ProjectID) (domain.Principal, error) {
	tok, err := domain.ParseToken(bearer)
	if err != nil {
		return domain.Principal{}, ErrUnauthenticated
	}
	cred, err := a.Store.ByHash(ctx, a.Hasher.Hash(tok.Raw()))
	switch {
	case errors.Is(err, ErrCredentialNotFound):
		return domain.Principal{}, ErrUnauthenticated
	case err != nil:
		return domain.Principal{}, err
	}
	if cred.Revoked() {
		return domain.Principal{}, ErrUnauthenticated
	}
	project := requestedProject
	if tok.Kind() == domain.TokenProject {
		if project != "" && project != tok.Project() {
			return domain.Principal{}, ErrUnauthenticated
		}
		project = tok.Project()
	}
	if !cred.Allows(project) {
		return domain.Principal{}, ErrUnauthenticated
	}
	return domain.NewPrincipal(cred.TenantID, project)
}
