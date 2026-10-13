package skill

import (
	"strings"
	"testing"
)

func TestContent(t *testing.T) {
	content := Content()
	for _, want := range []string{"name: herald-notify", "herald send", "explicitly asked"} {
		if !strings.Contains(content, want) {
			t.Errorf("skill missing %q", want)
		}
	}
}
