package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/marcustut/herald/internal/client"
	"github.com/marcustut/herald/internal/credentials"
)

// cloudTimeout bounds a single hosted sign-in API call.
const cloudTimeout = 30 * time.Second

// runLogin stores a bearer token where every shell and agent session can find
// it, and records the gateway and project. The token comes from --token or
// HERALD_TOKEN (noninteractive), from the hosted magic-link flow (--email), or
// from a prompt (self-host). The token itself is never printed.
func runLogin(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlagSet("login", stderr)
	configPath := fs.String("config", "herald.toml", "path to herald.toml")
	gateway := fs.String("gateway", "", "gateway base URL (defaults to HERALD_GATEWAY or herald.toml)")
	project := fs.String("project", "", "project slug (defaults to HERALD_PROJECT, herald.toml or the git repo)")
	token := fs.String("token", "", "store this token (noninteractive; defaults to HERALD_TOKEN)")
	email := fs.String("email", "", "sign in with a cloud magic link sent to this address")
	runTest := fs.Bool("test", false, "send a test event after storing the token")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	gw, slug, err := loginConfig(ctx, *configPath, *gateway, *project)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if gw == "" {
		fmt.Fprintln(stderr, "error: gateway URL is required: pass --gateway URL or set HERALD_GATEWAY")
		return 1
	}

	var tok string
	switch {
	case *token != "":
		tok = *token
	case *email != "":
		tok, err = cloudLogin(ctx, gw, slug, *email, stdin, stdout)
	case os.Getenv("HERALD_TOKEN") != "":
		tok = os.Getenv("HERALD_TOKEN")
	default:
		tok, err = promptToken(stdin, stdout, gw)
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	path, err := credentials.Save(tok)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "stored token in %s (0600)\n", path)

	if slug != "" {
		if err := writeClientConfig(*configPath, slug, gw); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "wrote %s (project %q → %s)\n", *configPath, slug, gw)
	}

	if *runTest {
		return loginTest(ctx, gw, slug, tok, stdout, stderr)
	}
	return 0
}

// loginConfig resolves the gateway and project, preferring an explicit flag,
// then the environment, then herald.toml, then the git repository name.
func loginConfig(ctx context.Context, configPath, gateway, project string) (gw, slug string, err error) {
	gw, slug, err = clientConfig(configPath)
	if err != nil {
		return "", "", err
	}
	if gateway != "" {
		gw = gateway
	}
	if project != "" {
		slug = project
	}
	gw = strings.TrimRight(gw, "/")
	if slug == "" {
		if wd, err := os.Getwd(); err == nil {
			slug = detectProject(ctx, wd)
		}
	}
	return gw, slug, nil
}

// cloudLogin runs the hosted sign-in path: it requests a magic link, redeems
// the link the user pastes back, ensures the project exists, and mints a
// project token scoped to just that project.
func cloudLogin(ctx context.Context, gateway, project, email string, stdin io.Reader, stdout io.Writer) (string, error) {
	if project == "" {
		return "", errors.New("--project is required to sign in to the cloud (it mints a token for one project)")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return "", err
	}
	hc := &http.Client{Jar: jar, Timeout: cloudTimeout}
	if err := requestMagicLink(ctx, hc, gateway, email); err != nil {
		return "", err
	}
	fmt.Fprintf(stdout, "sent a sign-in link to %s\n", email)
	fmt.Fprintln(stdout, "copy the link from the email and paste it here (the CLI redeems it; do not open it in a browser):")
	line, err := readLine(stdin)
	if err != nil || strings.TrimSpace(line) == "" {
		return "", errors.New("no sign-in link provided")
	}
	csrf, err := redeemMagicLink(ctx, hc, gateway, line)
	if err != nil {
		return "", err
	}
	if err := ensureProject(ctx, hc, gateway, csrf, project); err != nil {
		return "", err
	}
	return mintProjectToken(ctx, hc, gateway, csrf, project)
}

func requestMagicLink(ctx context.Context, hc *http.Client, gateway, email string) error {
	err := cloudCall(ctx, hc, http.MethodPost, gateway+"/v1/auth/magic-link", "",
		map[string]string{"email": email}, nil, http.StatusAccepted)
	if err != nil {
		return fmt.Errorf("request sign-in link: %w", err)
	}
	return nil
}

func redeemMagicLink(ctx context.Context, hc *http.Client, gateway, line string) (string, error) {
	raw := magicToken(line)
	if raw == "" {
		return "", errors.New("that does not look like a sign-in link or token")
	}
	var out struct {
		CSRF string `json:"csrfToken"`
	}
	err := cloudCall(ctx, hc, http.MethodGet,
		gateway+"/v1/auth/callback?token="+url.QueryEscape(raw), "", nil, &out, http.StatusOK)
	if err != nil {
		return "", fmt.Errorf("redeem sign-in link: %w", err)
	}
	if out.CSRF == "" {
		return "", errors.New("gateway returned no CSRF token")
	}
	return out.CSRF, nil
}

// ensureProject confirms the signed-in tenant owns project, creating it when
// missing so a fresh account can onboard without an operator.
func ensureProject(ctx context.Context, hc *http.Client, gateway, csrf, project string) error {
	var list struct {
		Projects []struct {
			ID string `json:"id"`
		} `json:"projects"`
	}
	if err := cloudCall(ctx, hc, http.MethodGet, gateway+"/v1/projects", "", nil, &list, http.StatusOK); err != nil {
		return fmt.Errorf("list projects: %w", err)
	}
	for _, p := range list.Projects {
		if p.ID == project {
			return nil
		}
	}
	err := cloudCall(ctx, hc, http.MethodPost, gateway+"/v1/projects", csrf,
		map[string]string{"id": project, "name": project}, nil, http.StatusCreated)
	if err != nil {
		return fmt.Errorf("create project: %w", err)
	}
	return nil
}

func mintProjectToken(ctx context.Context, hc *http.Client, gateway, csrf, project string) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	err := cloudCall(ctx, hc, http.MethodPost,
		gateway+"/v1/projects/"+url.PathEscape(project)+"/token", csrf, nil, &out, http.StatusCreated)
	if err != nil {
		return "", fmt.Errorf("mint project token: %w", err)
	}
	if out.Token == "" {
		return "", errors.New("gateway returned no token")
	}
	return out.Token, nil
}

// magicToken accepts the emailed sign-in link or the bare token and returns the
// token. A value that does not parse as a URL is treated as the token itself.
func magicToken(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	if u, err := url.Parse(line); err == nil && u.Scheme != "" {
		return u.Query().Get("token")
	}
	return line
}

// promptToken is the self-host path: the operator issues a token and the user
// pastes it. It reads stdin, so a noninteractive caller must pass --token or
// HERALD_TOKEN instead.
func promptToken(stdin io.Reader, stdout io.Writer, gateway string) (string, error) {
	fmt.Fprintf(stdout, "Paste a project token for %s\n", gateway)
	fmt.Fprintf(stdout, "  cloud: create one at %s/dashboard, or re-run with --email to sign in\n", gateway)
	fmt.Fprintln(stdout, "  self-host: ask the gateway operator, or run `herald token create --project <name>` on the gateway host")
	fmt.Fprint(stdout, "token: ")
	line, err := readLine(stdin)
	if err != nil {
		return "", errors.New("no token provided: pass --token or set HERALD_TOKEN for noninteractive use")
	}
	tok := strings.TrimSpace(line)
	if tok == "" {
		return "", errors.New("no token provided: pass --token or set HERALD_TOKEN for noninteractive use")
	}
	return tok, nil
}

// writeClientConfig records the gateway and project in herald.toml (no secret).
func writeClientConfig(path, project, gateway string) error {
	return os.WriteFile(path, []byte(fmt.Sprintf(initConfigTemplate, project, gateway)), 0o644)
}

// loginTest submits one event to verify the stored credential end to end.
func loginTest(ctx context.Context, gateway, project, token string, stdout, stderr io.Writer) int {
	cl := client.Client{Gateway: gateway, Token: token}
	id, err := cl.Submit(ctx, client.Event{
		Project: project,
		Status:  "success",
		Title:   "herald login test",
		Body:    "Test event from `herald login --test`.",
		Agent:   "herald",
	})
	if err != nil {
		fmt.Fprintln(stderr, "error: test event failed:", err)
		return 1
	}
	fmt.Fprintf(stdout, "test event accepted: %s\n", id)
	return 0
}

// cloudCall issues one JSON request to the hosted API and checks the status. A
// nil body sends no payload; a non-empty csrf adds the synchronizer header on
// mutations. A non-2xx response becomes an error carrying the gateway's code;
// a 2xx body is unmarshalled into out when out is non-nil.
func cloudCall(ctx context.Context, hc *http.Client, method, rawURL, csrf string, body, out any, want int) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		// Mirrors httpapi.CSRFHeader.
		req.Header.Set("X-CSRF-Token", csrf)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("request %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != want {
		var errBody struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &errBody)
		code := errBody.Error.Code
		if code == "" {
			code = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("gateway %d (%s): %s", resp.StatusCode, code, errBody.Error.Message)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// readLine reads one line, tolerating a final line without a trailing newline.
func readLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if line == "" && err != nil {
		return "", io.EOF
	}
	return strings.TrimRight(line, "\r\n"), nil
}
