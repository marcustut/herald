package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"html"
	"net/url"
	"strings"
	"time"

	"github.com/marcustut/herald/internal/domain"
)

// DefaultVerificationTTL bounds how long a destination-verification link may be
// redeemed. It is longer than a sign-in link because the recipient may not read
// the mailbox immediately.
const DefaultVerificationTTL = 24 * time.Hour

// Destinations is the hosted destination use case. A signed-in tenant lists,
// creates and deletes its own project's delivery destinations and proves
// ownership of an email target before it can receive reports.
//
// Every method takes the tenant and project from the caller's session; a
// destination's owner is never taken from a request body. The project must
// belong to the tenant, so a session for one tenant cannot address another's
// project even by guessing its slug.
type Destinations struct {
	Projects ProjectStore
	Store    DestinationStore
	Tokens   DestinationVerificationStore
	Mailer   Mailer
	Secrets  SecretGenerator
	Hasher   Hasher
	IDs      IDGenerator
	Clock    Clock
	// LinkBase is the public base under which the API is served, e.g.
	// https://cloud.example.com or https://example.com/api. The verify
	// path is appended to it.
	LinkBase string
	TokenTTL time.Duration
}

// List returns the tenant's project destinations, newest first.
func (d Destinations) List(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) ([]domain.Destination, error) {
	if err := d.requireProject(ctx, tenant, project); err != nil {
		return nil, err
	}
	return d.Store.DestinationsByProject(ctx, tenant, project)
}

// Create adds a destination to the tenant's own project. An email destination
// is created unverified and sent a verification link; a webhook is verified by
// configuration, so it can be delivered to at once.
func (d Destinations) Create(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, kind domain.DestinationKind, target string) (domain.Destination, error) {
	if err := d.requireProject(ctx, tenant, project); err != nil {
		return domain.Destination{}, err
	}
	now := d.Clock.Now()
	dst, err := domain.NewDestination(domain.Destination{
		ID:        d.IDs.NewDestinationID(),
		TenantID:  tenant,
		ProjectID: project,
		Kind:      kind,
		Target:    target,
		Enabled:   true,
		CreatedAt: now,
	})
	if err != nil {
		return domain.Destination{}, err
	}
	if dst.Kind == domain.DestinationWebhook {
		dst.VerifiedAt = now
	}
	if err := d.Store.PutDestination(ctx, dst); err != nil {
		return domain.Destination{}, err
	}
	if dst.Kind == domain.DestinationEmail {
		// The destination exists even if the mail fails; the owner can retry
		// verification, so surface the failure to the caller.
		if err := d.sendVerification(ctx, dst); err != nil {
			return domain.Destination{}, err
		}
	}
	return dst, nil
}

// Delete removes a destination from the tenant's own project.
func (d Destinations) Delete(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, id domain.DestinationID) error {
	if err := d.requireProject(ctx, tenant, project); err != nil {
		return err
	}
	return d.Store.DeleteDestination(ctx, tenant, project, id)
}

// RequestVerification mails a fresh verification link for an email destination
// the tenant owns. It is the retry path when the first link expired.
func (d Destinations) RequestVerification(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, id domain.DestinationID) error {
	if err := d.requireProject(ctx, tenant, project); err != nil {
		return err
	}
	dst, ok, err := d.Store.Destination(ctx, tenant, project, id)
	if err != nil {
		return err
	}
	if !ok {
		return ErrDestinationNotFound
	}
	if dst.Kind != domain.DestinationEmail {
		return ErrNotVerifiable
	}
	return d.sendVerification(ctx, dst)
}

// ConfirmVerification redeems a verification token and marks its destination
// verified. The owner comes from the stored token, never the request. A token
// that is unknown, expired or already used reports
// ErrDestinationVerificationNotFound.
func (d Destinations) ConfirmVerification(ctx context.Context, raw string) error {
	if !tokenShapeOK(raw) {
		return ErrDestinationVerificationNotFound
	}
	now := d.Clock.Now()
	hash := d.Hasher.Hash(raw)
	rec, err := d.Tokens.ConsumeDestinationVerification(ctx, hash, now)
	if errors.Is(err, ErrDestinationVerificationNotFound) {
		return ErrDestinationVerificationNotFound
	}
	if err != nil {
		return err
	}
	// Defence in depth: the store already matched by hash, but re-check in
	// constant time so a secret is never compared with a variable-time ==.
	if subtle.ConstantTimeCompare([]byte(rec.Hash), []byte(hash)) != 1 {
		return ErrDestinationVerificationNotFound
	}
	return d.Store.MarkVerified(ctx, rec.TenantID, rec.ProjectID, rec.DestinationID, now)
}

func (d Destinations) requireProject(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) error {
	if project == "" {
		return ErrProjectNotFound
	}
	_, ok, err := d.Projects.Project(ctx, tenant, project)
	if err != nil {
		return err
	}
	if !ok {
		return ErrProjectNotFound
	}
	return nil
}

func (d Destinations) sendVerification(ctx context.Context, dst domain.Destination) error {
	now := d.Clock.Now()
	raw := d.Secrets.NewSecret()
	if err := d.Tokens.PutDestinationVerification(ctx, DestinationVerificationRecord{
		Hash:          d.Hasher.Hash(raw),
		DestinationID: dst.ID,
		TenantID:      dst.TenantID,
		ProjectID:     dst.ProjectID,
		Email:         dst.Target,
		CreatedAt:     now,
		ExpiresAt:     now.Add(d.tokenTTL()),
	}); err != nil {
		return err
	}
	link := d.verifyLink(raw)
	return d.Mailer.SendMail(ctx, MailMessage{
		To:      dst.Target,
		Subject: "Confirm your Herald notification address",
		Text:    verifyText(link, dst.ProjectID),
		HTML:    verifyHTML(link, dst.ProjectID),
	})
}

func (d Destinations) tokenTTL() time.Duration {
	if d.TokenTTL <= 0 {
		return DefaultVerificationTTL
	}
	return d.TokenTTL
}

func (d Destinations) verifyLink(raw string) string {
	return strings.TrimRight(d.LinkBase, "/") + "/v1/destinations/verify?token=" + url.QueryEscape(raw)
}

func verifyText(link string, project domain.ProjectID) string {
	return fmt.Sprintf("Confirm that this address should receive reports for the %q project by opening this link:\n\n%s\n\nIt expires shortly and can be used once. If you did not ask for this, ignore this email.\n", project, link)
}

func verifyHTML(link string, project domain.ProjectID) string {
	escaped := html.EscapeString(link)
	return fmt.Sprintf(
		`<p>Confirm that this address should receive reports for the <strong>%s</strong> project:</p><p><a href="%s">Confirm address</a></p>`+
			`<p>It expires shortly and can be used once. If you did not ask for this, ignore this email.</p>`,
		html.EscapeString(string(project)), escaped)
}
