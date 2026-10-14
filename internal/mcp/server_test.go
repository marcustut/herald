package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/marcustut/herald/internal/client"
)

type fakeSubmitter struct {
	id  string
	err error
	got client.Event
}

func (f *fakeSubmitter) Submit(_ context.Context, r client.Event) (client.Accepted, error) {
	f.got = r
	return client.Accepted{ID: f.id}, f.err
}

func TestServe(t *testing.T) {
	in := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"send_event","arguments":{"status":"success","title":"done","occurrenceId":"occ-1","idempotencyKey":"k"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"bogus"}`,
	}, "\n") + "\n")

	var out bytes.Buffer
	fake := &fakeSubmitter{id: "evt_9"}
	s := &Server{Project: "axis", Agent: "herdr", Host: "h1", Client: fake, In: in, Out: &out}
	if err := s.Serve(context.Background()); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 responses, got %d: %s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], `"serverInfo"`) {
		t.Fatalf("initialize response = %q", lines[0])
	}
	if !strings.Contains(lines[1], `"send_event"`) {
		t.Fatalf("tools/list response = %q", lines[1])
	}
	if !strings.Contains(lines[2], "evt_9") {
		t.Fatalf("tools/call response = %q", lines[2])
	}
	if fake.got.Project != "axis" || fake.got.Agent != "herdr" || fake.got.Host != "h1" {
		t.Fatalf("submitter got %+v", fake.got)
	}
	if fake.got.OccurrenceID != "occ-1" || fake.got.IdempotencyKey != "k" {
		t.Fatalf("submitter got %+v", fake.got)
	}
}

func TestServeToolError(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_event","arguments":{"status":"success","title":"t"}}}` + "\n")
	var out bytes.Buffer
	s := &Server{Client: &fakeSubmitter{err: errors.New("gateway down")}, In: in, Out: &out}
	if err := s.Serve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"isError":true`) {
		t.Fatalf("expected tool error: %q", out.String())
	}
}

func TestServeUnknownTool(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope"}}` + "\n")
	var out bytes.Buffer
	s := &Server{Client: &fakeSubmitter{}, In: in, Out: &out}
	if err := s.Serve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "unknown tool") {
		t.Fatalf("got %q", out.String())
	}
}

func TestInitializeDefaultsProtocol(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n")
	var out bytes.Buffer
	s := &Server{Client: &fakeSubmitter{}, In: in, Out: &out}
	if err := s.Serve(context.Background()); err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Result.ProtocolVersion != defaultProtocolVersion {
		t.Fatalf("version = %q", resp.Result.ProtocolVersion)
	}
}

// TestInitializeNamesConfiguredGateway keeps the initialize result pointing at
// the cloud or self-hosted gateway the server actually uses.
func TestInitializeNamesConfiguredGateway(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n")
	var out bytes.Buffer
	s := &Server{Project: "axis", Gateway: "https://cloud.example.com", Client: &fakeSubmitter{}, In: in, Out: &out}
	if err := s.Serve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "https://cloud.example.com") {
		t.Fatalf("initialize did not name the gateway: %q", out.String())
	}
}

func TestServeIgnoresInvalidJSON(t *testing.T) {
	in := strings.NewReader("not json\n" + `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
	var out bytes.Buffer
	s := &Server{Client: &fakeSubmitter{}, In: in, Out: &out}
	if err := s.Serve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(strings.TrimSpace(out.String()), "\n") + 1; lines != 1 {
		t.Fatalf("expected 1 response, got %d: %q", lines, out.String())
	}
}

// TestSendEventToolPinsConsentPolicy keeps the advertised description aligned
// with docs/consent.md: opt-in / ask first, one event per terminal outcome, and
// no per-turn noise.
func TestSendEventToolPinsConsentPolicy(t *testing.T) {
	desc, _ := sendEventTool()["description"].(string)
	if desc == "" {
		t.Fatal("send_event tool has no description")
	}
	for _, want := range []string{
		"explicitly asked to be notified",
		"default is opt-in",
		"once per terminal outcome",
		"never on every turn",
		"unless the user explicitly asked for each checkpoint",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("send_event description missing %q:\n%s", want, desc)
		}
	}
}
