package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcustut/herald/internal/credentials"
)

// panicReader fails the test if any code tries to prompt on stdin. It proves a
// "noninteractive" login path never reads from stdin.
type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("unexpected read from stdin") }

func runWithStdin(t *testing.T, stdin io.Reader, args ...string) (int, string, string) {
	t.Helper()
	var out, errBuf strings.Builder
	code := Run(context.Background(), args, stdin, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func TestLoginNoninteractiveToken(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HERALD_CONFIG_DIR", t.TempDir())
	t.Setenv("HERALD_TOKEN", "")

	code, out, errOut := runWithStdin(t, panicReader{},
		"login", "--gateway", "https://gw.example.com/", "--project", "axis", "--token", "herald_axis_secret")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	if strings.Contains(out, "herald_axis_secret") {
		t.Fatalf("login printed the token: %q", out)
	}
	if strings.Contains(out, "Paste") {
		t.Fatalf("noninteractive login prompted: %q", out)
	}

	data, err := os.ReadFile(filepath.Join(dir, "herald.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `project = "axis"`) ||
		!strings.Contains(string(data), `gateway = "https://gw.example.com"`) {
		t.Fatalf("herald.toml = %s", data)
	}

	path, err := credentials.Path()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("credential mode = %o, want 0600", perm)
	}
	if got, _ := credentials.Token(); got != "herald_axis_secret" {
		t.Fatalf("stored token = %q", got)
	}
}

func TestLoginEnvTokenNoninteractive(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("HERALD_CONFIG_DIR", t.TempDir())
	t.Setenv("HERALD_TOKEN", "herald_axis_env")

	code, _, errOut := runWithStdin(t, panicReader{},
		"login", "--gateway", "https://gw.example.com", "--project", "axis")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	if got, _ := credentials.Token(); got != "herald_axis_env" {
		t.Fatalf("stored token = %q", got)
	}
}

func TestLoginInteractivePaste(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HERALD_CONFIG_DIR", t.TempDir())
	t.Setenv("HERALD_TOKEN", "")

	code, out, errOut := run(t, "herald_axis_pasted\n",
		"login", "--gateway", "https://gw.example.com", "--project", "axis")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "Paste a project token") {
		t.Fatalf("expected a prompt: %q", out)
	}
	if got, _ := credentials.Token(); got != "herald_axis_pasted" {
		t.Fatalf("stored token = %q", got)
	}
}

func TestLoginInteractiveEmptyTokenFails(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("HERALD_CONFIG_DIR", t.TempDir())
	t.Setenv("HERALD_TOKEN", "")

	code, _, errOut := run(t, "", "login", "--gateway", "https://gw.example.com", "--project", "axis")
	if code != 1 || !strings.Contains(errOut, "HERALD_TOKEN") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
}

func TestLoginRequiresGateway(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("HERALD_CONFIG_DIR", t.TempDir())
	t.Setenv("HERALD_GATEWAY", "")
	t.Setenv("HERALD_TOKEN", "")

	code, _, errOut := run(t, "", "login", "--project", "axis", "--token", "t")
	if code != 1 || !strings.Contains(errOut, "gateway URL is required") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
}

// TestLoginTestFlag verifies --test submits one event with the stored token.
func TestLoginTestFlag(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"evt_login"}`))
	}))
	defer srv.Close()

	t.Chdir(t.TempDir())
	t.Setenv("HERALD_CONFIG_DIR", t.TempDir())
	t.Setenv("HERALD_TOKEN", "")

	code, out, errOut := run(t, "",
		"login", "--gateway", srv.URL, "--project", "axis", "--token", "herald_axis_secret", "--test")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	if gotPath != "/v1/events" {
		t.Fatalf("test event path = %q", gotPath)
	}
	if !strings.Contains(out, "test event accepted: evt_login") {
		t.Fatalf("out = %q", out)
	}
}

func TestLoginTestFlagReportsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthenticated"}}`))
	}))
	defer srv.Close()

	t.Chdir(t.TempDir())
	t.Setenv("HERALD_CONFIG_DIR", t.TempDir())
	t.Setenv("HERALD_TOKEN", "")

	code, _, errOut := run(t, "",
		"login", "--gateway", srv.URL, "--project", "axis", "--token", "bad", "--test")
	if code != 1 || !strings.Contains(errOut, "test event failed") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
}

// TestLoginCloudMagicLink drives the whole hosted flow against a fake gateway:
// request a link, redeem it, create the project and mint a project token.
func TestLoginCloudMagicLink(t *testing.T) {
	var magicEmail, callbackToken, createdID, createdWithCSRF, mintCSRF string

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/magic-link", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Email string `json:"email"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		magicEmail = body.Email
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"sent"}`))
	})
	mux.HandleFunc("GET /v1/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		callbackToken = r.URL.Query().Get("token")
		http.SetCookie(w, &http.Cookie{Name: "herald_session", Value: "sess", Path: "/"})
		_, _ = w.Write([]byte(`{"csrfToken":"csrf-xyz","user":{"email":"me@example.com"}}`))
	})
	mux.HandleFunc("GET /v1/projects", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"projects":[{"id":"other"}]}`))
	})
	mux.HandleFunc("POST /v1/projects", func(w http.ResponseWriter, r *http.Request) {
		createdWithCSRF = r.Header.Get("X-CSRF-Token")
		var body struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		createdID = body.ID
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"project":{"id":"axis"}}`))
	})
	mux.HandleFunc("POST /v1/projects/axis/token", func(w http.ResponseWriter, r *http.Request) {
		mintCSRF = r.Header.Get("X-CSRF-Token")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"herald_axis_cloud","kind":"project","projects":["axis"]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HERALD_CONFIG_DIR", t.TempDir())
	t.Setenv("HERALD_TOKEN", "")

	link := srv.URL + "/v1/auth/callback?token=link-token-123\n"
	code, out, errOut := run(t, link, "login", "--gateway", srv.URL, "--project", "axis", "--email", "me@example.com")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	if strings.Contains(out, "herald_axis_cloud") {
		t.Fatalf("login printed the minted token: %q", out)
	}
	if magicEmail != "me@example.com" {
		t.Fatalf("magic-link email = %q", magicEmail)
	}
	if callbackToken != "link-token-123" {
		t.Fatalf("callback token = %q", callbackToken)
	}
	if createdID != "axis" || createdWithCSRF != "csrf-xyz" {
		t.Fatalf("project create id=%q csrf=%q", createdID, createdWithCSRF)
	}
	if mintCSRF != "csrf-xyz" {
		t.Fatalf("mint csrf = %q", mintCSRF)
	}
	if got, _ := credentials.Token(); got != "herald_axis_cloud" {
		t.Fatalf("stored token = %q", got)
	}
}

func TestLoginCloudReusesExistingProject(t *testing.T) {
	created := false
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/magic-link", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /v1/auth/callback", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"csrfToken":"c"}`))
	})
	mux.HandleFunc("GET /v1/projects", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"projects":[{"id":"axis"}]}`))
	})
	mux.HandleFunc("POST /v1/projects", func(w http.ResponseWriter, _ *http.Request) {
		created = true
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("POST /v1/projects/axis/token", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"herald_axis_cloud"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Chdir(t.TempDir())
	t.Setenv("HERALD_CONFIG_DIR", t.TempDir())
	t.Setenv("HERALD_TOKEN", "")

	code, _, errOut := run(t, "link-token\n",
		"login", "--gateway", srv.URL, "--project", "axis", "--email", "me@example.com")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	if created {
		t.Fatal("login recreated an existing project")
	}
}

func TestLoginCloudErrorsSurface(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthenticated","message":"invalid or expired link"}}`))
	}))
	defer srv.Close()

	t.Chdir(t.TempDir())
	t.Setenv("HERALD_CONFIG_DIR", t.TempDir())
	t.Setenv("HERALD_TOKEN", "")

	code, _, errOut := run(t, "link-token\n",
		"login", "--gateway", srv.URL, "--project", "axis", "--email", "me@example.com")
	if code != 1 || !strings.Contains(errOut, "unauthenticated") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
}

// TestGatewayClientTokenPrecedence pins the documented order: process env >
// credential file. herald.toml never carries a token.
func TestGatewayClientTokenPrecedence(t *testing.T) {
	t.Setenv("HERALD_CONFIG_DIR", t.TempDir())
	t.Setenv("HERALD_GATEWAY", "https://gw.example.com")
	t.Setenv("HERALD_PROJECT", "axis")
	t.Setenv("HERALD_TOKEN", "")

	if _, _, _, _, err := gatewayClient(""); err == nil {
		t.Fatal("expected an error when no token is configured")
	}

	if _, err := credentials.Save("herald_axis_fromfile"); err != nil {
		t.Fatal(err)
	}
	cl, _, _, _, err := gatewayClient("")
	if err != nil {
		t.Fatal(err)
	}
	if cl.Token != "herald_axis_fromfile" {
		t.Fatalf("token = %q, want credential file value", cl.Token)
	}

	t.Setenv("HERALD_TOKEN", "herald_axis_fromenv")
	cl, _, _, _, err = gatewayClient("")
	if err != nil {
		t.Fatal(err)
	}
	if cl.Token != "herald_axis_fromenv" {
		t.Fatalf("token = %q, want env to win", cl.Token)
	}
}

// TestSkillReferencesConfiguredGateway checks the skill points at the gateway
// this installation uses instead of a hard-coded one.
func TestSkillReferencesConfiguredGateway(t *testing.T) {
	t.Setenv("HERALD_GATEWAY", "https://cloud.example.com")
	t.Setenv("HERALD_PROJECT", "axis")
	code, out, _ := run(t, "", "skill", "--config", "/nonexistent/herald.toml")
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out, "https://cloud.example.com") || !strings.Contains(out, "project axis") {
		t.Fatalf("skill did not name the gateway: %q", out)
	}

	t.Setenv("HERALD_GATEWAY", "")
	t.Setenv("HERALD_PROJECT", "")
	_, out, _ = run(t, "", "skill", "--config", "/nonexistent/herald.toml")
	if !strings.Contains(out, "No gateway is configured") {
		t.Fatalf("skill should note the missing gateway: %q", out)
	}
}
