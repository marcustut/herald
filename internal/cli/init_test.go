package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestInitWritesConfig(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HERALD_GATEWAY", "")
	t.Setenv("HERALD_TOKEN", "")

	code, out, errOut := run(t, "", "init", "--project", "axis", "--gateway", "https://gw.example.com/")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	data, err := os.ReadFile(filepath.Join(dir, "herald.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `project = "axis"`) {
		t.Fatalf("missing project: %s", data)
	}
	if !strings.Contains(string(data), `gateway = "https://gw.example.com"`) {
		t.Fatalf("gateway not trimmed/recorded: %s", data)
	}
	if strings.Contains(string(data), "HERALD_TOKEN=") || strings.Contains(string(data), "herald_") {
		t.Fatalf("config must not carry a secret: %s", data)
	}
	if !strings.Contains(out, "herald skill") || !strings.Contains(out, "send_event") {
		t.Fatalf("missing skill/MCP guidance: %q", out)
	}
	if !strings.Contains(out, "HERALD_TOKEN is not set") {
		t.Fatalf("missing token warning: %q", out)
	}
}

func TestInitGuidanceTokenSet(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HERALD_TOKEN", "herald_axis_secret")

	code, out, errOut := run(t, "", "init", "--project", "axis", "--gateway", "https://gw")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "HERALD_TOKEN is set") {
		t.Fatalf("expected token-set note: %q", out)
	}
}

func TestInitNoGuidance(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	code, out, _ := run(t, "", "init", "--project", "axis", "--gateway", "https://gw", "--no-guidance")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if strings.Contains(out, "herald skill") {
		t.Fatalf("guidance should be suppressed: %q", out)
	}
}

func TestInitDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	original := "# keep me\nproject = \"old\"\ngateway = \"https://old\"\n"
	if err := os.WriteFile("herald.toml", []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, errOut := run(t, "", "init", "--project", "new", "--gateway", "https://new")
	if code != 1 || !strings.Contains(errOut, "already exists") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	data, _ := os.ReadFile("herald.toml")
	if string(data) != original {
		t.Fatalf("file was modified: %s", data)
	}
}

func TestInitForceOverwrites(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("herald.toml", []byte("project = \"old\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, errOut := run(t, "", "init", "--project", "axis", "--gateway", "https://gw", "--force")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	data, _ := os.ReadFile("herald.toml")
	if !strings.Contains(string(data), `project = "axis"`) {
		t.Fatalf("not overwritten: %s", data)
	}
}

func TestInitMissingGateway(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HERALD_GATEWAY", "")

	code, _, errOut := run(t, "", "init", "--project", "axis")
	if code != 1 || !strings.Contains(errOut, "gateway URL is required") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "herald.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("config must not be written when gateway is missing")
	}
}

func TestInitDetectsGitProject(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	git(t, dir, "init", "-q")
	git(t, dir, "remote", "add", "origin", "git@github.com:marcustut/axis.git")

	code, out, errOut := run(t, "", "init", "--gateway", "https://gw")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "herald.toml"))
	if !strings.Contains(string(data), `project = "axis"`) {
		t.Fatalf("did not detect project: %s\n%s", data, out)
	}
}

func TestDetectProjectFallsBackToDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", t.TempDir()) // no git available
	if got := detectProject(context.Background(), dir); got != filepath.Base(dir) {
		t.Fatalf("got %q, want %q", got, filepath.Base(dir))
	}
}

func TestProjectFromRemote(t *testing.T) {
	cases := map[string]string{
		"https://github.com/marcustut/herald.git":   "herald",
		"https://github.com/marcustut/herald":       "herald",
		"https://github.com/marcustut/herald/":      "herald",
		"git@github.com:marcustut/herald.git":       "herald",
		"ssh://git@github.com/marcustut/herald.git": "herald",
		"": "",
	}
	for remote, want := range cases {
		if got := projectFromRemote(remote); got != want {
			t.Errorf("projectFromRemote(%q) = %q, want %q", remote, got, want)
		}
	}
}

func TestSendMissingToken(t *testing.T) {
	t.Setenv("HERALD_GATEWAY", "http://example.invalid")
	t.Setenv("HERALD_PROJECT", "axis")
	t.Setenv("HERALD_TOKEN", "")

	code, _, errOut := run(t, "", "send", "--status", "success", "--title", "t")
	if code != 1 || !strings.Contains(errOut, "HERALD_TOKEN") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
}
