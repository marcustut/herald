package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/domain"
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
tenant = "owner"

[smtp]
addr = "127.0.0.1:587"
from = "herald@example.com"
server_name = "mail.example.com"

[projects.axis]
sinks = ["email", "slack"]

[projects.finance]
sinks = ["email"]

[sinks.email]
kind = "email"
recipient = "herald@example.com"

[sinks.slack]
kind = "webhook"
url = "https://hooks.example.com/x"
`

func TestLoadGatewayValid(t *testing.T) {
	g, err := LoadGateway(writeTemp(t, validGateway))
	if err != nil {
		t.Fatal(err)
	}
	if g.Server.Tenant != "owner" || len(g.Projects) != 2 {
		t.Fatalf("got %+v", g)
	}
	if g.SMTP.ServerName != "mail.example.com" {
		t.Fatalf("server_name = %q", g.SMTP.ServerName)
	}
}

func TestLoadGatewayInvalid(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"missing addr", "[server]\ntenant=\"x\"\n[projects.a]\nsinks=[\"e\"]\n[sinks.e]\nkind=\"email\"\nrecipient=\"a@b\""},
		{"missing tenant", "[server]\naddr=\"x\"\n[projects.a]\nsinks=[\"e\"]\n[sinks.e]\nkind=\"email\"\nrecipient=\"a@b\""},
		{"no projects", "[server]\naddr=\"x\"\ntenant=\"t\""},
		{"project no sinks", "[server]\naddr=\"x\"\ntenant=\"t\"\n[projects.a]\nsinks=[]"},
		{"unknown sink ref", "[server]\naddr=\"x\"\ntenant=\"t\"\n[projects.a]\nsinks=[\"nope\"]"},
		{"unknown kind", "[server]\naddr=\"x\"\ntenant=\"t\"\n[projects.a]\nsinks=[\"e\"]\n[sinks.e]\nkind=\"carrier-pigeon\""},
		{"email no recipient", "[server]\naddr=\"x\"\ntenant=\"t\"\n[projects.a]\nsinks=[\"e\"]\n[sinks.e]\nkind=\"email\""},
		{"webhook no url", "[server]\naddr=\"x\"\ntenant=\"t\"\n[projects.a]\nsinks=[\"w\"]\n[sinks.w]\nkind=\"webhook\""},
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
			body := "[server]\naddr=\"x\"\ntenant=\"t\"\n[smtp]\n" + tt.smtp +
				"\n[projects.a]\nsinks=[\"e\"]\n[sinks.e]\nkind=\"email\"\nrecipient=\"a@b\"\n"
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
	got := g.EffectiveLimits()[domain.ProjectID("axis")]
	want := struct {
		requests int
		window   time.Duration
		daily    int
		monthly  int
	}{DefaultRequests, time.Minute, DefaultDaily, DefaultMonthly}
	if got.Requests != want.requests || got.Window != want.window || got.Daily != want.daily || got.Monthly != want.monthly {
		t.Fatalf("effective limits = %+v", got)
	}
}

func TestLimitsGlobalAndProjectOverride(t *testing.T) {
	body := `
[server]
addr = "127.0.0.1:8787"
tenant = "owner"

[limits]
requests = 10
window = "30s"
daily = 500
monthly = 9000

[projects.axis]
sinks = ["email"]

[projects.axis.limits]
requests = 99
window = "2m"

[projects.finance]
sinks = ["email"]

[sinks.email]
kind = "email"
recipient = "herald@example.com"
`
	g, err := LoadGateway(writeTemp(t, body))
	if err != nil {
		t.Fatal(err)
	}
	limits := g.EffectiveLimits()

	axis := limits[domain.ProjectID("axis")]
	if axis.Requests != 99 || axis.Window != 2*time.Minute || axis.Daily != 500 || axis.Monthly != 9000 {
		t.Fatalf("axis = %+v", axis)
	}
	finance := limits[domain.ProjectID("finance")]
	if finance.Requests != 10 || finance.Window != 30*time.Second || finance.Daily != 500 || finance.Monthly != 9000 {
		t.Fatalf("finance = %+v", finance)
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
		{"project bad window", "[projects.a.limits]\nwindow=\"x\""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := "[server]\naddr=\"x\"\ntenant=\"t\"\n" + tt.body +
				"\n[projects.a]\nsinks=[\"e\"]\n[sinks.e]\nkind=\"email\"\nrecipient=\"a@b\"\n"
			if _, err := LoadGateway(writeTemp(t, body)); err == nil {
				t.Fatal("expected error")
			}
		})
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
