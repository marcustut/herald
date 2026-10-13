package herdr

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/BurntSushi/toml"
)

const scriptName = "herald-event.sh"

// event renders the compact pane_agent_status_changed envelope herdr delivers.
func event(status, pane, agent string) string {
	return fmt.Sprintf(
		`{"event":"pane_agent_status_changed","data":{"type":"pane_agent_status_changed","pane_id":%q,"workspace_id":"w1","agent_status":%q,"agent":%q}}`,
		pane, status, agent,
	)
}

// fixture is a throwaway plugin environment: fake herdr and herald binaries plus
// a plugin config dir, so the hook can be exercised without a running server.
type fixture struct {
	scriptPath string
	herdrBin   string
	heraldBin  string
	configDir  string
	outDir     string
}

func newFixture(t *testing.T, herdrJSON string, heraldExit int) *fixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the hook targets linux/macos; sh is unavailable")
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := t.TempDir()
	f := &fixture{
		scriptPath: filepath.Join(wd, scriptName),
		configDir:  filepath.Join(dir, "config"),
		herdrBin:   filepath.Join(dir, "herdr"),
		heraldBin:  filepath.Join(dir, "herald"),
		outDir:     filepath.Join(dir, "sent"),
	}
	for _, d := range []string{f.configDir, f.outDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	// Fake herdr: always answers `agent get` with the JSON under test.
	writeExec(t, f.herdrBin, fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' %s\n", shellQuote(herdrJSON)))
	// Fake herald: records its argv to a per-process file (one arg per line), so
	// concurrent invocations never interleave, then exits with the given code.
	writeExec(t, f.heraldBin, fmt.Sprintf(
		"#!/bin/sh\nprintf '%%s\\n' \"$@\" > %s/$$\nexit %d\n",
		shellQuote(f.outDir), heraldExit,
	))
	return f
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeEnv writes the scoped-token config the hook sources.
func (f *fixture) writeEnv(t *testing.T, body string) {
	t.Helper()
	writeExec(t, filepath.Join(f.configDir, "herald.env"), body)
}

// run invokes the hook with a clean environment and returns exit code + stderr.
// It never calls t.Fatal so it is safe from goroutines in the concurrency test.
func (f *fixture) run(eventJSON string) (int, string) {
	cmd := exec.Command("sh", f.scriptPath)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HERDR_PLUGIN_EVENT_JSON=" + eventJSON,
		"HERDR_PLUGIN_CONFIG_DIR=" + f.configDir,
		"HERDR_BIN_PATH=" + f.herdrBin,
		"HERALD_BIN=" + f.heraldBin,
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return 0, stderr.String()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), stderr.String()
	}
	return -1, err.Error()
}

// sends returns each recorded `herald send` invocation as a field slice.
func (f *fixture) sends(t *testing.T) [][]string {
	t.Helper()
	entries, err := os.ReadDir(f.outDir)
	if err != nil {
		t.Fatalf("read sent dir: %v", err)
	}
	var out [][]string
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(f.outDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		out = append(out, strings.Split(strings.TrimSpace(string(data)), "\n"))
	}
	return out
}

func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

const goodEnv = "HERALD_GATEWAY=https://herald.example\nHERALD_PROJECT=herdr\nHERALD_TOKEN=heraldacct_secret\n"

func TestManifestDeclaresLifecycleHook(t *testing.T) {
	var m struct {
		ID              string `toml:"id"`
		Name            string `toml:"name"`
		Version         string `toml:"version"`
		MinHerdrVersion string `toml:"min_herdr_version"`
		Events          []struct {
			On      string   `toml:"on"`
			Command []string `toml:"command"`
		} `toml:"events"`
	}
	if _, err := toml.DecodeFile("herdr-plugin.toml", &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if m.ID != "herald.notify" {
		t.Errorf("id = %q", m.ID)
	}
	if m.Name == "" || m.Version == "" || m.MinHerdrVersion == "" {
		t.Errorf("missing required metadata: %+v", m)
	}
	if len(m.Events) != 1 {
		t.Fatalf("want exactly one event hook, got %d", len(m.Events))
	}
	if m.Events[0].On != "pane.agent_status_changed" {
		t.Errorf("hook on = %q", m.Events[0].On)
	}
	if got := strings.Join(m.Events[0].Command, " "); got != "sh "+scriptName {
		t.Errorf("hook command = %q", got)
	}
}

func TestDoneTransitionSendsSuccess(t *testing.T) {
	f := newFixture(t, `{"result":{"agent":{"name":"herald-1","cwd":"/tmp/herald-1","completion_seq":340,"state_change_seq":340}}}`, 0)
	f.writeEnv(t, goodEnv)

	code, stderr := f.run(event("done", "wQ:p1", "opencode"))
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr)
	}
	sends := f.sends(t)
	if len(sends) != 1 {
		t.Fatalf("want 1 send, got %d: %v", len(sends), sends)
	}
	if got := argValue(sends[0], "--status"); got != "success" {
		t.Errorf("status = %q", got)
	}
	if got := argValue(sends[0], "--occurrence-id"); got != "herdr:wQ:p1:done:340" {
		t.Errorf("occurrence-id = %q", got)
	}
	if got := argValue(sends[0], "--agent"); got != "opencode" {
		t.Errorf("agent = %q", got)
	}
	if got := argValue(sends[0], "--title"); !strings.Contains(got, "herald-1") {
		t.Errorf("title = %q, want the agent name", got)
	}
}

func TestBlockedTransitionSendsBlocked(t *testing.T) {
	f := newFixture(t, `{"result":{"agent":{"name":"herald-2","cwd":"/tmp/herald-2","completion_seq":null,"state_change_seq":12}}}`, 0)
	f.writeEnv(t, goodEnv)

	code, stderr := f.run(event("blocked", "wP:p1", "opencode"))
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr)
	}
	sends := f.sends(t)
	if len(sends) != 1 {
		t.Fatalf("want 1 send, got %d: %v", len(sends), sends)
	}
	if got := argValue(sends[0], "--status"); got != "blocked" {
		t.Errorf("status = %q", got)
	}
	if got := argValue(sends[0], "--occurrence-id"); got != "herdr:wP:p1:blocked:12" {
		t.Errorf("occurrence-id = %q", got)
	}
}

func TestIgnoresNonTerminalStates(t *testing.T) {
	for _, status := range []string{"idle", "working", "unknown"} {
		t.Run(status, func(t *testing.T) {
			f := newFixture(t, `{"result":{"agent":{"completion_seq":9,"state_change_seq":9}}}`, 0)
			f.writeEnv(t, goodEnv)
			code, stderr := f.run(event(status, "wQ:p1", "opencode"))
			if code != 0 {
				t.Fatalf("exit = %d, stderr: %s", code, stderr)
			}
			if sends := f.sends(t); len(sends) != 0 {
				t.Errorf("status %q should not send, got %v", status, sends)
			}
		})
	}
}

func TestDuplicateDeliveryReusesOccurrence(t *testing.T) {
	f := newFixture(t, `{"result":{"agent":{"completion_seq":340,"state_change_seq":340}}}`, 0)
	f.writeEnv(t, goodEnv)

	ev := event("done", "wQ:p1", "opencode")
	f.run(ev)
	f.run(ev)

	sends := f.sends(t)
	if len(sends) != 2 {
		t.Fatalf("want 2 attempts, got %d", len(sends))
	}
	a := argValue(sends[0], "--occurrence-id")
	b := argValue(sends[1], "--occurrence-id")
	if a != b {
		t.Errorf("duplicate deliveries differ: %q vs %q", a, b)
	}
}

func TestDistinctCompletionsGetDistinctOccurrences(t *testing.T) {
	f := newFixture(t, `{"result":{"agent":{"completion_seq":340,"state_change_seq":340}}}`, 0)
	f.writeEnv(t, goodEnv)
	f.run(event("done", "wQ:p1", "opencode"))

	// A second, genuinely new completion on the same pane bumps completion_seq.
	writeExec(t, f.herdrBin, "#!/bin/sh\nprintf '%s\\n' '{\"result\":{\"agent\":{\"completion_seq\":341,\"state_change_seq\":341}}}'\n")
	f.run(event("done", "wQ:p1", "opencode"))

	sends := f.sends(t)
	if len(sends) != 2 {
		t.Fatalf("want 2 sends, got %d", len(sends))
	}
	if argValue(sends[0], "--occurrence-id") == argValue(sends[1], "--occurrence-id") {
		t.Errorf("different completions collapsed to one occurrence id")
	}
}

func TestMissingConfigDoesNotSend(t *testing.T) {
	f := newFixture(t, `{"result":{"agent":{}}}`, 0)
	code, _ := f.run(event("done", "wQ:p1", "opencode"))
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if sends := f.sends(t); len(sends) != 0 {
		t.Errorf("sent without config: %v", sends)
	}
}

func TestIncompleteConfigDoesNotSend(t *testing.T) {
	// Token scoping needs the gateway, the project and the token; any missing
	// one must simply skip, never send a half-addressed event.
	full := map[string]string{
		"HERALD_GATEWAY": "https://herald.example",
		"HERALD_PROJECT": "herdr",
		"HERALD_TOKEN":   "heraldacct_secret",
	}
	for missing := range full {
		t.Run(missing, func(t *testing.T) {
			f := newFixture(t, `{"result":{"agent":{}}}`, 0)
			var b strings.Builder
			for k, v := range full {
				if k != missing {
					fmt.Fprintf(&b, "%s=%s\n", k, v)
				}
			}
			f.writeEnv(t, b.String())
			code, _ := f.run(event("done", "wQ:p1", "opencode"))
			if code != 0 {
				t.Errorf("exit = %d, want 0", code)
			}
			if sends := f.sends(t); len(sends) != 0 {
				t.Errorf("sent with %s missing: %v", missing, sends)
			}
		})
	}
}

func TestHeraldFailureIsIsolated(t *testing.T) {
	f := newFixture(t, `{"result":{"agent":{"completion_seq":340,"state_change_seq":340}}}`, 3)
	f.writeEnv(t, goodEnv)
	code, stderr := f.run(event("done", "wQ:p1", "opencode"))
	if code != 0 {
		t.Fatalf("exit = %d, want 0 so herdr is never wedged", code)
	}
	if !strings.Contains(stderr, "send failed") {
		t.Errorf("stderr = %q, want a logged failure", stderr)
	}
}

func TestHerdrLookupFailureFallsBackToStableKey(t *testing.T) {
	f := newFixture(t, `{"error":{"code":"agent_not_found"}}`, 0)
	f.writeEnv(t, goodEnv)

	ev := event("blocked", "wQ:p1", "opencode")
	f.run(ev)
	f.run(ev)
	sends := f.sends(t)
	if len(sends) != 2 {
		t.Fatalf("want 2 attempts, got %d", len(sends))
	}
	a := argValue(sends[0], "--occurrence-id")
	b := argValue(sends[1], "--occurrence-id")
	if a == "" || a != b {
		t.Errorf("fallback key not stable: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "herdr:wQ:p1:blocked:") {
		t.Errorf("fallback key = %q", a)
	}
}

func TestMalformedEventDoesNotSend(t *testing.T) {
	for _, ev := range []string{"", "not json", `{"event":"pane_agent_status_changed"}`} {
		t.Run(ev, func(t *testing.T) {
			f := newFixture(t, `{"result":{"agent":{}}}`, 0)
			f.writeEnv(t, goodEnv)
			code, _ := f.run(ev)
			if code != 0 {
				t.Errorf("exit = %d, want 0", code)
			}
			if sends := f.sends(t); len(sends) != 0 {
				t.Errorf("sent on malformed event: %v", sends)
			}
		})
	}
}

func TestAgentIsSlugSafe(t *testing.T) {
	f := newFixture(t, `{"result":{"agent":{"completion_seq":1,"state_change_seq":1}}}`, 0)
	f.writeEnv(t, goodEnv)
	f.run(event("blocked", "wQ:p1", "My Agent!"))
	sends := f.sends(t)
	if len(sends) != 1 {
		t.Fatalf("want 1 send, got %d", len(sends))
	}
	agent := argValue(sends[0], "--agent")
	if strings.ContainsAny(agent, " !") || strings.HasPrefix(agent, "-") || strings.HasSuffix(agent, "-") {
		t.Errorf("agent not sanitized: %q", agent)
	}
}

func TestConcurrentDuplicatesShareOccurrence(t *testing.T) {
	f := newFixture(t, `{"result":{"agent":{"completion_seq":340,"state_change_seq":340}}}`, 0)
	f.writeEnv(t, goodEnv)

	const n = 8
	ev := event("done", "wQ:p1", "opencode")
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, stderr := f.run(ev); code != 0 {
				t.Errorf("exit = %d, stderr: %s", code, stderr)
			}
		}()
	}
	wg.Wait()

	sends := f.sends(t)
	if len(sends) != n {
		t.Fatalf("want %d attempts, got %d", n, len(sends))
	}
	want := argValue(sends[0], "--occurrence-id")
	for i, s := range sends {
		if got := argValue(s, "--occurrence-id"); got != want {
			t.Errorf("send %d occurrence = %q, want %q", i, got, want)
		}
	}
}
