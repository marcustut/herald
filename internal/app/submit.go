package app

import (
	"context"
	"errors"

	"github.com/marcustut/herald/internal/domain"
)

// LinkInput is a raw link from a submit request.
type LinkInput struct {
	Label string
	URL   string
}

// SubmitRequest is the transport-agnostic shape of POST /v1/reports. Agent and
// Context are asserted by the trusted client; Tenant and Project come from the
// principal, never the request.
type SubmitRequest struct {
	Status         string
	Title          string
	Body           string
	Links          []LinkInput
	Context        map[string]string
	Agent          string
	IdempotencyKey string
}

// SubmitReport accepts a report, deduplicates it by idempotency key, redacts
// untrusted text, and fans it out to the project's sinks as delivery rows.
type SubmitReport struct {
	Reports ReportStore
	Sinks   SinkDirectory
	IDs     IDGenerator
	Clock   Clock
}

// Handle returns the report id. A replay of the same (tenant, idempotency key)
// returns the original id without creating a second report or re-fanning out.
func (uc SubmitReport) Handle(ctx context.Context, p domain.Principal, req SubmitRequest) (domain.ReportID, error) {
	status, err := domain.ParseStatus(req.Status)
	if err != nil {
		return "", err
	}
	agent, err := domain.ParseAgentID(req.Agent)
	if err != nil {
		return "", err
	}
	key, err := domain.ParseIdempotencyKey(req.IdempotencyKey)
	if err != nil {
		return "", err
	}
	links, err := parseLinks(req.Links)
	if err != nil {
		return "", err
	}
	labels, err := domain.NewContext(req.Context)
	if err != nil {
		return "", err
	}

	if existing, ok, err := uc.Reports.ByIdempotencyKey(ctx, p.TenantID, key); err != nil {
		return "", err
	} else if ok {
		return existing.ID, nil
	}

	report, err := domain.NewReport(domain.NewReportInput{
		ID:             uc.IDs.NewReportID(),
		TenantID:       p.TenantID,
		ProjectID:      p.ProjectID,
		AgentID:        agent,
		Status:         status,
		Title:          req.Title,
		Body:           req.Body,
		Links:          links,
		Context:        labels,
		IdempotencyKey: key,
		CreatedAt:      uc.Clock.Now(),
	})
	if err != nil {
		return "", err
	}
	// Redact before persistence so the gateway never becomes a secret store.
	report = report.Redact()

	sinks, err := uc.Sinks.EnabledSinks(ctx, p.TenantID, p.ProjectID)
	if err != nil {
		return "", err
	}
	deliveries := make([]domain.Delivery, len(sinks))
	for i, s := range sinks {
		deliveries[i] = domain.NewDelivery(uc.IDs.NewDeliveryID(), report.ID, s, report.CreatedAt)
	}

	if err := uc.Reports.Save(ctx, report, deliveries); err != nil {
		// A concurrent request may have won the idempotency race.
		if errors.Is(err, ErrIdempotencyConflict) {
			existing, ok, lookupErr := uc.Reports.ByIdempotencyKey(ctx, p.TenantID, key)
			if lookupErr != nil {
				return "", lookupErr
			}
			if ok {
				return existing.ID, nil
			}
		}
		return "", err
	}
	return report.ID, nil
}

func parseLinks(in []LinkInput) ([]domain.Link, error) {
	if len(in) == 0 {
		return nil, nil
	}
	links := make([]domain.Link, len(in))
	for i, l := range in {
		link, err := domain.NewLink(l.Label, l.URL)
		if err != nil {
			return nil, err
		}
		links[i] = link
	}
	return links, nil
}
