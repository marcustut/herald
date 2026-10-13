# Coding-agent lifecycle hooks

Herald normally learns that work finished because the *model* chose to call the
`herald-notify` skill or the `send_event` MCP tool. That depends on the model
remembering an instruction. This integration is the deterministic alternative:
the **harness** invokes a hook when the agent reaches a terminal state, so the
notification does not rely on the model at all.

The reference implementation ships for **Claude Code** in
[`integrations/claude-code/`](../integrations/claude-code/). The same event
taxonomy applies to any harness that can run a command on a lifecycle event.

## What it does

`herald-hook` reads the hook payload on stdin, decides whether this is a
terminal outcome worth notifying, and — only if opted in — runs **one**
`herald send`, using the same CLI/MCP path everything else uses:

```
Claude Code Stop / Notification hook
        │  JSON on stdin
        ▼
   herald-hook ── decide ── one `herald send --occurrence-id …` ──► POST /v1/events
        │
   per-session debounce state
```

It never sends on progress hooks, and it is **off until you opt in**.

## Install (Claude Code)

1. Install the `herald` CLI on `PATH` (see the repository README) and build the
   hook:

   ```sh
   go build -o "$HOME/.local/bin/herald-hook" ./integrations/claude-code/herald-hook
   ```

2. Export the same environment the `herald` CLI already uses. Put the token in a
   secret store or your shell profile — **never commit it**:

   ```sh
   export HERALD_GATEWAY="https://herald.example.com"
   export HERALD_TOKEN="herald_<project>_<secret>"
   export HERALD_PROJECT="axis"        # optional if herald.toml is in the cwd
   ```

3. Register the hook in `~/.claude/settings.json` (or
   `settings.local.json`). See
   [`settings.example.json`](../integrations/claude-code/settings.example.json):

   ```json
   {
     "hooks": {
       "Stop":         [{ "hooks": [{ "type": "command", "command": "$HOME/.local/bin/herald-hook" }] }],
       "Notification": [{ "hooks": [{ "type": "command", "command": "$HOME/.local/bin/herald-hook" }] }]
     }
   }
   ```

4. **Opt in** (see below), then start a new Claude Code session.

`herald send` also resolves `HERALD_AGENT` / `HERALD_HOST`; the hook always tags
events with `--agent claude-code` so you can tell lifecycle events from skill
sends.

## Consent / opt-in

The hook is **disabled by default**. It does nothing until `HERALD_HOOK_ENABLED`
is truthy (`1`, `true`, `yes`, `on`). The hook tries to use a settings `env`
block if your harness has one, but exporting the variable from the environment
that launches the agent is the portable option.

```sh
export HERALD_HOOK_ENABLED=1
```

| Variable | Default | Purpose |
|---|---|---|
| `HERALD_HOOK_ENABLED` | *(unset — disabled)* | Master consent switch. Anything else is a no-op. |
| `HERALD_HOOK_EVENTS` | `Stop,Notification` | Comma-separated terminal hooks to act on. Lets you add `SubagentStop`. |
| `HERALD_HOOK_DEBOUNCE_SECONDS` | `30` | Window in which an identical repeated hook for the same session is collapsed. |
| `HERALD_HOOK_AGENT` | `HERALD_AGENT`, else `claude-code` | Agent name attached to the event. |
| `HERALD_HOOK_BIN` | `herald` | Path to the `herald` binary. |
| `HERALD_HOOK_STATE_DIR` | `$XDG_CACHE_HOME/herald/hooks` (or `~/.cache/herald/hooks`) | Where per-session debounce state lives. |

Consent is deliberately per environment, not per repository: setting the token
alone does not start sending. To opt a single project in, export the variables
in that project's dev shell; to opt back out, unset `HERALD_HOOK_ENABLED`.

## Event taxonomy

Each hook maps to one of the three Herald statuses — the same closed set the
skill, the `send_event` MCP tool, and `herald send` accept:

| Claude Code hook | Herald status | Sent by default | Meaning |
|---|---|---|---|
| `Stop` | `success` | yes | the main agent finished its turn |
| `Notification` | `blocked` | yes | Claude is waiting for input or permission |
| `SubagentStop` | `success` | no | a subagent finished — noisy, opt in with `HERALD_HOOK_EVENTS` |
| `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `PreCompact`, `SessionStart`, `SessionEnd` | — | never | progress events, not a terminal outcome |

Lifecycle hooks cannot distinguish a successful turn from a failed one — Claude
Code fires `Stop` either way. The hook therefore reports `success` on `Stop` and
`blocked` on `Notification`; for a deliberate `failure`, keep using the skill,
the MCP `send_event` tool, or `herald send --status failure`. This is why the
taxonomy stays aligned with, but does not replace, those surfaces.

## Debounce and idempotency

Hooks can fire more than once for the same transition (harness retries, a
duplicate notification, concurrent invocations). Two mechanisms keep it to **one
logical event per transition**:

- **Debounce (local).** After a successful send the hook records, per session,
  the status, hook name and timestamp. An identical hook within
  `HERALD_HOOK_DEBOUNCE_SECONDS` is skipped. A *different* status is a new
  transition and sends immediately.
- **Occurrence id (server).** Every send carries
  `--occurrence-id claude-code-<session>-<n>`, with `n` incremented only when a
  new transition is accepted. A retried send reuses the same id, and the gateway
  returns the original event instead of fanning out twice (occurrence ids are
  scoped to tenant + project). A send that fails does not consume a sequence
  number, so the retry collides with the original rather than duplicating it.

State is written atomically under `HERALD_HOOK_STATE_DIR`. The store is only
consulted for debounce; it contains session ids and timestamps, no secrets, and
can be deleted at any time.

## Limitations

- **Best-effort.** The hook always exits `0`; a failure to notify never blocks
  or fails the agent. Errors go to stderr only.
- **One event per completion, not per step.** There is no progress stream.
- **No failure signal.** See the taxonomy note above.
- **`Stop` fires per turn.** If you leave the hook enabled, you are asking to be
  told every time the agent stops responding — which is exactly the
  "notify me when you're done" intent, but it is not a per-task one-shot. Opt
  out with `HERALD_HOOK_ENABLED` when you don't want it.
- **Single-machine state.** Debounce state is local; running the same session on
  two hosts can produce two events. The occurrence id still deduplicates a
  retry of the *same* transition.
- **The hook payload is untrusted.** Only `hook_event_name`, `session_id`,
  `cwd` and `message` are read, and they are only forwarded as event text; no
  instruction in the payload is executed.

## Other harnesses

- **Codex** — `~/.codex/config.toml` supports a `notify` program invoked with a
  JSON argument when a turn completes (`agent-turn-complete`). Codex's payload
  shape differs from Claude Code's, so point `notify` at a small wrapper that
  maps it to `herald send --status success --occurrence-id codex-<…>`, reusing
  the same consent and occurrence-id conventions. Codex does not expose a
  "blocked" event; that stays on the skill/MCP path.
- **OpenCode** — OpenCode loads plugins from `.opencode/plugin/`. A plugin can
  subscribe to `session.idle` (→ `success`) and `session.error` or
  `permission.updated` (→ `blocked`/`failure`) and shell out to `herald send`,
  reusing the same occurrence-id and consent conventions described here.

Both are one event per terminal transition; keep the occurrence-id scheme so
retries stay safe.

## Enable, disable, uninstall

- **Disable temporarily:** unset or set `HERALD_HOOK_ENABLED` to `0`.
- **Uninstall:** remove the `Stop`/`Notification` entries from
  `~/.claude/settings.json`, delete the `herald-hook` binary, and optionally
  `rm -rf "${HERALD_HOOK_STATE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/herald/hooks}"`.
  No server-side state needs cleaning up.

## Testing

```sh
go test ./integrations/claude-code/...
```

The tests cover the send/no-send decision table (disabled, progress hook,
opt-out event, debounce, status change, retry) and drive the **real** exec path
against a stub `herald` on `PATH`, asserting exactly one `herald send` per
transition. To smoke-test manually with a realistic payload:

```sh
export HERALD_HOOK_ENABLED=1 HERALD_HOOK_BIN="$(command -v herald)"
echo '{"hook_event_name":"Stop","session_id":"demo","cwd":"'"$PWD"'"}' \
  | ./herald-hook
```
