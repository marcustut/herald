// Package cli implements the herald command line: serve, send, token and mcp.
package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/marcustut/herald/internal/client"
	"github.com/marcustut/herald/internal/config"
)

const version = "0.1.0"

const usage = `herald — agent event gateway

Usage:
  herald serve [--config FILE] [--database-url URL] [--dev] [--dispatch] [--issue-token PROJECT]
  herald dispatch [--config FILE] --database-url URL [--workers N]
  herald send  [--config FILE] --status STATUS --title TITLE [--body TEXT]
               [--link LABEL=URL]... [--agent NAME] [--host NAME] [--idempotency-key KEY] [--json]
  herald token create --database-url URL --project PROJECT [--tenant ID] [--scoped P1,P2]
  herald token revoke --database-url URL --project PROJECT [--tenant ID]
  herald mcp   [--config FILE] [--agent NAME] [--host NAME]
  herald skill
  herald version
`

// Run executes a command and returns a process exit code.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return runServe(ctx, args[1:], stdout, stderr)
	case "dispatch":
		return runDispatch(ctx, args[1:], stdout, stderr)
	case "send":
		return runSend(ctx, args[1:], stdin, stdout, stderr)
	case "token":
		return runToken(ctx, args[1:], stdout, stderr)
	case "mcp":
		return runMCP(ctx, args[1:], stdin, stdout, stderr)
	case "skill":
		return runSkill(args[1:], stdout, stderr)
	case "version", "-v", "--version":
		fmt.Fprintln(stdout, version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		fmt.Fprint(stderr, usage)
		return 2
	}
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// gatewayClient resolves the gateway URL, token, project, agent and host from a
// client config file and the environment. The config file wins for project and
// gateway; the environment supplies the token and defaults the rest.
func gatewayClient(configPath string) (client.Client, string, string, string, error) {
	var project, gateway string
	if configPath != "" {
		if _, err := os.Stat(configPath); err == nil {
			cfg, err := config.LoadClient(configPath)
			if err != nil {
				return client.Client{}, "", "", "", err
			}
			project, gateway = cfg.Project, cfg.Gateway
		} else if !errors.Is(err, os.ErrNotExist) {
			return client.Client{}, "", "", "", err
		}
	}
	if v := os.Getenv("HERALD_PROJECT"); v != "" {
		project = v
	}
	if v := os.Getenv("HERALD_GATEWAY"); v != "" {
		gateway = v
	}
	if gateway == "" {
		return client.Client{}, "", "", "", fmt.Errorf("gateway URL is required (herald.toml or HERALD_GATEWAY)")
	}
	token := os.Getenv("HERALD_TOKEN")
	return client.Client{Gateway: gateway, Token: token}, project, os.Getenv("HERALD_AGENT"), os.Getenv("HERALD_HOST"), nil
}

// deriveKey produces a stable idempotency key from the event content so a
// retry of the same event does not create a duplicate.
func deriveKey(status, title, body string, links []client.Link) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s", status, title, body)
	for _, l := range links {
		fmt.Fprintf(h, "\x00%s\x00%s", l.Label, l.URL)
	}
	return "herald-" + hex.EncodeToString(h.Sum(nil))[:32]
}
