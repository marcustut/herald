// Package sink implements the delivery destinations: email over SMTP and
// outbound webhooks.
package sink

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/config"
	"github.com/marcustut/herald/internal/domain"
)

// Noop is a sink that records nothing; useful for tests and dry runs.
type Noop struct {
	sinkID domain.SinkID
	Logger *slog.Logger
}

func NewNoop(id domain.SinkID) *Noop { return &Noop{sinkID: id} }

func (s *Noop) ID() domain.SinkID { return s.sinkID }

func (s *Noop) Deliver(_ context.Context, r domain.Event) error {
	if s.Logger != nil {
		s.Logger.Info("noop sink delivered", "sink", s.sinkID, "event", r.ID)
	}
	return nil
}

// buildDestination constructs a sink from a stored destination. SMTP
// credentials are passed in from the environment, never from the database.
// Webhook destinations share one hardened HTTP client built from the [security]
// settings. The only routing is a database destination, so hosted and self-host
// deployments share this one implementation.
func buildDestination(d domain.Destination, smtpCfg config.SMTP, user, pass string, webhookClient *http.Client) (app.Sink, error) {
	switch d.Kind {
	case domain.DestinationEmail:
		return NewEmail(domain.SinkID(d.ID), EmailConfig{
			Addr:          smtpCfg.Addr,
			From:          smtpCfg.From,
			FromName:      smtpCfg.FromName,
			Recipient:     d.Target,
			Username:      user,
			Password:      pass,
			TLSServerName: smtpCfg.ServerName,
		}), nil
	case domain.DestinationWebhook:
		return NewWebhook(domain.SinkID(d.ID), d.Target, webhookClient), nil
	default:
		return nil, fmt.Errorf("unknown destination kind %q", d.Kind)
	}
}
