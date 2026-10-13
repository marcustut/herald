package cli

import (
	"fmt"
	"io"

	"github.com/marcustut/herald/internal/skill"
)

// runSkill prints the agent skill so it can be installed into a harness, e.g.
//
//	herald skill > ~/.claude/skills/herald/SKILL.md
//
// It appends the configured gateway and project, so the installed skill points
// at the same cloud or self-hosted gateway the CLI uses.
func runSkill(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("skill", stderr)
	configPath := fs.String("config", "herald.toml", "path to herald.toml")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fmt.Fprint(stdout, skill.Content())

	gateway, project, err := clientConfig(*configPath)
	if err != nil {
		// The skill is still usable; only the gateway hint is unavailable.
		fmt.Fprintln(stderr, "note: could not read client config:", err)
	}
	fmt.Fprintln(stdout)
	if gateway == "" {
		fmt.Fprintln(stdout, "## Configured gateway")
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "No gateway is configured yet. Run `herald login` (hosted) or `herald init --gateway <url>` (self-host).")
		return 0
	}
	fmt.Fprintln(stdout, "## Configured gateway")
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "This installation sends events to %s", gateway)
	if project != "" {
		fmt.Fprintf(stdout, " (project %s)", project)
	}
	fmt.Fprintln(stdout, ".")
	fmt.Fprintln(stdout, "`herald send` resolves it from herald.toml, HERALD_GATEWAY, or the credential file.")
	return 0
}
