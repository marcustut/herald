// Package cli implements the herald command line: serve, send, token and mcp.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/marcustut/herald/internal/client"
	"github.com/marcustut/herald/internal/config"
	"github.com/marcustut/herald/internal/credentials"
	"github.com/marcustut/herald/internal/version"
)

const usage = `herald — agent event gateway

Usage:
  herald init  [--config FILE] [--project NAME] [--gateway URL] [--force] [--no-guidance]
  herald login [--config FILE] [--gateway URL] [--project NAME] [--token TOKEN]
               [--email EMAIL] [--test]
  herald serve [--config FILE] [--database-url URL] [--dev] [--dispatch] [--issue-token PROJECT --tenant ID]
  herald dispatch [--config FILE] --database-url URL [--workers N]
  herald send  [--config FILE] --status STATUS --title TITLE [--body TEXT]
               [--link LABEL=URL]... [--agent NAME] [--host NAME]
               [--occurrence-id ID] [--idempotency-key KEY] [--json]
  herald token create --database-url URL --tenant ID --project PROJECT [--scoped P1,P2]
  herald token revoke --database-url URL --tenant ID --project PROJECT
  herald delivery list [--state STATE] [--event ID] [--sink ID] [--limit N] [--database-url URL]
  herald delivery show <id> [--database-url URL]
  herald delivery retry <id> [--database-url URL]
  herald delivery requeue [--state dead|pending] [--event ID] [--sink ID] [--database-url URL]
  herald delivery reclaim [--database-url URL]
  herald mcp   [--config FILE] [--agent NAME] [--host NAME]
  herald skill [--config FILE]
  herald version
`

// Run executes a command and returns a process exit code.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "init":
		return runInit(ctx, args[1:], stdout, stderr)
	case "login":
		return runLogin(ctx, args[1:], stdin, stdout, stderr)
	case "serve":
		return runServe(ctx, args[1:], stdout, stderr)
	case "dispatch":
		return runDispatch(ctx, args[1:], stdout, stderr)
	case "send":
		return runSend(ctx, args[1:], stdin, stdout, stderr)
	case "token":
		return runToken(ctx, args[1:], stdout, stderr)
	case "delivery":
		return runDelivery(ctx, args[1:], stdout, stderr)
	case "mcp":
		return runMCP(ctx, args[1:], stdin, stdout, stderr)
	case "skill":
		return runSkill(args[1:], stdout, stderr)
	case "version", "-v", "--version":
		fmt.Fprintln(stdout, version.String())
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

// clientConfig resolves the gateway URL and project from a client config file
// and the environment. The config file supplies both; HERALD_GATEWAY and
// HERALD_PROJECT override them. A missing file is not an error.
func clientConfig(configPath string) (gateway, project string, err error) {
	if configPath != "" {
		if _, statErr := os.Stat(configPath); statErr == nil {
			cfg, err := config.LoadClient(configPath)
			if err != nil {
				return "", "", err
			}
			project, gateway = cfg.Project, cfg.Gateway
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", "", statErr
		}
	}
	if v := os.Getenv("HERALD_PROJECT"); v != "" {
		project = v
	}
	if v := os.Getenv("HERALD_GATEWAY"); v != "" {
		gateway = v
	}
	return gateway, project, nil
}

// resolveToken resolves the bearer token, preferring the process environment
// over the user credential file written by `herald login`. herald.toml never
// carries a token, so the order is: env > credential file > herald.toml.
func resolveToken() (string, error) {
	if v := os.Getenv("HERALD_TOKEN"); v != "" {
		return v, nil
	}
	return credentials.Token()
}

// gatewayClient resolves the gateway URL, token, project, agent and host from a
// client config file, the environment and the user credential file.
func gatewayClient(configPath string) (client.Client, string, string, string, error) {
	gateway, project, err := clientConfig(configPath)
	if err != nil {
		return client.Client{}, "", "", "", err
	}
	if gateway == "" {
		return client.Client{}, "", "", "", fmt.Errorf("gateway URL is missing: set gateway in herald.toml or HERALD_GATEWAY (herald init writes it)")
	}
	token, err := resolveToken()
	if err != nil {
		return client.Client{}, "", "", "", err
	}
	if token == "" {
		return client.Client{}, "", "", "", fmt.Errorf("HERALD_TOKEN is missing: run `herald login` to store one, or issue one with `herald token create --project <name>` and export it")
	}
	return client.Client{Gateway: gateway, Token: token}, project, os.Getenv("HERALD_AGENT"), os.Getenv("HERALD_HOST"), nil
}
