package app

import (
	"context"
	"errors"
	"time"

	"github.com/marcustut/herald/internal/domain"
)

// lastUsedWindow throttles the best-effort last-used write: a token in active
// use is stamped at most once per window, so a busy project does not pay a
// database write on every submit.
const lastUsedWindow = 10 * time.Minute

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
	a.recordLastUsed(ctx, cred)
	return domain.NewPrincipal(cred.TenantID, project)
}

// recordLastUsed stamps the credential's last-used time when the stored value
// is older than the throttle window. It is best-effort telemetry: a failed
// write is ignored, so it can never fail an otherwise-valid submit.
func (a Authenticator) recordLastUsed(ctx context.Context, cred Credential) {
	now := time.Now()
	if now.Sub(cred.LastUsedAt) < lastUsedWindow {
		return
	}
	_ = a.Store.TouchLastUsed(ctx, cred.Hash, now)
}
