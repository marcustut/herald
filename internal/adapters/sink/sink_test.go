package sink

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/smtp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/config"
	"github.com/marcustut/herald/internal/domain"
)

func sampleEvent() domain.Event {
	return domain.Event{
		ID:             "evt_1",
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

	if err := s.Deliver(context.Background(), sampleEvent()); err != nil {
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
	msg, err := s.buildMessage(sampleEvent())
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
	rep := sampleEvent()
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
	err := s.Deliver(context.Background(), sampleEvent())
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
	if err := w.Deliver(context.Background(), sampleEvent()); err != nil {
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
			err := w.Deliver(context.Background(), sampleEvent())
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
	if err := w.Deliver(context.Background(), sampleEvent()); !app.IsPermanent(err) {
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
	if err := n.Deliver(context.Background(), sampleEvent()); err != nil {
		t.Fatal(err)
	}
}

func TestEmailWithAuth(t *testing.T) {
	// A username triggers PlainAuth construction; exercise that branch.
	s := NewEmail("email", EmailConfig{Addr: "smtp.x:587", From: "h@x", Recipient: "m@x", Username: "u", Password: "p"}).WithTransport(&fakeTransport{})
	if err := s.Deliver(context.Background(), sampleEvent()); err != nil {
		t.Fatal(err)
	}
}

// fakeSMTP speaks just enough SMTP (no STARTTLS, AUTH PLAIN) to exercise the
// real smtpTransport end to end.
type fakeSMTP struct {
	mu   sync.Mutex
	body string
	to   []string
}

func (f *fakeSMTP) Body() string { f.mu.Lock(); defer f.mu.Unlock(); return f.body }
func (f *fakeSMTP) To() []string { f.mu.Lock(); defer f.mu.Unlock(); return f.to }

func startFakeSMTP(t *testing.T) (string, *fakeSMTP) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	fake := &fakeSMTP{}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		r := bufio.NewReader(conn)
		say := func(s string) { _, _ = fmt.Fprintf(conn, "%s\r\n", s) }
		say("220 test ESMTP")
		inData := false
		var data strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if inData {
				if line == "." {
					inData = false
					fake.mu.Lock()
					fake.body = data.String()
					fake.mu.Unlock()
					say("250 ok")
					continue
				}
				data.WriteString(line + "\n")
				continue
			}
			up := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(up, "EHLO"):
				say("250-test")
				say("250 AUTH PLAIN")
			case strings.HasPrefix(up, "HELO"):
				say("250 test")
			case strings.HasPrefix(up, "AUTH"):
				say("235 ok")
			case strings.HasPrefix(up, "MAIL FROM"):
				say("250 ok")
			case strings.HasPrefix(up, "RCPT TO"):
				fake.mu.Lock()
				fake.to = append(fake.to, line)
				fake.mu.Unlock()
				say("250 ok")
			case up == "DATA":
				say("354 go")
				inData = true
			case up == "QUIT":
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()
	return ln.Addr().String(), fake
}

func TestSMTPTransportDelivers(t *testing.T) {
	addr, fake := startFakeSMTP(t)
	s := NewEmail("email", EmailConfig{
		Addr: addr, From: "herald@x", Recipient: "me@x", Username: "herald", Password: "secret",
	})
	if err := s.Deliver(context.Background(), sampleEvent()); err != nil {
		t.Fatal(err)
	}
	body := fake.Body()
	if !strings.Contains(body, "deploy finished") || !strings.Contains(body, "Subject:") {
		t.Fatalf("server did not receive the message: %q", body)
	}
	if to := fake.To(); len(to) != 1 || !strings.Contains(to[0], "me@x") {
		t.Fatalf("rcpt = %v", to)
	}
}

func TestSMTPTransportDialFailure(t *testing.T) {
	// A closed listener gives a connection error (retryable).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	s := NewEmail("email", EmailConfig{Addr: addr, From: "h@x", Recipient: "m@x"})
	if err := s.Deliver(context.Background(), sampleEvent()); err == nil || app.IsPermanent(err) {
		t.Fatalf("err = %v", err)
	}
}
