package sink

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/config"
)

func TestBlockedIP(t *testing.T) {
	tests := []struct {
		ip      string
		blocked bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		{"169.254.169.254", true}, // cloud metadata
		{"100.100.100.200", true}, // Alibaba Cloud metadata (100.64/10)
		{"fe80::1", true},
		{"fc00::1", true},
		{"0.0.0.0", true},
		{"224.0.0.1", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"2606:4700:4700::1111", false},
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			if got := blockedIP(net.ParseIP(tt.ip)); got != tt.blocked {
				t.Fatalf("blockedIP(%s) = %v, want %v", tt.ip, got, tt.blocked)
			}
		})
	}
}

// TestWebhookRefusesPrivateTarget proves the dial-time guard: a loopback
// destination (the SSRF shape) is refused and never reached.
func TestWebhookRefusesPrivateTarget(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// NewWebhook with a nil client uses the hardened default (no private nets).
	w := NewWebhook("hook", srv.URL, nil)
	err := w.Deliver(context.Background(), sampleEvent())
	if err == nil || !app.IsPermanent(err) {
		t.Fatalf("err = %v, want a permanent refusal", err)
	}
	if hits.Load() != 0 {
		t.Fatal("private target was contacted")
	}
}

// TestWebhookAllowsPrivateWhenConfigured covers the opt-in for trusted internal
// receivers.
func TestWebhookAllowsPrivateWhenConfigured(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	w := NewWebhook("hook", srv.URL, NewHTTPClient(time.Second, true))
	if err := w.Deliver(context.Background(), sampleEvent()); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}
}

// TestWebhookDoesNotFollowRedirect proves the redirect policy: a 3xx is reported
// to the caller as permanent and the target is never contacted.
func TestWebhookDoesNotFollowRedirect(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	w := NewWebhook("hook", redirector.URL, NewHTTPClient(time.Second, true))
	err := w.Deliver(context.Background(), sampleEvent())
	if err == nil || !app.IsPermanent(err) {
		t.Fatalf("err = %v, want a permanent redirect refusal", err)
	}
	if targetHits.Load() != 0 {
		t.Fatal("redirect target was followed")
	}
}

func TestWebhookMaliciousURLs(t *testing.T) {
	urls := []string{
		"file:///etc/passwd",
		"gopher://evil.example/",
		"javascript:alert(1)",
		"ftp://evil.example/x",
		"//evil.example/x",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/",
		"http://10.0.0.1/hook",
		"://bad url",
	}
	for _, raw := range urls {
		t.Run(raw, func(t *testing.T) {
			w := NewWebhook("hook", raw, nil)
			err := w.Deliver(context.Background(), sampleEvent())
			if err == nil || !app.IsPermanent(err) {
				t.Fatalf("Deliver(%q) = %v, want a permanent error", raw, err)
			}
		})
	}
}

// TestBuildWiresPrivateNetworkPolicy is the end-to-end check that the [security]
// setting controls whether a private webhook can be reached.
func TestBuildWiresPrivateNetworkPolicy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	mk := func(allow bool) app.Sink {
		cfg := config.Gateway{
			Security: config.Security{AllowPrivateNetworks: allow},
			Sinks:    map[string]config.Sink{"hook": {Kind: "webhook", URL: srv.URL}},
		}
		sinks, err := Build(cfg, "", "")
		if err != nil {
			t.Fatal(err)
		}
		return sinks["hook"]
	}

	if err := mk(true).Deliver(context.Background(), sampleEvent()); err != nil {
		t.Fatalf("allow_private_networks: %v", err)
	}
	if err := mk(false).Deliver(context.Background(), sampleEvent()); !app.IsPermanent(err) {
		t.Fatalf("default policy: err = %v, want a permanent refusal", err)
	}
}

// TestWebhookConcurrentDeliver exercises the shared hardened transport under
// concurrency.
func TestWebhookConcurrentDeliver(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	w := NewWebhook("hook", srv.URL, NewHTTPClient(time.Second, true))

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- w.Deliver(context.Background(), sampleEvent())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestEmailLoopbackPlaintextPolicy(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:587", true},
		{"[::1]:587", true},
		{"localhost:587", true},
		{"smtp.example.com:587", false},
		{"10.0.0.1:25", false},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			e := NewEmail("email", EmailConfig{Addr: tt.addr})
			tr, ok := e.transport.(smtpTransport)
			if !ok {
				t.Fatalf("transport = %T", e.transport)
			}
			if tr.allowPlaintext != tt.want {
				t.Fatalf("allowPlaintext = %v, want %v", tr.allowPlaintext, tt.want)
			}
		})
	}
}

// TestSMTPTransportRefusesPlaintextRemote drives the real transport against a
// server that does not offer STARTTLS; a non-loopback policy must refuse before
// sending anything.
func TestSMTPTransportRefusesPlaintextRemote(t *testing.T) {
	addr, fake := startFakeSMTP(t)
	tr := smtpTransport{} // allowPlaintext false, as for a remote host
	err := tr.Send(addr, nil, "herald@x", []string{"me@x"}, []byte("hello"))
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("err = %v, want a STARTTLS refusal", err)
	}
	if body := fake.Body(); body != "" {
		t.Fatalf("message reached the server: %q", body)
	}
}

// TestEmailSubjectHeaderInjection checks a CRLF in the title cannot add headers.
func TestEmailSubjectHeaderInjection(t *testing.T) {
	ev := sampleEvent()
	ev.Title = "deploy\r\nBcc: evil@example.com"
	s := NewEmail("email", EmailConfig{From: "h@x", Recipient: "m@x"})
	msg, err := s.buildMessage(ev)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := mail.ReadMessage(strings.NewReader(string(msg)))
	if err != nil {
		t.Fatalf("unparseable: %v", err)
	}
	if got := parsed.Header.Get("Bcc"); got != "" {
		t.Fatalf("injected Bcc header: %q", got)
	}
	if subject := parsed.Header.Get("Subject"); strings.ContainsAny(subject, "\r\n") {
		t.Fatalf("subject has a line break: %q", subject)
	}
}
