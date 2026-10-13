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
	key := fs.String("idempotency-key", "", "stable key (defaults to a hash of the report)")
	asJSON := fs.Bool("json", false, "read a JSON report from stdin")
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

	report := client.Report{
		Project: project,
		Status:  *status,
		Title:   *title,
		Body:    *body,
		Agent:   firstNonEmpty(*agent, defaultAgent),
		Host:    firstNonEmpty(*host, defaultHost),
		Links:   links,
	}
	if *asJSON {
		var payload sendPayload
		if err := json.NewDecoder(stdin).Decode(&payload); err != nil {
			fmt.Fprintln(stderr, "error: invalid JSON on stdin:", err)
			return 1
		}
		report.Project = firstNonEmpty(payload.Project, project)
		report.Status = payload.Status
		report.Title = payload.Title
		report.Body = payload.Body
		report.Agent = firstNonEmpty(payload.Agent, report.Agent)
		report.Links = payload.Links
		if h, ok := payload.Context["host"]; ok {
			report.Host = h
		}
		report.IdempotencyKey = payload.IdempotencyKey
	}
	if report.Agent == "" {
		report.Agent = "herald"
	}
	if report.IdempotencyKey == "" {
		report.IdempotencyKey = firstNonEmpty(*key, deriveKey(report.Status, report.Title, report.Body, report.Links))
	} else {
		report.IdempotencyKey = firstNonEmpty(*key, report.IdempotencyKey)
	}

	id, err := cl.Submit(ctx, report)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintln(stdout, id)
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
