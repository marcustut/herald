package sink

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
)

func TestSinkIDs(t *testing.T) {
	if got := NewEmail("email", EmailConfig{}).ID(); got != "email" {
		t.Fatalf("email id = %q", got)
	}
	if got := NewWebhook("hook", "https://x", nil).ID(); got != "hook" {
		t.Fatalf("webhook id = %q", got)
	}
}

func TestNoopLogs(t *testing.T) {
	var buf bytes.Buffer
	n := NewNoop("noop")
	n.Logger = slog.New(slog.NewTextHandler(&buf, nil))
	if err := n.Deliver(context.Background(), sampleEvent()); err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 {
		t.Fatal("expected a log line")
	}
}
