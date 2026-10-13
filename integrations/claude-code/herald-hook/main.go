// Command herald-hook is a Claude Code lifecycle hook that notifies Herald
// when an agent run reaches a terminal outcome (finished or blocked), and stays
// silent on every other hook. It is consent-gated: nothing is sent unless
// HERALD_HOOK_ENABLED opts in, so it never fires on every turn.
//
// Install it as a Stop and Notification hook; see docs/lifecycle-hooks.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	statusSuccess = "success"
	statusBlocked = "blocked"

	eventStop         = "Stop"
	eventSubagentStop = "SubagentStop"
	eventNotification = "Notification"

	defaultDebounce = 30 * time.Second
	defaultAgent    = "claude-code"
	defaultBin      = "herald"

	maxTitle    = 200
	maxBody     = 8 * 1024
	maxHookJSON = 1 << 20
	maxKeyLen   = 64
)

// hookInput is the subset of the Claude Code hook payload the hook reads. All
// fields are untrusted data; only session_id and hook_event_name drive logic.
type hookInput struct {
	Event     string `json:"hook_event_name"`
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`
	Message   string `json:"message"`
}

// policy is the resolved, opt-in configuration.
type policy struct {
	Enabled  bool
	Events   map[string]bool
	Debounce time.Duration
	Agent    string
	StateDir string
	Bin      string
}

// state is the per-session debounce record persisted between hook runs.
type state struct {
	SessionID  string `json:"session_id,omitempty"`
	LastStatus string `json:"last_status,omitempty"`
	LastEvent  string `json:"last_event,omitempty"`
	LastAt     int64  `json:"last_at_unix,omitempty"`
	Seq        int    `json:"seq,omitempty"`
}

// decision is the outcome of applying the policy to one hook invocation. When
// Send is true the caller submits one event with Occurrence and persists Next.
type decision struct {
	Send       bool
	Status     string
	Occurrence string
	Reason     string
	Next       state
}

// sender runs `herald send` with args. It is a seam for tests.
type sender func(ctx context.Context, bin string, args []string) error

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	os.Exit(run(ctx, os.Stdin, os.Stderr, os.Getenv, time.Now, execSender))
}

// run never fails the harness: a hook error must not block the agent, so it
// always exits 0 and only reports problems on stderr.
func run(ctx context.Context, stdin io.Reader, stderr io.Writer, getenv func(string) string, now func() time.Time, send sender) int {
	data, err := io.ReadAll(io.LimitReader(stdin, maxHookJSON))
	if err != nil {
		fmt.Fprintln(stderr, "herald-hook: read hook input:", err)
		return 0
	}
	var in hookInput
	if err := json.Unmarshal(data, &in); err != nil {
		fmt.Fprintln(stderr, "herald-hook: invalid hook JSON:", err)
		return 0
	}

	p := policyFromEnv(getenv)
	prev, err := loadState(p.StateDir, in.SessionID)
	if err != nil {
		fmt.Fprintln(stderr, "herald-hook: read state:", err)
	}
	d := decide(in, prev, p, now())
	if !d.Send {
		return 0
	}
	if err := send(ctx, p.Bin, sendArgs(in, d, p)); err != nil {
		fmt.Fprintln(stderr, "herald-hook: herald send failed:", err)
		return 0
	}
	if err := saveState(p.StateDir, in.SessionID, d.Next); err != nil {
		fmt.Fprintln(stderr, "herald-hook: save state:", err)
	}
	return 0
}

// decide applies the consent policy, event taxonomy and per-session debounce.
// It is pure so the send/no-send logic is unit-testable without the harness.
func decide(in hookInput, prev state, p policy, now time.Time) decision {
	if !p.Enabled {
		return decision{Reason: "disabled", Next: prev}
	}
	status, terminal := terminalOutcome(in.Event)
	if !terminal {
		return decision{Reason: "non_terminal_event", Next: prev}
	}
	if !p.Events[in.Event] {
		return decision{Reason: "event_not_enabled", Next: prev}
	}
	if debounced(in, prev, p, now) {
		return decision{Reason: "debounced", Next: prev}
	}
	seq := 1
	if prev.SessionID == in.SessionID {
		seq = prev.Seq + 1
	}
	next := state{
		SessionID:  in.SessionID,
		LastStatus: status,
		LastEvent:  in.Event,
		LastAt:     now.Unix(),
		Seq:        seq,
	}
	return decision{
		Send:       true,
		Status:     status,
		Occurrence: occurrenceID(in.SessionID, seq),
		Reason:     "terminal_outcome",
		Next:       next,
	}
}

// debounced collapses rapid duplicate hooks for the same terminal transition.
// A different event or status is a new transition and always sends.
func debounced(in hookInput, prev state, p policy, now time.Time) bool {
	if prev.SessionID != in.SessionID {
		return false
	}
	if prev.LastStatus != "" && prev.LastStatus != mustStatus(in.Event) {
		return false
	}
	return prev.LastEvent == in.Event &&
		now.Sub(time.Unix(prev.LastAt, 0)) < p.Debounce
}

// terminalOutcome maps a terminal hook to a Herald status. Progress hooks
// (PreToolUse, PostToolUse, UserPromptSubmit, SessionStart, SessionEnd,
// PreCompact) return ok=false and never send.
func terminalOutcome(event string) (string, bool) {
	switch event {
	case eventStop, eventSubagentStop:
		return statusSuccess, true
	case eventNotification:
		return statusBlocked, true
	default:
		return "", false
	}
}

func mustStatus(event string) string {
	status, _ := terminalOutcome(event)
	return status
}

func policyFromEnv(getenv func(string) string) policy {
	p := policy{
		Enabled:  truthy(getenv("HERALD_HOOK_ENABLED")),
		Events:   eventSet(getenv("HERALD_HOOK_EVENTS")),
		Debounce: defaultDebounce,
		Agent:    firstNonEmpty(getenv("HERALD_HOOK_AGENT"), getenv("HERALD_AGENT"), defaultAgent),
		StateDir: getenv("HERALD_HOOK_STATE_DIR"),
		Bin:      firstNonEmpty(getenv("HERALD_HOOK_BIN"), defaultBin),
	}
	if v := strings.TrimSpace(getenv("HERALD_HOOK_DEBOUNCE_SECONDS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			p.Debounce = time.Duration(n) * time.Second
		}
	}
	if p.StateDir == "" {
		p.StateDir = defaultStateDir(getenv)
	}
	return p
}

// eventSet defaults to the terminal events (Stop, Notification) and drops names
// that are not terminal outcomes.
func eventSet(raw string) map[string]bool {
	if strings.TrimSpace(raw) == "" {
		return map[string]bool{eventStop: true, eventNotification: true}
	}
	set := map[string]bool{}
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if _, ok := terminalOutcome(name); ok {
			set[name] = true
		}
	}
	return set
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func defaultStateDir(getenv func(string) string) string {
	if dir := getenv("XDG_CACHE_HOME"); dir != "" {
		return filepath.Join(dir, "herald", "hooks")
	}
	if dir, err := os.UserCacheDir(); err == nil && dir != "" {
		return filepath.Join(dir, "herald", "hooks")
	}
	return filepath.Join(os.TempDir(), "herald-hooks")
}

// occurrenceID is stable for one transition so a retried hook (or a concurrent
// duplicate) is deduplicated server-side on (tenant, project, occurrence).
func occurrenceID(sessionID string, seq int) string {
	return "claude-code-" + stateKey(sessionID) + "-" + strconv.Itoa(seq)
}

func statePath(dir, sessionID string) string {
	return filepath.Join(dir, stateKey(sessionID)+".json")
}

// stateKey makes a filesystem-safe, bounded key from a session id.
func stateKey(sessionID string) string {
	var b strings.Builder
	for _, r := range sessionID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		}
		if b.Len() >= maxKeyLen {
			break
		}
	}
	if b.Len() == 0 {
		return "default"
	}
	return b.String()
}

func loadState(dir, sessionID string) (state, error) {
	data, err := os.ReadFile(statePath(dir, sessionID))
	if errors.Is(err, os.ErrNotExist) {
		return state{}, nil
	}
	if err != nil {
		return state{}, err
	}
	var s state
	if err := json.Unmarshal(data, &s); err != nil {
		return state{}, err
	}
	return s, nil
}

// saveState writes atomically so a crash cannot leave a half-written record.
func saveState(dir, sessionID string, s state) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, statePath(dir, sessionID))
}

func sendArgs(in hookInput, d decision, p policy) []string {
	return []string{
		"send",
		"--status", d.Status,
		"--title", titleFor(in, d.Status),
		"--body", bodyFor(in, d.Status),
		"--occurrence-id", d.Occurrence,
		"--agent", p.Agent,
	}
}

func titleFor(in hookInput, status string) string {
	label := "finished"
	switch {
	case status == statusBlocked:
		label = "blocked"
	case in.Event == eventSubagentStop:
		label = "subagent finished"
	}
	if project := filepath.Base(in.CWD); project != "" && project != "." && project != string(filepath.Separator) {
		label += ": " + project
	}
	return truncate("Claude Code "+label, maxTitle)
}

func bodyFor(in hookInput, status string) string {
	var b strings.Builder
	if status == statusBlocked && in.Message != "" {
		fmt.Fprintf(&b, "Blocked: %s\n", in.Message)
	} else {
		fmt.Fprintf(&b, "Claude Code reported %s.\n", status)
	}
	if in.SessionID != "" {
		fmt.Fprintf(&b, "Session: %s\n", in.SessionID)
	}
	if in.CWD != "" {
		fmt.Fprintf(&b, "Directory: %s\n", in.CWD)
	}
	return truncate(b.String(), maxBody)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// execSender is the real sender: it runs herald send and discards its output
// (the gateway event id is not needed here). Delivery is best-effort.
func execSender(ctx context.Context, bin string, args []string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}
