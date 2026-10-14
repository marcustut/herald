package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/marcustut/herald/internal/client"
)

type linkFlags []client.Link

func (l *linkFlags) String() string { return "" }

func (l *linkFlags) Set(value string) error {
	label, url, ok := strings.Cut(value, "=")
	if !ok || label == "" || url == "" {
		return fmt.Errorf("link must be LABEL=URL, got %q", value)
	}
	*l = append(*l, client.Link{Label: label, URL: url})
	return nil
}

type sendPayload struct {
	Project        string            `json:"project"`
	Status         string            `json:"status"`
	Title          string            `json:"title"`
	Body           string            `json:"body"`
	Agent          string            `json:"agent"`
	Links          []client.Link     `json:"links"`
	Context        map[string]string `json:"context"`
	OccurrenceID   string            `json:"occurrenceId"`
	IdempotencyKey string            `json:"idempotencyKey"`
}

func runSend(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlagSet("send", stderr)
	configPath := fs.String("config", "herald.toml", "path to herald.toml")
	status := fs.String("status", "", "success, failure or blocked")
	title := fs.String("title", "", "short summary")
	body := fs.String("body", "", "details")
	agent := fs.String("agent", "", "agent name (defaults to HERALD_AGENT)")
	host := fs.String("host", "", "machine (defaults to HERALD_HOST)")
	occurrenceID := fs.String("occurrence-id", "", "stable id so a retry reuses the same event")
	key := fs.String("idempotency-key", "", "explicit retry key (alternative to --occurrence-id)")
	asJSON := fs.Bool("json", false, "read a JSON event from stdin")
	var links linkFlags
	fs.Var(&links, "link", "LABEL=URL (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cl, project, defaultAgent, defaultHost, err := gatewayClient(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	event := client.Event{
		Project:        project,
		Status:         *status,
		Title:          *title,
		Body:           *body,
		Agent:          firstNonEmpty(*agent, defaultAgent),
		Host:           firstNonEmpty(*host, defaultHost),
		Links:          links,
		OccurrenceID:   *occurrenceID,
		IdempotencyKey: *key,
	}
	if *asJSON {
		var payload sendPayload
		if err := json.NewDecoder(stdin).Decode(&payload); err != nil {
			fmt.Fprintln(stderr, "error: invalid JSON on stdin:", err)
			return 1
		}
		event.Project = firstNonEmpty(payload.Project, project)
		event.Status = payload.Status
		event.Title = payload.Title
		event.Body = payload.Body
		event.Agent = firstNonEmpty(payload.Agent, event.Agent)
		event.Links = payload.Links
		if h, ok := payload.Context["host"]; ok {
			event.Host = h
		}
		event.OccurrenceID = firstNonEmpty(*occurrenceID, payload.OccurrenceID)
		event.IdempotencyKey = firstNonEmpty(*key, payload.IdempotencyKey)
	}
	if event.Agent == "" {
		event.Agent = "herald"
	}

	res, err := cl.Submit(ctx, event)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintln(stdout, res.ID)
	if res.Deliveries == 0 {
		scope := "this project"
		if event.Project != "" {
			scope = fmt.Sprintf("project %q", event.Project)
		}
		fmt.Fprintf(stderr, "warning: event stored but %s has no verified destination — nothing will be delivered\n", scope)
	}
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
