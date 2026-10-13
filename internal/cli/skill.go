package cli

import (
	"fmt"
	"io"

	"github.com/marcustut/herald/internal/skill"
)

// runSkill prints the agent skill so it can be installed into a harness, e.g.
//
//	herald skill > ~/.claude/skills/herald/SKILL.md
func runSkill(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("skill", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fmt.Fprint(stdout, skill.Content())
	return 0
}
