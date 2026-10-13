package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// initConfigTemplate is the secret-free herald.toml that herald init writes.
// The bearer token is read from HERALD_TOKEN at send time and never stored here.
const initConfigTemplate = `# Herald client configuration for this project (no secrets).
# Commit this file; the bearer token is read from HERALD_TOKEN when sending.
project = %q
gateway = %q
`

// runInit writes a safe per-project herald.toml, detecting the project name from
// the git repository when --project is not given.
func runInit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("init", stderr)
	configPath := fs.String("config", "herald.toml", "path to herald.toml")
	project := fs.String("project", "", "project slug (defaults to the git project name)")
	gateway := fs.String("gateway", os.Getenv("HERALD_GATEWAY"), "gateway base URL (defaults to HERALD_GATEWAY)")
	force := fs.Bool("force", false, "overwrite an existing herald.toml")
	noGuidance := fs.Bool("no-guidance", false, "skip the skill/MCP install guidance")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	slug := strings.TrimSpace(*project)
	if slug == "" {
		if wd, err := os.Getwd(); err == nil {
			slug = detectProject(ctx, wd)
		}
	}
	if slug == "" {
		fmt.Fprintln(stderr, "error: could not detect a project name: pass --project NAME")
		return 1
	}
	gw := strings.TrimRight(strings.TrimSpace(*gateway), "/")
	if gw == "" {
		fmt.Fprintln(stderr, "error: gateway URL is required: pass --gateway URL or set HERALD_GATEWAY")
		return 1
	}

	if !*force {
		if _, err := os.Stat(*configPath); err == nil {
			fmt.Fprintf(stderr, "error: %s already exists: use --force to overwrite\n", *configPath)
			return 1
		} else if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
	}

	if err := os.WriteFile(*configPath, []byte(fmt.Sprintf(initConfigTemplate, slug, gw)), 0o644); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote %s (project %q → %s)\n", *configPath, slug, gw)

	if !*noGuidance {
		fmt.Fprintln(stdout)
		printInitGuidance(stdout, slug)
	}
	if os.Getenv("HERALD_TOKEN") != "" {
		fmt.Fprintf(stdout, "\nHERALD_TOKEN is set; %s is ready to use.\n", *configPath)
	} else {
		fmt.Fprintln(stdout, "\nHERALD_TOKEN is not set; export a token before sending (see above).")
	}
	return 0
}

// printInitGuidance explains credential setup without ever writing a secret. The
// token is issued by the operator (or by the CLI on the gateway host) and lives
// only in HERALD_TOKEN.
func printInitGuidance(w io.Writer, project string) {
	fmt.Fprintf(w, `Next steps:
  1. Get a token for project %q from the gateway operator, or issue one on the
     gateway host (it needs database access):

       herald token create --project %s --database-url "$HERALD_DATABASE_URL"

     The token (herald_%s_<secret>) is shown once; store it in your secret store.
  2. Export it where you run agents — never commit it to herald.toml:

       export HERALD_TOKEN=herald_%s_<secret>

  3. Send a test event:

       herald send --status success --title "hello from herald"

On Herald Cloud, skip steps 1-2: run "herald login --gateway <url> --email
you@example.com" to sign in and store a project token for you.

Install the agent skill so an agent can notify you when you ask it to:

    herald skill > ~/.claude/skills/herald/SKILL.md

Or register the MCP server, which exposes the send_event tool:

    herald mcp
`, project, project, project, project)
}

// detectProject resolves a project slug for dir: the git remote's repository
// name when one is configured, else the repository/directory name. It never
// fails; an unusable git checkout falls back to the base directory name.
func detectProject(ctx context.Context, dir string) string {
	if out, err := gitOutput(ctx, dir, "remote", "get-url", "origin"); err == nil {
		if name := projectFromRemote(out); name != "" {
			return name
		}
	}
	if out, err := gitOutput(ctx, dir, "rev-parse", "--show-toplevel"); err == nil {
		if name := filepath.Base(out); name != "" && name != "." && name != string(filepath.Separator) {
			return name
		}
	}
	return filepath.Base(dir)
}

// projectFromRemote extracts the repository name from a git remote URL, both
// scp-like (git@host:owner/repo.git) and HTTP/SSH forms.
func projectFromRemote(remote string) string {
	s := strings.TrimSuffix(strings.TrimSpace(remote), ".git")
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndexAny(s, "/:"); i != -1 {
		s = s[i+1:]
	}
	if s == "" || s == "." {
		return ""
	}
	return s
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
