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

// SubmitRequest is the transport-agnostic shape of POST /v1/events. Agent and
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

// SubmitEvent accepts a event, deduplicates it by idempotency key, redacts
// untrusted text, and fans it out to the project's sinks as delivery rows.
type SubmitEvent struct {
	Events EventStore
	Sinks  SinkDirectory
	IDs    IDGenerator
	Clock  Clock
}

// Handle returns the event id. A replay of the same (tenant, idempotency key)
// returns the original id without creating a second event or re-fanning out.
func (uc SubmitEvent) Handle(ctx context.Context, p domain.Principal, req SubmitRequest) (domain.EventID, error) {
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

	if existing, ok, err := uc.Events.ByIdempotencyKey(ctx, p.TenantID, key); err != nil {
		return "", err
	} else if ok {
		return existing.ID, nil
	}

	event, err := domain.NewEvent(domain.NewEventInput{
		ID:             uc.IDs.NewEventID(),
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
	event = event.Redact()

	sinks, err := uc.Sinks.EnabledSinks(ctx, p.TenantID, p.ProjectID)
	if err != nil {
		return "", err
	}
	deliveries := make([]domain.Delivery, len(sinks))
	for i, s := range sinks {
		deliveries[i] = domain.NewDelivery(uc.IDs.NewDeliveryID(), event.ID, s, event.CreatedAt)
	}

	if err := uc.Events.Save(ctx, event, deliveries); err != nil {
		// A concurrent request may have won the idempotency race.
		if errors.Is(err, ErrIdempotencyConflict) {
			existing, ok, lookupErr := uc.Events.ByIdempotencyKey(ctx, p.TenantID, key)
			if lookupErr != nil {
				return "", lookupErr
			}
			if ok {
				return existing.ID, nil
			}
		}
		return "", err
	}
	return event.ID, nil
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
