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
	smtpUser     string
	smtpPass     string
	webhook      *http.Client
	transport    MailTransport
}

// NewDestinationSource builds the source. SMTP credentials come from the
// environment; webhook delivery shares the same hardened client as a
// config-built webhook, so the SSRF policy is identical for hosted targets.
func NewDestinationSource(destinations app.DestinationStore, cfg config.Gateway, smtpUser, smtpPass string) *DestinationSource {
	return &DestinationSource{
		destinations: destinations,
		smtp:         cfg.SMTP,
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
	built, err := build(string(d.ID), destinationConfig(d), s.smtp, s.smtpUser, s.smtpPass, s.webhook)
	if err != nil {
		return nil, false, fmt.Errorf("build sink for destination %s: %w", d.ID, err)
	}
	// The email sink is the report delivery path; reuse it unchanged. Tests can
	// substitute the transport to observe a delivery without SMTP.
	if e, ok := built.(*Email); ok && s.transport != nil {
		e.WithTransport(s.transport)
	}
	return built, true, nil
}

// destinationConfig maps a stored destination to the config shape the sink
// factory builds from, so hosted and self-host sinks share one implementation.
func destinationConfig(d domain.Destination) config.Sink {
	if d.Kind == domain.DestinationWebhook {
		return config.Sink{Kind: "webhook", URL: d.Target}
	}
	return config.Sink{Kind: "email", Recipient: d.Target}
}

var _ app.SinkSource = (*DestinationSource)(nil)
