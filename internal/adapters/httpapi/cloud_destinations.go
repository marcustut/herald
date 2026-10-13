package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// Destination routes are registered only when the destination use case is
// wired, so a self-host deployment exposes none of them. Tenant and project
// scope always come from the session and the path; nothing in the request body
// can choose an owner.
func (c *Cloud) registerDestinations(mux *http.ServeMux) {
	if c.Destinations == nil {
		return
	}
	mux.HandleFunc("GET /v1/projects/{project}/destinations", c.handleListDestinations)
	mux.HandleFunc("POST /v1/projects/{project}/destinations", c.handleCreateDestination)
	mux.HandleFunc("DELETE /v1/projects/{project}/destinations/{id}", c.handleDeleteDestination)
	mux.HandleFunc("POST /v1/projects/{project}/destinations/{id}/verify", c.handleRequestVerification)
	mux.HandleFunc("GET /v1/destinations/verify", c.handleConfirmVerification)
}

type destinationPayload struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
}

type destinationDTO struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Target     string `json:"target"`
	Enabled    bool   `json:"enabled"`
	Verified   bool   `json:"verified"`
	VerifiedAt string `json:"verifiedAt,omitempty"`
	CreatedAt  string `json:"createdAt"`
}

func destinationDTOFrom(d domain.Destination) destinationDTO {
	dto := destinationDTO{
		ID:        string(d.ID),
		Kind:      string(d.Kind),
		Target:    d.Target,
		Enabled:   d.Enabled,
		Verified:  d.Verified(),
		CreatedAt: d.CreatedAt.UTC().Format(time.RFC3339),
	}
	if !d.VerifiedAt.IsZero() {
		dto.VerifiedAt = d.VerifiedAt.UTC().Format(time.RFC3339)
	}
	return dto
}

func (c *Cloud) handleListDestinations(w http.ResponseWriter, r *http.Request) {
	sess, ok := c.session(w, r)
	if !ok {
		return
	}
	list, err := c.Destinations.List(r.Context(), sess.Identity.TenantID, projectFrom(r))
	if err != nil {
		c.destinationError(w, err)
		return
	}
	out := make([]destinationDTO, len(list))
	for i, d := range list {
		out[i] = destinationDTOFrom(d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"destinations": out})
}

func (c *Cloud) handleCreateDestination(w http.ResponseWriter, r *http.Request) {
	sess, ok := c.session(w, r)
	if !ok {
		return
	}
	if !c.csrf(w, r, sess) {
		return
	}
	if !isJSON(r.Header.Get("Content-Type")) {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json", "")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(defaultMaxBody)))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "could not read request body", "")
		return
	}
	var payload destinationPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "malformed JSON", "")
		return
	}
	dst, err := c.Destinations.Create(r.Context(), sess.Identity.TenantID, projectFrom(r), domain.DestinationKind(payload.Kind), payload.Target)
	if err != nil {
		c.destinationError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"destination": destinationDTOFrom(dst)})
}

func (c *Cloud) handleDeleteDestination(w http.ResponseWriter, r *http.Request) {
	sess, ok := c.session(w, r)
	if !ok {
		return
	}
	if !c.csrf(w, r, sess) {
		return
	}
	err := c.Destinations.Delete(r.Context(), sess.Identity.TenantID, projectFrom(r), domain.DestinationID(r.PathValue("id")))
	if err != nil {
		c.destinationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRequestVerification mails a fresh verification link for an email
// destination. It is rate limited per client IP so an authenticated tenant
// cannot use it to mail-bomb an arbitrary address.
func (c *Cloud) handleRequestVerification(w http.ResponseWriter, r *http.Request) {
	sess, ok := c.session(w, r)
	if !ok {
		return
	}
	if !c.csrf(w, r, sess) {
		return
	}
	if !c.allow(w, r, "destination-verify:"+clientIP(r), "too many verification requests") {
		return
	}
	err := c.Destinations.RequestVerification(r.Context(), sess.Identity.TenantID, projectFrom(r), domain.DestinationID(r.PathValue("id")))
	if err != nil {
		c.destinationError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "sent"})
}

// handleConfirmVerification redeems the token in the emailed link. Like the
// magic-link callback it is a top-level navigation from a mailbox, so it is
// token-only: the recipient may not be the signed-in owner, and the token
// itself already names the owner to verify.
func (c *Cloud) handleConfirmVerification(w http.ResponseWriter, r *http.Request) {
	if err := c.Destinations.ConfirmVerification(r.Context(), r.URL.Query().Get("token")); err != nil {
		c.destinationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "verified"})
}

func projectFrom(r *http.Request) domain.ProjectID {
	return domain.ProjectID(r.PathValue("project"))
}

func (c *Cloud) destinationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, app.ErrProjectNotFound):
		writeError(w, http.StatusNotFound, "project_not_found", "no such project", "")
	case errors.Is(err, app.ErrDestinationNotFound):
		writeError(w, http.StatusNotFound, "destination_not_found", "no such destination", "")
	case errors.Is(err, app.ErrDestinationVerificationNotFound):
		writeError(w, http.StatusNotFound, "invalid_verification", "invalid or expired verification link", "")
	case errors.Is(err, app.ErrNotVerifiable):
		writeError(w, http.StatusUnprocessableEntity, "not_verifiable", "this destination kind cannot be verified by email", "kind")
	case errors.Is(err, domain.ErrInvalidDestinationKind):
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error(), "kind")
	case errors.Is(err, domain.ErrInvalidDestination):
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error(), "target")
	default:
		c.serverError(w, err)
	}
}
