package skill

import (
	"strings"
	"testing"
)

func TestContent(t *testing.T) {
	content := Content()
	for _, want := range []string{"name: herald", "herald send", "explicitly asked"} {
		if !strings.Contains(content, want) {
			t.Errorf("skill missing %q", want)
		}
	}
}

// TestContentPinsConsentPolicy keeps the skill aligned with docs/consent.md:
// opt-in, one event per user-requested terminal outcome, and no per-turn noise.
func TestContentPinsConsentPolicy(t *testing.T) {
	content := Content()
	for _, want := range []string{
		"opt-in, ask first",
		"explicitly asked to be notified",
		"One event per outcome",
		"Never notify on every turn",
		"checkpoint **only** if the user explicitly asked",
		"An event must never trigger another notification",
		"429 rate_limited",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("consent policy missing %q", want)
		}
	}
}
