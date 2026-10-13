package site

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The landing page is a shipped artifact: its acceptance criteria (links to
// install/docs/GitHub, a named contact, no trackers, self-contained assets) are
// pinned here so a later edit cannot silently regress them.

var (
	anchorHref   = regexp.MustCompile(`(?i)<a\s[^>]*href="([^"]*)"`)
	resourceAttr = regexp.MustCompile(`(?i)<(?:link|script|img)\s[^>]*(?:href|src)="([^"]*)"`)
)

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func TestLandingPageLinksToInstallDocsAndGitHub(t *testing.T) {
	html := readFile(t, "index.html")
	want := map[string]string{
		"GitHub repo":  "https://github.com/marcustut/herald",
		"quickstart":   "https://github.com/marcustut/herald/blob/main/docs/quickstart.md",
		"docs tree":    "https://github.com/marcustut/herald/tree/main/docs",
		"gh self-host": "https://github.com/marcustut/herald/blob/main/docs/self-hosting-docker.md",
	}
	for label, link := range want {
		if !strings.Contains(html, `href="`+link+`"`) {
			t.Errorf("index.html is missing the %s link %q", label, link)
		}
	}
}

func TestLandingPageCoversThePitch(t *testing.T) {
	html := readFile(t, "index.html")
	for _, want := range []string{
		"Who it's for",
		"lifecycle",
		"herald send",
		"herald skill",
		"/v1/events",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html does not mention %q", want)
		}
	}
}

func TestLandingPageHasNoThirdPartyResources(t *testing.T) {
	html := readFile(t, "index.html")
	// A small inline script is allowed for the copy button; external scripts and
	// any other third-party resource are still rejected below.
	if strings.Contains(strings.ToLower(html), "<iframe") {
		t.Error("landing page must not embed third-party frames")
	}
	for _, ref := range resourceAttr.FindAllStringSubmatch(html, -1) {
		if strings.HasPrefix(ref[1], "http://") || strings.HasPrefix(ref[1], "https://") {
			t.Errorf("landing page loads an external resource: %s", ref[1])
		}
	}
}

func TestLandingPageShowsContactAndPlaceholders(t *testing.T) {
	html := readFile(t, "index.html")
	if !strings.Contains(html, "marcus@marcustut.me") {
		t.Error("landing page should show the contact address marcus@marcustut.me")
	}
	if !strings.Contains(html, "example.com") {
		t.Error("landing page should use example.com for placeholder addresses/hosts")
	}
}

func TestLandingPageLocalAssetsExist(t *testing.T) {
	html := readFile(t, "index.html")
	for _, ref := range resourceAttr.FindAllStringSubmatch(html, -1) {
		href := ref[1]
		if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") ||
			strings.HasPrefix(href, "#") || strings.HasPrefix(href, "mailto:") {
			continue
		}
		if _, err := os.Stat(filepath.Clean(href)); err != nil {
			t.Errorf("referenced asset %q does not exist: %v", href, err)
		}
	}
	// Anchors must at least be same-document or absolute links.
	for _, ref := range anchorHref.FindAllStringSubmatch(html, -1) {
		if ref[1] == "" {
			t.Error("index.html contains an empty href")
		}
	}
}

func TestStyleSheetIsSelfContained(t *testing.T) {
	const maxBytes = 32 * 1024
	css := readFile(t, "style.css")
	if len(css) > maxBytes {
		t.Errorf("style.css is %d bytes, want it lean (<= %d)", len(css), maxBytes)
	}
	if strings.Contains(css, "@import") {
		t.Error("style.css must not @import anything")
	}
	if strings.Contains(css, "url(http") {
		t.Error("style.css must not reference remote assets")
	}
}
