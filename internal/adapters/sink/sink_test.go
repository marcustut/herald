package sink

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/smtp"
	"strings"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/config"
	"github.com/marcustut/herald/internal/domain"
)

func sampleReport() domain.Report {
	return domain.Report{
		ID:             "rep_1",
		TenantID:       "ten_1",
		ProjectID:      "axis",
		AgentID:        "herdr-build",
		Status:         domain.StatusSuccess,
		Title:          "deploy finished",
		Body:           "all green",
		Links:          []domain.Link{{Label: "PR", URL: "https://x.test/1"}},
		Context:        domain.Context{"host": "build-01"},
		IdempotencyKey: "run-1",
		CreatedAt:      time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC),
	}
}

type fakeTransport struct {
	addr string
	from string
	to   []string
	msg  []byte
	err  error
}

func (f *fakeTransport) Send(addr string, _ smtp.Auth, from string, to []string, msg []byte) error {
	f.addr, f.from, f.to, f.msg = addr, from, to, msg
	return f.err
}

func TestEmailDeliver(t *testing.T) {
	ft := &fakeTransport{}
	s := NewEmail("email", EmailConfig{Addr: "127.0.0.1:587", From: "herald@x", Recipient: "me@x"}).WithTransport(ft)

	if err := s.Deliver(context.Background(), sampleReport()); err != nil {
		t.Fatal(err)
	}
	if ft.addr != "127.0.0.1:587" || ft.from != "herald@x" {
		t.Fatalf("addr=%q from=%q", ft.addr, ft.from)
	}
	if len(ft.to) != 1 || ft.to[0] != "me@x" {
		t.Fatalf("to=%v", ft.to)
	}
	msg := string(ft.msg)
	if !strings.Contains(msg, "multipart/alternative") || !strings.Contains(msg, "text/html") {
		t.Fatalf("message not multipart: %q", msg)
	}
	if !strings.Contains(msg, "Idempotency-Key: run-1") {
		t.Fatalf("missing idempotency header")
	}
}

func TestEmailMessageIsParseable(t *testing.T) {
	s := NewEmail("email", EmailConfig{From: "herald@x", Recipient: "me@x"})
	msg, err := s.buildMessage(sampleReport())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := mail.ReadMessage(strings.NewReader(string(msg)))
	if err != nil {
		t.Fatalf("message unparseable: %v", err)
	}
	if parsed.Header.Get("From") != "herald@x" || parsed.Header.Get("To") != "me@x" {
		t.Fatalf("headers = %v", parsed.Header)
	}
	body, _ := io.ReadAll(parsed.Body)
	if !strings.Contains(string(body), "deploy finished") {
		t.Fatalf("body missing title: %q", body)
	}
}

func TestEmailBuildMessageEscapesHTML(t *testing.T) {
	rep := sampleReport()
	rep.Body = "<script>alert(1)</script>"
	s := NewEmail("email", EmailConfig{From: "h@x", Recipient: "m@x"})
	msg, err := s.buildMessage(rep)
	if err != nil {
		t.Fatal(err)
	}
	// The HTML part must escape; the text/plain part carries the literal text.
	if !strings.Contains(string(msg), "&lt;script&gt;") {
		t.Fatal("HTML part did not escape the script tag")
	}
}

func TestEmailTransportErrorIsRetryable(t *testing.T) {
	ft := &fakeTransport{err: errors.New("connection refused")}
	s := NewEmail("email", EmailConfig{From: "h@x", Recipient: "m@x"}).WithTransport(ft)
	err := s.Deliver(context.Background(), sampleReport())
	if err == nil || app.IsPermanent(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestHostOf(t *testing.T) {
	if got := hostOf("smtp.example.com:587"); got != "smtp.example.com" {
		t.Fatalf("got %q", got)
	}
	if got := hostOf("localhost"); got != "localhost" {
		t.Fatalf("got %q", got)
	}
}

func TestWebhookDeliver(t *testing.T) {
	var got webhookPayload
	var key string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key = r.Header.Get("Idempotency-Key")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	w := NewWebhook("slack", srv.URL, srv.Client())
	if err := w.Deliver(context.Background(), sampleReport()); err != nil {
		t.Fatal(err)
	}
	if got.Status != "success" || got.Agent != "herdr-build" || got.Host != "build-01" {
		t.Fatalf("payload = %+v", got)
	}
	if got.IdempotencyKey != "run-1" || key != "run-1" {
		t.Fatalf("idempotency key: body=%q header=%q", got.IdempotencyKey, key)
	}
}

func TestWebhookStatusClassification(t *testing.T) {
	tests := []struct {
		code      int
		permanent bool
	}{
		{http.StatusOK, false},
		{http.StatusInternalServerError, false},
		{http.StatusTooManyRequests, false},
		{http.StatusBadRequest, true},
		{http.StatusNotFound, true},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.code)
			}))
			defer srv.Close()
			w := NewWebhook("h", srv.URL, srv.Client())
			err := w.Deliver(context.Background(), sampleReport())
			if tt.code < 300 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || app.IsPermanent(err) != tt.permanent {
				t.Fatalf("code=%d permanent=%v err=%v", tt.code, tt.permanent, err)
			}
		})
	}
}

func TestWebhookBadURLIsPermanent(t *testing.T) {
	w := NewWebhook("h", "://bad url", nil)
	if err := w.Deliver(context.Background(), sampleReport()); !app.IsPermanent(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestDirectory(t *testing.T) {
	cfg := config.Gateway{
		Server:   config.Server{Addr: "x", Tenant: "t"},
		Projects: map[string]config.Project{"axis": {Sinks: []string{"email", "slack"}}},
		Sinks: map[string]config.Sink{
			"email": {Kind: "email", Recipient: "a@b"},
			"slack": {Kind: "webhook", URL: "https://x"},
		},
	}
	dir := NewDirectory(cfg)
	got, err := dir.EnabledSinks(context.Background(), "t", "axis")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "email" || got[1] != "slack" {
		t.Fatalf("got %v", got)
	}
	empty, _ := dir.EnabledSinks(context.Background(), "t", "unknown")
	if len(empty) != 0 {
		t.Fatalf("unknown project = %v", empty)
	}
}

func TestBuild(t *testing.T) {
	cfg := config.Gateway{
		SMTP: config.SMTP{Addr: "127.0.0.1:25", From: "h@x"},
		Sinks: map[string]config.Sink{
			"email": {Kind: "email", Recipient: "a@b"},
			"hook":  {Kind: "webhook", URL: "https://x"},
		},
	}
	sinks, err := Build(cfg, "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if len(sinks) != 2 {
		t.Fatalf("got %d sinks", len(sinks))
	}
	if _, ok := sinks["email"].(*Email); !ok {
		t.Fatalf("email sink = %T", sinks["email"])
	}
	if _, ok := sinks["hook"].(*Webhook); !ok {
		t.Fatalf("hook sink = %T", sinks["hook"])
	}
}

func TestBuildRejectsUnknownKind(t *testing.T) {
	cfg := config.Gateway{Sinks: map[string]config.Sink{"x": {Kind: "pigeon"}}}
	if _, err := Build(cfg, "", ""); err == nil {
		t.Fatal("expected error")
	}
}

func TestNoop(t *testing.T) {
	n := NewNoop("noop")
	if n.ID() != "noop" {
		t.Fatal("bad id")
	}
	if err := n.Deliver(context.Background(), sampleReport()); err != nil {
		t.Fatal(err)
	}
}

func TestEmailWithAuth(t *testing.T) {
	// A username triggers PlainAuth construction; exercise that branch.
	s := NewEmail("email", EmailConfig{Addr: "smtp.x:587", From: "h@x", Recipient: "m@x", Username: "u", Password: "p"}).WithTransport(&fakeTransport{})
	if err := s.Deliver(context.Background(), sampleReport()); err != nil {
		t.Fatal(err)
	}
}
