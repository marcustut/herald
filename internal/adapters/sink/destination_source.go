package sink

import (
	"context"
	"fmt"
	"net/http"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/config"
	"github.com/marcustut/herald/internal/domain"
)

// DestinationSource is a hosted app.SinkSource. It resolves a delivery's sink
// from the persisted destination row on every call, so a destination that was
// removed, disabled or left unverified after the event was queued is not
// delivered to. The lookup is scoped by the stored event's (tenant, project),
// never by the delivery row or any request input.
type DestinationSource struct {
	destinations app.DestinationStore
	smtp         config.SMTP
	security     config.Security
	smtpUser     string
	smtpPass     string
	webhook      *http.Client
	transport    MailTransport
	// suppressions, when set, refuses email to a suppressed address. It is nil
	// for a self-host deployment, where suppression (a hosted deliverability
	// safeguard) does not apply.
	suppressions app.SuppressionStore
}

// NewDestinationSource builds the source. SMTP credentials come from the
// environment; webhook delivery shares the same hardened client as a
// config-built webhook, so the SSRF policy is identical for hosted targets.
func NewDestinationSource(destinations app.DestinationStore, cfg config.Gateway, smtpUser, smtpPass string) *DestinationSource {
	return &DestinationSource{
		destinations: destinations,
		smtp:         cfg.SMTP,
		security:     cfg.Security,
		smtpUser:     smtpUser,
		smtpPass:     smtpPass,
		webhook:      NewHTTPClient(WebhookTimeout, cfg.Security.AllowPrivateNetworks),
	}
}

// WithMailTransport overrides the mail transport (tests).
func (s *DestinationSource) WithMailTransport(t MailTransport) *DestinationSource {
	s.transport = t
	return s
}

// WithSuppressions attaches the platform-wide suppression list, so a hosted
// email delivery to a suppressed address is refused at delivery time. A nil
// store leaves the check off (the self-host path).
func (s *DestinationSource) WithSuppressions(store app.SuppressionStore) *DestinationSource {
	s.suppressions = store
	return s
}

func (s *DestinationSource) Sink(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, id domain.SinkID) (app.Sink, bool, error) {
	d, ok, err := s.destinations.Destination(ctx, tenant, project, domain.DestinationID(id))
	if err != nil || !ok {
		return nil, false, err
	}
	// Re-check at delivery time: enabled and verified are live state, not a
	// snapshot taken when the event was queued.
	if !d.Enabled || !d.Verified() {
		return nil, false, nil
	}
	// A webhook destination must also pass the operator's optional host
	// allowlist (the destination row is live state, so the policy is applied
	// here, not at config load).
	if d.Kind == domain.DestinationWebhook && !s.security.WebhookHostAllowed(d.Target) {
		return nil, false, nil
	}
	built, err := buildDestination(d, s.smtp, s.smtpUser, s.smtpPass, s.webhook)
	if err != nil {
		return nil, false, fmt.Errorf("build sink for destination %s: %w", d.ID, err)
	}
	// The email sink is the report delivery path; reuse it unchanged. Tests can
	// substitute the transport to observe a delivery without SMTP.
	if e, ok := built.(*Email); ok {
		if s.transport != nil {
			e.WithTransport(s.transport)
		}
		if s.suppressions != nil {
			return &suppressionGuard{inner: e, address: d.Target, suppressions: s.suppressions}, true, nil
		}
	}
	return built, true, nil
}

var _ app.SinkSource = (*DestinationSource)(nil)
