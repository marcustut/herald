package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	if code, out, _ := run(t, "", "version"); code != 0 || strings.TrimSpace(out) != version {
		t.Fatalf("version: code=%d out=%q", code, out)
	}
	if code, _, _ := run(t, "", "bogus"); code == 0 {
		t.Fatal("unknown command should fail")
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
		_, _ = w.Write([]byte(`{"id":"rep_x"}`))
	}))
	defer srv.Close()

	t.Setenv("HERALD_GATEWAY", srv.URL)
	t.Setenv("HERALD_TOKEN", "herald_axis_secret")
	t.Setenv("HERALD_PROJECT", "axis")

	code, out, errOut := run(t, "", "send", "--status", "success", "--title", "deploy")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	if strings.TrimSpace(out) != "rep_x" {
		t.Fatalf("out = %q", out)
	}
	if got.Project != "axis" || got.Status != "success" || got.Agent != "herald" {
		t.Fatalf("got %+v", got)
	}
}

func TestSendJSONFromStdin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"rep_json"}`))
	}))
	defer srv.Close()
	t.Setenv("HERALD_GATEWAY", srv.URL)

	payload := `{"status":"failure","title":"boom","agent":"codex","idempotencyKey":"k1"}`
	code, out, errOut := run(t, payload, "send", "--json")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	if strings.TrimSpace(out) != "rep_json" {
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
		_, _ = w.Write([]byte(`{"id":"rep_mcp"}`))
	}))
	defer srv.Close()
	t.Setenv("HERALD_GATEWAY", srv.URL)
	t.Setenv("HERALD_TOKEN", "tok")

	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_report","arguments":{"status":"success","title":"done"}}}` + "\n"
	code, out, errOut := run(t, in, "mcp")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "rep_mcp") {
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

func TestDeriveKeyIsStable(t *testing.T) {
	a := deriveKey("success", "t", "b", nil)
	b := deriveKey("success", "t", "b", nil)
	if a != b {
		t.Fatal("key should be deterministic")
	}
	if a == deriveKey("failure", "t", "b", nil) {
		t.Fatal("different content should differ")
	}
}
