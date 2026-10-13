package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const validGateway = `
[server]
addr = "127.0.0.1:8787"

[smtp]
addr = "127.0.0.1:587"
from = "herald@example.com"
server_name = "mail.example.com"

[auth]
base_url = "https://cloud.example.com"
`

func TestLoadGatewayValid(t *testing.T) {
	g, err := LoadGateway(writeTemp(t, validGateway))
	if err != nil {
		t.Fatal(err)
	}
	if g.Auth.BaseURL != "https://cloud.example.com" {
		t.Fatalf("base_url = %q", g.Auth.BaseURL)
	}
	// Signup defaults to true when the field is omitted.
	if !g.Auth.Signup {
		t.Fatal("signup should default to true")
	}
	if g.SMTP.ServerName != "mail.example.com" {
		t.Fatalf("server_name = %q", g.SMTP.ServerName)
	}
}

func TestLoadGatewaySignupExplicit(t *testing.T) {
	g, err := LoadGateway(writeTemp(t, validGateway+"signup = false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if g.Auth.Signup {
		t.Fatal("signup = false not parsed")
	}
}

func TestLoadGatewayInvalid(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"missing addr", "[server]\n[smtp]\naddr=\"a\"\nfrom=\"f\"\n[auth]\nbase_url=\"https://x\""},
		{"missing base_url", "[server]\naddr=\"x\"\n[smtp]\naddr=\"a\"\nfrom=\"f\""},
		{"relative base_url", "[server]\naddr=\"x\"\n[smtp]\naddr=\"a\"\nfrom=\"f\"\n[auth]\nbase_url=\"/api\""},
		{"non-http base_url", "[server]\naddr=\"x\"\n[smtp]\naddr=\"a\"\nfrom=\"f\"\n[auth]\nbase_url=\"ftp://x\""},
		{"missing smtp addr", "[server]\naddr=\"x\"\n[smtp]\nfrom=\"f\"\n[auth]\nbase_url=\"https://x\""},
		{"missing smtp from", "[server]\naddr=\"x\"\n[smtp]\naddr=\"a\"\n[auth]\nbase_url=\"https://x\""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := LoadGateway(writeTemp(t, tt.body)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestLoadGatewayBadSyntax(t *testing.T) {
	if _, err := LoadGateway(writeTemp(t, "not = [valid")); err == nil {
		t.Fatal("expected decode error")
	}
}

// The legacy routing tables are gone: a config that still names them must not
// silently parse into some other meaning. The loader leaves them unread, so the
// document stays parseable but exposes no project/sink surface.
func TestGatewayHasNoProjectOrSinkSurface(t *testing.T) {
	legacy := validGateway + `
[projects.axis]
sinks = ["email"]

[sinks.email]
kind = "email"
recipient = "a@b"
`
	g, err := LoadGateway(writeTemp(t, legacy))
	if err != nil {
		t.Fatalf("legacy tables should be ignored, got %v", err)
	}
	// The struct has no such fields; a parsed gateway carries only server/smtp/
	// security/limits/auth. We assert the fields the rest of the code relies on.
	if g.Server.Addr == "" || g.Auth.BaseURL == "" {
		t.Fatalf("gateway not parsed: %+v", g)
	}
}

func TestLoadGatewayFromName(t *testing.T) {
	tests := []struct {
		name string
		smtp string
		want string
	}{
		{"omitted defaults to Herald", `addr = "127.0.0.1:587"
from = "herald@example.com"`, "Herald"},
		{"explicit value is kept", `addr = "127.0.0.1:587"
from = "herald@example.com"
from_name = "Build Bot"`, "Build Bot"},
		{"explicit empty keeps bare address", `addr = "127.0.0.1:587"
from = "herald@example.com"
from_name = ""`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := "[server]\naddr=\"x\"\n[smtp]\n" + tt.smtp + "\n[auth]\nbase_url=\"https://x\"\n"
			g, err := LoadGateway(writeTemp(t, body))
			if err != nil {
				t.Fatal(err)
			}
			if g.SMTP.FromName != tt.want {
				t.Fatalf("from_name = %q, want %q", g.SMTP.FromName, tt.want)
			}
		})
	}
}

func TestLimitsDefaults(t *testing.T) {
	g, err := LoadGateway(writeTemp(t, validGateway))
	if err != nil {
		t.Fatal(err)
	}
	got := g.Limits
	if got.Requests != DefaultRequests || got.Window != DefaultWindow || got.Daily != DefaultDaily || got.Monthly != DefaultMonthly {
		t.Fatalf("limits = %+v", got)
	}
	if got.Queued != 0 {
		t.Fatalf("queued default = %d, want 0", got.Queued)
	}
	appLimits := g.Limits.AppLimits()
	if appLimits.Window != time.Minute || appLimits.Requests != DefaultRequests {
		t.Fatalf("app limits = %+v", appLimits)
	}
}

func TestLimitsGlobal(t *testing.T) {
	body := validGateway + `
[limits]
requests = 10
window = "30s"
daily = 500
monthly = 9000
queued = 100
`
	g, err := LoadGateway(writeTemp(t, body))
	if err != nil {
		t.Fatal(err)
	}
	got := g.Limits.AppLimits()
	if got.Requests != 10 || got.Window != 30*time.Second || got.Daily != 500 || got.Monthly != 9000 || got.Queued != 100 {
		t.Fatalf("limits = %+v", got)
	}
}

func TestLimitsInvalid(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"bad window", "[limits]\nwindow=\"nope\""},
		{"zero window", "[limits]\nwindow=\"0s\""},
		{"negative requests", "[limits]\nrequests=-1"},
		{"negative queued", "[limits]\nqueued=-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := "[server]\naddr=\"x\"\n[smtp]\naddr=\"a\"\nfrom=\"f\"\n[auth]\nbase_url=\"https://x\"\n" + tt.body
			if _, err := LoadGateway(writeTemp(t, body)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestSecurityAllowPrivateNetworks(t *testing.T) {
	g, err := LoadGateway(writeTemp(t, validGateway+"\n[security]\nallow_private_networks = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !g.Security.AllowPrivateNetworks {
		t.Fatal("allow_private_networks not parsed")
	}
	if len(g.Security.WebhookAllowHosts) != 0 {
		t.Fatalf("unexpected allowlist: %v", g.Security.WebhookAllowHosts)
	}
}

func TestSecurityWebhookAllowlistValidation(t *testing.T) {
	if _, err := LoadGateway(writeTemp(t, validGateway+"\n[security]\nwebhook_allow_hosts=[\"\"]\n")); err == nil {
		t.Fatal("empty allowlist entry should fail")
	}
	if _, err := LoadGateway(writeTemp(t, validGateway+"\n[security]\nwebhook_allow_hosts=[\"hooks.example.com\"]\n")); err != nil {
		t.Fatalf("valid allowlist rejected: %v", err)
	}
}

func TestClient(t *testing.T) {
	c, err := LoadClient(writeTemp(t, "project = \"axis\"\ngateway = \"https://h.example\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Project != "axis" || c.Gateway != "https://h.example" {
		t.Fatalf("got %+v", c)
	}

	if _, err := LoadClient(writeTemp(t, "project = \"axis\"\n")); err == nil {
		t.Fatal("expected missing gateway error")
	}
	if _, err := LoadClient(writeTemp(t, "gateway = \"x\"\n")); err == nil {
		t.Fatal("expected missing project error")
	}
	if _, err := LoadClient(writeTemp(t, "=bad")); err == nil {
		t.Fatal("expected decode error")
	}
}

// The auth surface requires a public base URL and a usable SMTP path. Bounce
// webhook is a plain boolean here; the secret env is checked at startup.
func TestAuthGate(t *testing.T) {
	base := "[server]\naddr=\"x\"\n[smtp]\naddr=\"127.0.0.1:587\"\nfrom=\"herald@example.com\"\n"

	t.Run("prefix path base_url is allowed", func(t *testing.T) {
		g, err := LoadGateway(writeTemp(t, base+"[auth]\nbase_url=\"https://example.com/api\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if g.Auth.BaseURL != "https://example.com/api" {
			t.Fatalf("base_url = %q", g.Auth.BaseURL)
		}
	})

	t.Run("bounce webhook parses", func(t *testing.T) {
		g, err := LoadGateway(writeTemp(t, base+"[auth]\nbase_url=\"https://cloud.example.com\"\nbounce_webhook=true\n"))
		if err != nil {
			t.Fatal(err)
		}
		if !g.Auth.BounceWebhook {
			t.Fatal("bounce_webhook not threaded")
		}
	})

	for _, tt := range []struct {
		name string
		body string
	}{
		{"missing base_url", base + "[auth]\n"},
		{"relative base_url", base + "[auth]\nbase_url=\"/cloud\"\n"},
		{"missing smtp", "[server]\naddr=\"x\"\n[auth]\nbase_url=\"https://cloud.example.com\"\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := LoadGateway(writeTemp(t, tt.body)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
