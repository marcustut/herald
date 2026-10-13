// Package skill embeds the SKILL.md that teaches a coding harness when to
// notify the owner. `herald skill` prints it for installation.
package skill

import _ "embed"

//go:embed SKILL.md
var content string

// Content returns the skill text.
func Content() string { return content }
