package sink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// Webhook POSTs a JSON representation of the event to a destination URL.
type Webhook struct {
	sinkID domain.SinkID
	url    string
	client *http.Client
}

func NewWebhook(id domain.SinkID, url string, client *http.Client) *Webhook {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Webhook{sinkID: id, url: url, client: client}
}

func (w *Webhook) ID() domain.SinkID { return w.sinkID }

type webhookPayload struct {
	ID             string            `json:"id"`
	Status         string            `json:"status"`
	Project        string            `json:"project"`
	Agent          string            `json:"agent"`
	Host           string            `json:"host,omitempty"`
	Title          string            `json:"title"`
	Body           string            `json:"body,omitempty"`
	Links          []webhookLink     `json:"links,omitempty"`
	Context        map[string]string `json:"context,omitempty"`
	IdempotencyKey string            `json:"idempotencyKey"`
	CreatedAt      string            `json:"createdAt"`
}

type webhookLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

func (w *Webhook) Deliver(ctx context.Context, r domain.Event) error {
	payload := webhookPayload{
		ID:             string(r.ID),
		Status:         r.Status.String(),
		Project:        string(r.ProjectID),
		Agent:          r.AgentID.String(),
		Host:           r.Context.Host(),
		Title:          r.Title,
		Body:           r.Body,
		Context:        r.Context,
		IdempotencyKey: string(r.IdempotencyKey),
		CreatedAt:      r.CreatedAt.UTC().Format(time.RFC3339),
	}
	for _, l := range r.Links {
		payload.Links = append(payload.Links, webhookLink{Label: l.Label, URL: l.URL})
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return app.Permanent(fmt.Errorf("encode webhook payload: %w", err))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return app.Permanent(fmt.Errorf("build webhook request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", string(r.IdempotencyKey))

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("webhook status %d", resp.StatusCode)
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return app.Permanent(fmt.Errorf("webhook status %d", resp.StatusCode))
	default:
		return fmt.Errorf("webhook status %d", resp.StatusCode)
	}
}

var _ app.Sink = (*Webhook)(nil)
