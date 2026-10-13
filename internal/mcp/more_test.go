package mcp

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestCallToolInvalidArguments(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_event","arguments":"not-an-object"}}` + "\n")
	var out bytes.Buffer
	s := &Server{Client: &fakeSubmitter{}, In: in, Out: &out}
	if err := s.Serve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "invalid arguments") {
		t.Fatalf("got %q", out.String())
	}
}

func TestCallToolBadParams(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"nope"}` + "\n")
	var out bytes.Buffer
	s := &Server{Client: &fakeSubmitter{}, In: in, Out: &out}
	if err := s.Serve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "invalid tool arguments") {
		t.Fatalf("got %q", out.String())
	}
}
