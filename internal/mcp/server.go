// Package mcp implements a minimal Model Context Protocol server over stdio,
// exposing a single send_event tool backed by the gateway client.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/marcustut/herald/internal/client"
)

const defaultProtocolVersion = "2024-11-05"

// Submitter is the gateway client the tool calls.
type Submitter interface {
	Submit(ctx context.Context, r client.Event) (string, error)
}

// Server serves MCP requests from In and writes responses to Out.
type Server struct {
	Project string
	Agent   string
	Host    string
	Client  Submitter
	In      io.Reader
	Out     io.Writer
}

func (s *Server) Serve(ctx context.Context) error {
	scanner := bufio.NewScanner(s.In)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		resp, ok := s.handle(ctx, req)
		if !ok {
			continue
		}
		if err := writeMessage(s.Out, resp); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (s *Server) handle(ctx context.Context, req request) (response, bool) {
	isNotification := len(req.ID) == 0
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		version := params.ProtocolVersion
		if version == "" {
			version = defaultProtocolVersion
		}
		return ok(req, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "herald", "version": "1.0.0"},
		}), !isNotification
	case "notifications/initialized", "notifications/cancelled":
		return response{}, false
	case "ping":
		return ok(req, map[string]any{}), !isNotification
	case "tools/list":
		return ok(req, map[string]any{"tools": []any{sendEventTool()}}), !isNotification
	case "tools/call":
		result := s.callTool(ctx, req.Params)
		return ok(req, result), !isNotification
	default:
		if isNotification {
			return response{}, false
		}
		return fail(req, -32601, "method not found: "+req.Method), true
	}
}

func (s *Server) callTool(ctx context.Context, raw json.RawMessage) any {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return toolError("invalid tool arguments")
	}
	if params.Name != "send_event" {
		return toolError("unknown tool: " + params.Name)
	}
	var args struct {
		Status         string        `json:"status"`
		Title          string        `json:"title"`
		Body           string        `json:"body"`
		Agent          string        `json:"agent"`
		Host           string        `json:"host"`
		Links          []client.Link `json:"links"`
		IdempotencyKey string        `json:"idempotencyKey"`
	}
	if err := json.Unmarshal(params.Arguments, &args); err != nil {
		return toolError("invalid arguments: " + err.Error())
	}
	agent := args.Agent
	if agent == "" {
		agent = s.Agent
	}
	host := args.Host
	if host == "" {
		host = s.Host
	}
	id, err := s.Client.Submit(ctx, client.Event{
		Project:        s.Project,
		Status:         args.Status,
		Title:          args.Title,
		Body:           args.Body,
		Agent:          agent,
		Host:           host,
		Links:          args.Links,
		IdempotencyKey: args.IdempotencyKey,
	})
	if err != nil {
		return toolError(err.Error())
	}
	return textResult("event accepted: " + id)
}

func sendEventTool() map[string]any {
	return map[string]any{
		"name":        "send_event",
		"description": "Send a finished-work event to the owner. Use when a task completes, fails, or is blocked.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"status":         map[string]any{"type": "string", "enum": []string{"success", "failure", "blocked"}},
				"title":          map[string]any{"type": "string", "description": "Short summary of the work"},
				"body":           map[string]any{"type": "string", "description": "Details; free text"},
				"agent":          map[string]any{"type": "string", "description": "Agent name (defaults to the configured agent)"},
				"host":           map[string]any{"type": "string", "description": "Machine the agent ran on"},
				"links":          map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"label": map[string]any{"type": "string"}, "url": map[string]any{"type": "string"}}, "required": []string{"label", "url"}}},
				"idempotencyKey": map[string]any{"type": "string", "description": "Stable key so a retry does not duplicate the event"},
			},
			"required": []string{"status", "title"},
		},
	}
}

func textResult(text string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
}

func toolError(text string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": true}
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func ok(req request, result any) response {
	return response{JSONRPC: "2.0", ID: req.ID, Result: result}
}

func fail(req request, code int, message string) response {
	return response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: code, Message: message}}
}

func writeMessage(w io.Writer, resp response) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("encode response: %w", err)
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}
