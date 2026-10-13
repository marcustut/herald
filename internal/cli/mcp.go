package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/marcustut/herald/internal/mcp"
)

func runMCP(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlagSet("mcp", stderr)
	configPath := fs.String("config", "herald.toml", "path to herald.toml")
	agent := fs.String("agent", "", "agent name (defaults to HERALD_AGENT)")
	host := fs.String("host", "", "machine (defaults to HERALD_HOST)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cl, project, defaultAgent, defaultHost, err := gatewayClient(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	server := &mcp.Server{
		Project: project,
		Agent:   firstNonEmpty(*agent, defaultAgent),
		Host:    firstNonEmpty(*host, defaultHost),
		Client:  &cl,
		In:      stdin,
		Out:     stdout,
	}
	if err := server.Serve(ctx); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}
