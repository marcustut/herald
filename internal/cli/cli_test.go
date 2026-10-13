package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marcustut/herald/internal/version"
)

func run(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := Run(context.Background(), args, strings.NewReader(stdin), &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func TestUsageAndDispatch(t *testing.T) {
	if code, _, _ := run(t, "", ""); code == 0 {
		t.Fatal("no args should be a usage error")
	}
	if code, out, _ := run(t, "", "help"); code != 0 || !strings.Contains(out, "Usage:") {
		t.Fatalf("help: code=%d out=%q", code, out)
	}
	if code, out, _ := run(t, "", "version"); code != 0 || strings.TrimSpace(out) != version.String() {
		t.Fatalf("version: code=%d out=%q", code, out)
	}
	if code, _, _ := run(t, "", "bogus"); code == 0 {
		t.Fatal("unknown command should fail")
	}
}

// TestVersionCommandReflectsBuild checks the CLI prints the injected build
// version and stays usable when built without -ldflags (dev/empty).
func TestVersionCommandReflectsBuild(t *testing.T) {
	original := version.Version
	t.Cleanup(func() { version.Version = original })

	for _, tc := range []struct {
		name  string
		build string
		want  string
	}{
		{"injected release tag", "v1.2.3", "v1.2.3"},
		{"dev build", "dev", "dev"},
		{"empty build", "", "dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version.Version = tc.build
			code, out, errOut := run(t, "", "version")
			if code != 0 {
				t.Fatalf("code = %d, stderr = %q", code, errOut)
			}
			if got := strings.TrimSpace(out); got != tc.want {
				t.Fatalf("version output = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSend(t *testing.T) {
	var got struct {
		Project string `json:"project"`
		Status  string `json:"status"`
		Title   string `json:"title"`
		Agent   string `json:"agent"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"evt_x"}`))
	}))
	defer srv.Close()

	t.Setenv("HERALD_GATEWAY", srv.URL)
	t.Setenv("HERALD_TOKEN", "herald_axis_secret")
	t.Setenv("HERALD_PROJECT", "axis")

	code, out, errOut := run(t, "", "send", "--status", "success", "--title", "deploy")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	if strings.TrimSpace(out) != "evt_x" {
		t.Fatalf("out = %q", out)
	}
	if got.Project != "axis" || got.Status != "success" || got.Agent != "herald" {
		t.Fatalf("got %+v", got)
	}
}

func TestSendJSONFromStdin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"evt_json"}`))
	}))
	defer srv.Close()
	t.Setenv("HERALD_GATEWAY", srv.URL)
	t.Setenv("HERALD_TOKEN", "herald_axis_secret")

	payload := `{"status":"failure","title":"boom","agent":"codex","idempotencyKey":"k1"}`
	code, out, errOut := run(t, payload, "send", "--json")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	if strings.TrimSpace(out) != "evt_json" {
		t.Fatalf("out = %q", out)
	}
}

func TestSendGatewayError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthenticated"}}`))
	}))
	defer srv.Close()
	t.Setenv("HERALD_GATEWAY", srv.URL)
	t.Setenv("HERALD_TOKEN", "tok")

	if code, _, _ := run(t, "", "send", "--status", "success", "--title", "t"); code != 1 {
		t.Fatalf("code = %d", code)
	}
}

func TestSendMissingGateway(t *testing.T) {
	t.Setenv("HERALD_GATEWAY", "")
	t.Setenv("HERALD_PROJECT", "")
	if code, _, errOut := run(t, "", "send", "--status", "success", "--title", "t"); code != 1 || !strings.Contains(errOut, "gateway") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
}

func TestSendBadLink(t *testing.T) {
	t.Setenv("HERALD_GATEWAY", "http://example.invalid")
	if code, _, _ := run(t, "", "send", "--status", "success", "--title", "t", "--link", "nodelimiter"); code != 2 {
		t.Fatalf("code = %d", code)
	}
}

func TestMCP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"evt_mcp"}`))
	}))
	defer srv.Close()
	t.Setenv("HERALD_GATEWAY", srv.URL)
	t.Setenv("HERALD_TOKEN", "tok")

	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_event","arguments":{"status":"success","title":"done"}}}` + "\n"
	code, out, errOut := run(t, in, "mcp")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "evt_mcp") {
		t.Fatalf("out = %q", out)
	}
}

func TestServeBadConfig(t *testing.T) {
	if code, _, _ := run(t, "", "serve", "--config", "/nonexistent/gateway.toml"); code != 1 {
		t.Fatalf("code = %d", code)
	}
}

func TestDispatchRequiresDatabase(t *testing.T) {
	t.Setenv("HERALD_DATABASE_URL", "")
	if code, _, _ := run(t, "", "dispatch"); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}

func TestTokenRequiresDatabase(t *testing.T) {
	t.Setenv("HERALD_DATABASE_URL", "")
	if code, _, _ := run(t, "", "token", "create", "--project", "axis"); code != 1 {
		t.Fatal("expected failure without database url")
	}
	if code, _, _ := run(t, "", "token", "bogus"); code != 2 {
		t.Fatal("expected usage error")
	}
	if code, _, _ := run(t, "", "token"); code != 2 {
		t.Fatal("expected usage error")
	}
}

func TestSkillCommand(t *testing.T) {
	code, out, _ := run(t, "", "skill")
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out, "name: herald-notify") || !strings.Contains(out, "herald send") {
		t.Fatalf("skill output = %q", out)
	}
}

func TestSendOccurrenceID(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"evt_occ"}`))
	}))
	defer srv.Close()
	t.Setenv("HERALD_GATEWAY", srv.URL)
	t.Setenv("HERALD_TOKEN", "tok")
	t.Setenv("HERALD_PROJECT", "axis")

	code, out, errOut := run(t, "", "send", "--status", "success", "--title", "t", "--occurrence-id", "deploy-42")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	if strings.TrimSpace(out) != "evt_occ" {
		t.Fatalf("out = %q", out)
	}
	if got["occurrenceId"] != "deploy-42" {
		t.Fatalf("occurrenceId = %v", got["occurrenceId"])
	}
	// No content-derived key by default.
	if k, ok := got["idempotencyKey"]; ok && k != "" {
		t.Fatalf("unexpected default idempotencyKey = %v", k)
	}
}
