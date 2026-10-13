package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedNow() time.Time { return time.Unix(1_700_000_000, 0) }

func testInput() hookInput {
	return hookInput{Event: eventStop, SessionID: "s1", CWD: "/home/u/proj"}
}

func enabledPolicy() policy {
	return policy{
		Enabled:  true,
		Events:   eventSet(""),
		Debounce: 30 * time.Second,
		Agent:    defaultAgent,
		StateDir: "unused",
		Bin:      defaultBin,
	}
}

func withEvents(events string) policy {
	p := enabledPolicy()
	p.Events = eventSet(events)
	return p
}

func withDebounce(d time.Duration) policy {
	p := enabledPolicy()
	p.Debounce = d
	return p
}

func TestDecide(t *testing.T) {
	sent := state{SessionID: "s1", LastStatus: statusSuccess, LastEvent: eventStop, LastAt: fixedNow().Unix(), Seq: 1}

	tests := []struct {
		name       string
		input      hookInput
		prev       state
		policy     policy
		now        time.Time
		wantSend   bool
		wantStatus string
		wantOcc    string
		wantReason string
	}{
		{name: "disabled sends nothing", input: testInput(), policy: policy{Enabled: false, Events: eventSet("")}, now: fixedNow(), wantReason: "disabled"},
		{name: "progress hook never sends", input: hookInput{Event: "UserPromptSubmit", SessionID: "s1"}, policy: enabledPolicy(), now: fixedNow(), wantReason: "non_terminal_event"},
		{name: "subagent stop not enabled by default", input: hookInput{Event: eventSubagentStop, SessionID: "s1"}, policy: enabledPolicy(), now: fixedNow(), wantReason: "event_not_enabled"},
		{name: "stop sends success", input: testInput(), policy: enabledPolicy(), now: fixedNow(), wantSend: true, wantStatus: statusSuccess, wantOcc: "claude-code-s1-1"},
		{name: "notification sends blocked", input: hookInput{Event: eventNotification, SessionID: "s1"}, policy: enabledPolicy(), now: fixedNow(), wantSend: true, wantStatus: statusBlocked, wantOcc: "claude-code-s1-1"},
		{name: "duplicate stop is debounced", input: testInput(), prev: sent, policy: enabledPolicy(), now: fixedNow(), wantReason: "debounced"},
		{name: "duplicate stop after window is a new transition", input: testInput(), prev: sent, policy: enabledPolicy(), now: fixedNow().Add(31 * time.Second), wantSend: true, wantStatus: statusSuccess, wantOcc: "claude-code-s1-2"},
		{name: "status change is a new transition", input: hookInput{Event: eventNotification, SessionID: "s1"}, prev: sent, policy: enabledPolicy(), now: fixedNow(), wantSend: true, wantStatus: statusBlocked, wantOcc: "claude-code-s1-2"},
		{name: "subagent stop when opted in", input: hookInput{Event: eventSubagentStop, SessionID: "s1"}, policy: withEvents("Stop,SubagentStop,Notification"), now: fixedNow(), wantSend: true, wantStatus: statusSuccess, wantOcc: "claude-code-s1-1"},
		{name: "only blocked opted in ignores finished", input: testInput(), policy: withEvents("Notification"), now: fixedNow(), wantReason: "event_not_enabled"},
		{name: "only blocked opted in sends blocked", input: hookInput{Event: eventNotification, SessionID: "s1"}, policy: withEvents("Notification"), now: fixedNow(), wantSend: true, wantStatus: statusBlocked, wantOcc: "claude-code-s1-1"},
		{name: "another session is not debounced", input: hookInput{Event: eventStop, SessionID: "s2", CWD: "/x"}, prev: sent, policy: enabledPolicy(), now: fixedNow(), wantSend: true, wantStatus: statusSuccess, wantOcc: "claude-code-s2-1"},
		{name: "zero debounce always sends", input: testInput(), prev: state{SessionID: "s1", LastStatus: statusSuccess, LastEvent: eventStop, LastAt: fixedNow().Unix(), Seq: 1}, policy: withDebounce(0), now: fixedNow(), wantSend: true, wantStatus: statusSuccess, wantOcc: "claude-code-s1-2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decide(tt.input, tt.prev, tt.policy, tt.now)
			if got.Send != tt.wantSend {
				t.Fatalf("Send = %v, want %v (reason %q)", got.Send, tt.wantSend, got.Reason)
			}
			if !tt.wantSend {
				if got.Reason != tt.wantReason {
					t.Errorf("Reason = %q, want %q", got.Reason, tt.wantReason)
				}
				if got.Next != tt.prev {
					t.Errorf("Next = %+v, want unchanged %+v", got.Next, tt.prev)
				}
				return
			}
			if got.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q", got.Status, tt.wantStatus)
			}
			if got.Occurrence != tt.wantOcc {
				t.Errorf("Occurrence = %q, want %q", got.Occurrence, tt.wantOcc)
			}
			if got.Next.LastAt != tt.now.Unix() {
				t.Errorf("Next.LastAt = %d, want %d", got.Next.LastAt, tt.now.Unix())
			}
		})
	}
}

func TestDecideOtherSessionStartsSeqOne(t *testing.T) {
	prev := state{SessionID: "old", LastStatus: statusSuccess, LastEvent: eventStop, LastAt: fixedNow().Unix(), Seq: 9}
	got := decide(hookInput{Event: eventStop, SessionID: "new", CWD: "/x"}, prev, enabledPolicy(), fixedNow())
	if !got.Send || got.Occurrence != "claude-code-new-1" {
		t.Fatalf("got send=%v occurrence=%q, want new session seq 1", got.Send, got.Occurrence)
	}
}

func TestPolicyFromEnv(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }

	def := policyFromEnv(getenv)
	if def.Enabled {
		t.Error("hook must be disabled by default (consent)")
	}
	if !def.Events[eventStop] || !def.Events[eventNotification] || def.Events[eventSubagentStop] {
		t.Errorf("default events = %v, want Stop+Notification only", def.Events)
	}
	if def.Debounce != defaultDebounce {
		t.Errorf("Debounce = %s, want %s", def.Debounce, defaultDebounce)
	}
	if def.Agent != defaultAgent || def.Bin != defaultBin {
		t.Errorf("Agent/Bin = %q/%q", def.Agent, def.Bin)
	}

	env["HERALD_HOOK_ENABLED"] = "YES"
	env["HERALD_HOOK_EVENTS"] = "Stop, SubagentStop, Bogus"
	env["HERALD_HOOK_DEBOUNCE_SECONDS"] = "5"
	env["HERALD_HOOK_AGENT"] = "my-agent"
	env["HERALD_HOOK_BIN"] = "/opt/herald"
	env["HERALD_HOOK_STATE_DIR"] = "/var/tmp/herald"
	got := policyFromEnv(getenv)
	if !got.Enabled {
		t.Error("YES should enable")
	}
	if got.Events[eventNotification] || !got.Events[eventSubagentStop] || !got.Events[eventStop] {
		t.Errorf("events = %v, want Stop+SubagentStop", got.Events)
	}
	if got.Debounce != 5*time.Second {
		t.Errorf("Debounce = %s, want 5s", got.Debounce)
	}
	if got.Agent != "my-agent" || got.Bin != "/opt/herald" || got.StateDir != "/var/tmp/herald" {
		t.Errorf("resolved policy = %+v", got)
	}

	env["HERALD_HOOK_DEBOUNCE_SECONDS"] = "not-a-number"
	if policyFromEnv(getenv).Debounce != defaultDebounce {
		t.Error("invalid debounce should keep the default")
	}
	env["HERALD_HOOK_DEBOUNCE_SECONDS"] = "-1"
	if policyFromEnv(getenv).Debounce != defaultDebounce {
		t.Error("negative debounce should keep the default")
	}
}

func TestPolicyAgentFallsBackToHeraldAgent(t *testing.T) {
	env := map[string]string{"HERALD_AGENT": "ci-bot"}
	p := policyFromEnv(func(k string) string { return env[k] })
	if p.Agent != "ci-bot" {
		t.Errorf("Agent = %q, want ci-bot", p.Agent)
	}
}

func TestDefaultStateDir(t *testing.T) {
	env := map[string]string{"XDG_CACHE_HOME": "/home/u/.cache"}
	if got := defaultStateDir(func(k string) string { return env[k] }); got != filepath.Join("/home/u/.cache", "herald", "hooks") {
		t.Errorf("defaultStateDir = %q", got)
	}
}

func TestDefaultStateDirFallback(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")
	if got := defaultStateDir(func(string) string { return "" }); got != filepath.Join(os.TempDir(), "herald-hooks") {
		t.Errorf("fallback defaultStateDir = %q, want temp dir", got)
	}
}

func TestStateRoundTripAndPath(t *testing.T) {
	dir := t.TempDir()
	in := state{SessionID: "sess-123", LastStatus: statusSuccess, LastEvent: eventStop, LastAt: 42, Seq: 2}
	if err := saveState(dir, "sess-123", in); err != nil {
		t.Fatal(err)
	}
	got, err := loadState(dir, "sess-123")
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Errorf("round trip = %+v, want %+v", got, in)
	}
	if _, err := os.Stat(statePath(dir, "sess-123")); err != nil {
		t.Fatalf("state file missing: %v", err)
	}
}

func TestLoadStateMissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	if got, err := loadState(dir, "absent"); err != nil || got != (state{}) {
		t.Errorf("missing state = %+v, %v; want zero, nil", got, err)
	}
	if err := os.WriteFile(statePath(dir, "bad"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadState(dir, "bad"); err == nil {
		t.Error("corrupt state should error")
	}
	if err := os.Mkdir(statePath(dir, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := loadState(dir, "dir"); err == nil {
		t.Error("unreadable state should error")
	}
}

func TestSaveStateError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveState(file, "s", state{Seq: 1}); err == nil {
		t.Error("saveState into a file should error")
	}
}

func TestStateKey(t *testing.T) {
	if got := stateKey("../../etc/passwd"); got != "etcpasswd" {
		t.Errorf("stateKey = %q, want etcpasswd", got)
	}
	if got := stateKey(""); got != "default" {
		t.Errorf("stateKey empty = %q, want default", got)
	}
	long := strings.Repeat("a", 200)
	if got := stateKey(long); len(got) != maxKeyLen {
		t.Errorf("stateKey length = %d, want %d", len(got), maxKeyLen)
	}
}

func TestTitlesAndBody(t *testing.T) {
	stop := hookInput{Event: eventStop, SessionID: "s1", CWD: "/home/u/proj"}
	if got := titleFor(stop, statusSuccess); got != "Claude Code finished: proj" {
		t.Errorf("titleFor stop = %q", got)
	}
	blocked := hookInput{Event: eventNotification, SessionID: "s1", CWD: "/home/u/proj", Message: "needs permission"}
	if got := titleFor(blocked, statusBlocked); got != "Claude Code blocked: proj" {
		t.Errorf("titleFor blocked = %q", got)
	}
	if body := bodyFor(blocked, statusBlocked); !strings.Contains(body, "needs permission") {
		t.Errorf("bodyFor blocked = %q, want the notification message", body)
	}
	if title := titleFor(hookInput{Event: eventStop, CWD: "."}, statusSuccess); title != "Claude Code finished" {
		t.Errorf("titleFor without cwd = %q", title)
	}
	if title := titleFor(hookInput{Event: eventStop, CWD: "/"}, statusSuccess); title != "Claude Code finished" {
		t.Errorf("titleFor at root = %q", title)
	}
	if title := titleFor(hookInput{Event: eventSubagentStop, CWD: "/home/u/proj"}, statusSuccess); title != "Claude Code subagent finished: proj" {
		t.Errorf("titleFor subagent = %q", title)
	}
	if got := truncate(strings.Repeat("x", 300), maxTitle); len([]rune(got)) != maxTitle {
		t.Errorf("truncate did not cap at %d", maxTitle)
	}
	if got := truncate("short", maxTitle); got != "short" {
		t.Errorf("truncate = %q, want unchanged", got)
	}
	multibyte := strings.Repeat("é", 5)
	if got := truncate(multibyte, 6); got != multibyte {
		t.Errorf("truncate should not split runes: %q", got)
	}
	if got := truncate(multibyte, 3); got != strings.Repeat("é", 3) {
		t.Errorf("truncate multibyte = %q", got)
	}
	if firstNonEmpty("", "") != "" {
		t.Error("firstNonEmpty of empties should be empty")
	}
	if firstNonEmpty("", "b", "c") != "b" {
		t.Error("firstNonEmpty should return the first non-empty")
	}
}

// stubHerald writes an executable `herald` that records its argv to HERALD_TEST_CALLS.
func stubHerald(t *testing.T) (bin, record string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "herald")
	record = filepath.Join(dir, "calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" | tr '\\n' ' ' >> \"$HERALD_TEST_CALLS\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERALD_TEST_CALLS", record)
	return bin, record
}

func TestRunSendsOncePerTransition(t *testing.T) {
	bin, record := stubHerald(t)
	stateDir := t.TempDir()
	getenv := func(k string) string {
		switch k {
		case "HERALD_HOOK_ENABLED":
			return "1"
		case "HERALD_HOOK_BIN":
			return bin
		case "HERALD_HOOK_STATE_DIR":
			return stateDir
		default:
			return ""
		}
	}
	hookJSON := `{"hook_event_name":"Stop","session_id":"sess-abc","cwd":"/home/u/proj"}`

	var stderr bytes.Buffer
	for i := 0; i < 2; i++ {
		if code := run(context.Background(), strings.NewReader(hookJSON), &stderr, getenv, fixedNow, execSender); code != 0 {
			t.Fatalf("run exit = %d, want 0", code)
		}
	}

	calls := readCalls(t, record)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1 (debounced):\n%s", len(calls), strings.Join(calls, "\n"))
	}
	for _, want := range []string{"send", "--status success", "--occurrence-id claude-code-sess-abc-1", "--agent claude-code", "--title Claude Code finished: proj"} {
		if !strings.Contains(calls[0], want) {
			t.Errorf("call %q missing %q", calls[0], want)
		}
	}
}

func TestRunDisabledSendsNothing(t *testing.T) {
	bin, record := stubHerald(t)
	getenv := func(k string) string {
		switch k {
		case "HERALD_HOOK_BIN":
			return bin
		case "HERALD_HOOK_STATE_DIR":
			return t.TempDir()
		default:
			return ""
		}
	}
	hookJSON := `{"hook_event_name":"Stop","session_id":"sess-abc"}`
	if code := run(context.Background(), strings.NewReader(hookJSON), &bytes.Buffer{}, getenv, fixedNow, execSender); code != 0 {
		t.Fatalf("run exit = %d", code)
	}
	if calls := readCalls(t, record); len(calls) != 0 {
		t.Fatalf("disabled hook sent %d events", len(calls))
	}
}

// TestRunNotifiesEveryTurnWhenDebounceOff covers the explicit "notify me on
// every turn/checkpoint" opt-in: with the debounce window disabled, each
// terminal hook for the same session is its own event.
func TestRunNotifiesEveryTurnWhenDebounceOff(t *testing.T) {
	bin, record := stubHerald(t)
	stateDir := t.TempDir()
	getenv := func(k string) string {
		switch k {
		case "HERALD_HOOK_ENABLED":
			return "1"
		case "HERALD_HOOK_DEBOUNCE_SECONDS":
			return "0"
		case "HERALD_HOOK_BIN":
			return bin
		case "HERALD_HOOK_STATE_DIR":
			return stateDir
		default:
			return ""
		}
	}
	hookJSON := `{"hook_event_name":"Stop","session_id":"sess-each","cwd":"/home/u/proj"}`
	var stderr bytes.Buffer
	for i := 0; i < 2; i++ {
		if code := run(context.Background(), strings.NewReader(hookJSON), &stderr, getenv, fixedNow, execSender); code != 0 {
			t.Fatalf("run exit = %d, want 0", code)
		}
	}
	calls := readCalls(t, record)
	if len(calls) != 1 { // the stub concatenates runs on one line
		t.Fatalf("got %d recorded lines, want 1: %q", len(calls), strings.Join(calls, "\n"))
	}
	if n := strings.Count(calls[0], "send --status"); n != 2 {
		t.Fatalf("got %d sends, want 2 (one per turn): %q", n, calls[0])
	}
	if !strings.Contains(calls[0], "--occurrence-id claude-code-sess-each-1") || !strings.Contains(calls[0], "--occurrence-id claude-code-sess-each-2") {
		t.Errorf("occurrence ids = %q, want incrementing per turn", calls[0])
	}
}

func TestRunBlockedNotification(t *testing.T) {
	bin, record := stubHerald(t)
	getenv := func(k string) string {
		switch k {
		case "HERALD_HOOK_ENABLED":
			return "true"
		case "HERALD_HOOK_BIN":
			return bin
		case "HERALD_HOOK_STATE_DIR":
			return t.TempDir()
		default:
			return ""
		}
	}
	hookJSON := `{"hook_event_name":"Notification","session_id":"s9","cwd":"/x","message":"Claude needs your permission to use Bash"}`
	if code := run(context.Background(), strings.NewReader(hookJSON), &bytes.Buffer{}, getenv, fixedNow, execSender); code != 0 {
		t.Fatalf("run exit = %d", code)
	}
	calls := readCalls(t, record)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	if !strings.Contains(calls[0], "--status blocked") || !strings.Contains(calls[0], "needs your permission") {
		t.Errorf("blocked call = %q", calls[0])
	}
}

func TestRunRetryReusesOccurrenceOnFailure(t *testing.T) {
	stateDir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	getenv := func(k string) string {
		switch k {
		case "HERALD_HOOK_ENABLED":
			return "1"
		case "HERALD_HOOK_BIN":
			return missing
		case "HERALD_HOOK_STATE_DIR":
			return stateDir
		default:
			return ""
		}
	}
	hookJSON := `{"hook_event_name":"Stop","session_id":"sess-abc"}`
	var stderr bytes.Buffer
	if code := run(context.Background(), strings.NewReader(hookJSON), &stderr, getenv, fixedNow, execSender); code != 0 {
		t.Fatalf("run exit = %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "herald send failed") {
		t.Errorf("stderr = %q, want send failure report", stderr.String())
	}
	// A failed send must not consume a sequence number: the retry reuses seq 1.
	prev, err := loadState(stateDir, "sess-abc")
	if err != nil {
		t.Fatal(err)
	}
	retry := hookInput{Event: eventStop, SessionID: "sess-abc", CWD: "/home/u/proj"}
	if got := decide(retry, prev, enabledPolicy(), fixedNow()); got.Occurrence != "claude-code-sess-abc-1" {
		t.Errorf("occurrence = %q, want the retry to reuse seq 1", got.Occurrence)
	}
}

func TestRunInvalidJSONNeverFails(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(context.Background(), strings.NewReader("not json"), &stderr, func(string) string { return "" }, fixedNow, execSender); code != 0 {
		t.Fatalf("run exit = %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "invalid hook JSON") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestRunReadErrorNeverFails(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(context.Background(), errReader{}, &stderr, func(string) string { return "" }, fixedNow, execSender); code != 0 {
		t.Fatalf("run exit = %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "read hook input") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunSaveStateErrorStillExitsZero(t *testing.T) {
	bin, _ := stubHerald(t)
	stateFile := filepath.Join(t.TempDir(), "state-file")
	if err := os.WriteFile(stateFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string {
		switch k {
		case "HERALD_HOOK_ENABLED":
			return "1"
		case "HERALD_HOOK_BIN":
			return bin
		case "HERALD_HOOK_STATE_DIR":
			return stateFile
		default:
			return ""
		}
	}
	var stderr bytes.Buffer
	code := run(context.Background(), strings.NewReader(`{"hook_event_name":"Stop","session_id":"s1"}`), &stderr, getenv, fixedNow, execSender)
	if code != 0 || !strings.Contains(stderr.String(), "save state") {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
}

func TestRunReportsUnreadableState(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(statePath(dir, "s1"), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin, _ := stubHerald(t)
	getenv := func(k string) string {
		switch k {
		case "HERALD_HOOK_ENABLED":
			return "1"
		case "HERALD_HOOK_BIN":
			return bin
		case "HERALD_HOOK_STATE_DIR":
			return dir
		default:
			return ""
		}
	}
	var stderr bytes.Buffer
	code := run(context.Background(), strings.NewReader(`{"hook_event_name":"Stop","session_id":"s1"}`), &stderr, getenv, fixedNow, execSender)
	if code != 0 || !strings.Contains(stderr.String(), "read state") {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
}

func readCalls(t *testing.T, record string) []string {
	t.Helper()
	data, err := os.ReadFile(record)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}
