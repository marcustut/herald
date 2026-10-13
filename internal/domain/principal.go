package domain

import "fmt"

// Principal is the authenticated identity a credential resolves to for a
// request. The project is authoritative — derived from the credential's scope,
// never from the request body. Agent and host are asserted separately by the
// trusted client and are not part of the principal.
type Principal struct {
	TenantID  TenantID
	ProjectID ProjectID
}

func NewPrincipal(tenant TenantID, project ProjectID) (Principal, error) {
	if !validID(string(tenant)) {
		return Principal{}, fmt.Errorf("%w: tenant id", ErrInvalidID)
	}
	if !validSlug(string(project)) {
		return Principal{}, fmt.Errorf("%w: project id", ErrInvalidID)
	}
	return Principal{TenantID: tenant, ProjectID: project}, nil
}
