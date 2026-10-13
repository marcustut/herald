// Package sink implements the delivery destinations: email over SMTP and
// outbound webhooks, plus the config-driven factory and directory.
package sink

import (
	"context"
	"fmt"
	"log/slog"

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

func (s *Noop) Deliver(_ context.Context, r domain.Report) error {
	if s.Logger != nil {
		s.Logger.Info("noop sink delivered", "sink", s.sinkID, "report", r.ID)
	}
	return nil
}

// Directory resolves a project's enabled sinks from configuration.
type Directory struct {
	projects map[domain.ProjectID][]domain.SinkID
}

func NewDirectory(cfg config.Gateway) *Directory {
	projects := make(map[domain.ProjectID][]domain.SinkID, len(cfg.Projects))
	for slug, p := range cfg.Projects {
		ids := make([]domain.SinkID, len(p.Sinks))
		for i, s := range p.Sinks {
			ids[i] = domain.SinkID(s)
		}
		projects[domain.ProjectID(slug)] = ids
	}
	return &Directory{projects: projects}
}

func (d *Directory) EnabledSinks(_ context.Context, _ domain.TenantID, project domain.ProjectID) ([]domain.SinkID, error) {
	ids := d.projects[project]
	return append([]domain.SinkID(nil), ids...), nil
}

var _ app.SinkDirectory = (*Directory)(nil)

// Build constructs every configured sink. SMTP credentials are passed in from
// the environment, never from the config file.
func Build(cfg config.Gateway, smtpUser, smtpPass string) (map[domain.SinkID]app.Sink, error) {
	sinks := make(map[domain.SinkID]app.Sink, len(cfg.Sinks))
	for id, sc := range cfg.Sinks {
		s, err := build(id, sc, cfg.SMTP, smtpUser, smtpPass)
		if err != nil {
			return nil, err
		}
		sinks[domain.SinkID(id)] = s
	}
	return sinks, nil
}

func build(id string, sc config.Sink, smtpCfg config.SMTP, user, pass string) (app.Sink, error) {
	switch sc.Kind {
	case "email":
		return NewEmail(domain.SinkID(id), EmailConfig{
			Addr:      smtpCfg.Addr,
			From:      smtpCfg.From,
			Recipient: sc.Recipient,
			Username:  user,
			Password:  pass,
		}), nil
	case "webhook":
		return NewWebhook(domain.SinkID(id), sc.URL, nil), nil
	default:
		return nil, fmt.Errorf("unknown sink kind %q", sc.Kind)
	}
}
