// Package client is the HTTP client the CLI, MCP server and hooks use to talk
// to a gateway.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Client posts events to a gateway.
type Client struct {
	Gateway string
	Token   string
	HTTP    *http.Client
}

type Link struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// Event is the caller-facing shape of a event. Project is required for scoped
// tokens; for a project token it may be left empty.
type Event struct {
	Project        string
	Status         string
	Title          string
	Body           string
	Agent          string
	Host           string
	Links          []Link
	IdempotencyKey string
}

// HTTPError is a non-2xx gateway response.
type HTTPError struct {
	StatusCode int
	Code       string
	Message    string
	Field      string
}

func (e *HTTPError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("gateway %d (%s): %s", e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("gateway returned %d", e.StatusCode)
}

// Submit posts a event and returns its id.
func (c *Client) Submit(ctx context.Context, r Event) (string, error) {
	body := map[string]any{
		"status":         r.Status,
		"title":          r.Title,
		"body":           r.Body,
		"agent":          r.Agent,
		"idempotencyKey": r.IdempotencyKey,
	}
	if r.Project != "" {
		body["project"] = r.Project
	}
	if r.Host != "" {
		body["context"] = map[string]string{"host": r.Host}
	}
	if len(r.Links) > 0 {
		body["links"] = r.Links
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("encode event: %w", err)
	}

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.Gateway, "/")+"/v1/events", bytes.NewReader(encoded))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("submit event: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	if resp.StatusCode != http.StatusAccepted {
		return "", decodeError(resp.StatusCode, data)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if out.ID == "" {
		return "", fmt.Errorf("gateway returned no id")
	}
	return out.ID, nil
}

func decodeError(status int, data []byte) error {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Field   string `json:"field"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &body)
	return &HTTPError{StatusCode: status, Code: body.Error.Code, Message: body.Error.Message, Field: body.Error.Field}
}
