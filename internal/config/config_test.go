package config

import (
	"os"
	"path/filepath"
	"testing"
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
