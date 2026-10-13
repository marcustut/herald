package site

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The landing page is a shipped artifact: its acceptance criteria (links to
// install/docs/GitHub, an inert waitlist with consent, no trackers, no personal
// contact data) are pinned here so a later edit cannot silently regress them.

var (
	anchorHref   = regexp.MustCompile(`(?i)<a\s[^>]*href="([^"]*)"`)
	resourceAttr = regexp.MustCompile(`(?i)<(?:link|script|img)\s[^>]*(?:href|src)="([^"]*)"`)
	formTag      = regexp.MustCompile(`(?i)<form\b[^>]*>`)
	checkboxTag  = regexp.MustCompile(`(?i)<input\b[^>]*type="checkbox"[^>]*>`)
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
		"Self-host now",
		"Cloud",
		"waitlist",
		"herald send",
		"herald skill",
		"/v1/events",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html does not mention %q", want)
		}
	}
}

func TestWaitlistFormIsInertUntilWired(t *testing.T) {
	html := readFile(t, "index.html")
	tags := formTag.FindAllString(html, -1)
	if len(tags) == 0 {
		t.Fatal("index.html has no waitlist form")
	}
	for _, tag := range tags {
		lower := strings.ToLower(tag)
		if !strings.Contains(lower, `action=""`) {
			t.Errorf("form should post nowhere by default (action=\"\"), got %s", tag)
		}
		if strings.Contains(lower, `method="post"`) || strings.Contains(lower, `method='post'`) {
			t.Errorf("form must not POST by default, got %s", tag)
		}
	}

	boxes := checkboxTag.FindAllString(html, -1)
	if len(boxes) == 0 {
		t.Fatal("waitlist form has no consent checkbox")
	}
	hasRequiredConsent := false
	for _, box := range boxes {
		if strings.Contains(strings.ToLower(box), "required") {
			hasRequiredConsent = true
		}
	}
	if !hasRequiredConsent {
		t.Error("consent checkbox must be required")
	}
	if !strings.Contains(html, `href="#privacy"`) {
		t.Error("waitlist form must link to the privacy note")
	}
}

func TestLandingPageHasNoThirdPartyResources(t *testing.T) {
	html := readFile(t, "index.html")
	if strings.Contains(strings.ToLower(html), "<script") {
		t.Error("landing page must not use JavaScript")
	}
	if strings.Contains(strings.ToLower(html), "<iframe") {
		t.Error("landing page must not embed third-party frames")
	}
	for _, ref := range resourceAttr.FindAllStringSubmatch(html, -1) {
		if strings.HasPrefix(ref[1], "http://") || strings.HasPrefix(ref[1], "https://") {
			t.Errorf("landing page loads an external resource: %s", ref[1])
		}
	}
}

func TestLandingPageHasNoPersonalContactData(t *testing.T) {
	html := readFile(t, "index.html")
	for _, leak := range []string{"@marcustut", "marcustut.me", "marcustut@"} {
		if strings.Contains(html, leak) {
			t.Errorf("landing page leaks personal contact data %q; use example.com", leak)
		}
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
