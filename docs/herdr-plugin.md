# herdr lifecycle plugin

A [herdr](https://herdr.dev) plugin that notifies Herald when a coding-agent pane
becomes **blocked** (needs a human) or **done** (finished), so you get an email
without an agent having to remember to send one.

It is event-driven: herdr calls the hook on every `pane.agent_status_changed`
event, the hook filters to the two terminal transitions, and it shells out to
`herald send` with an occurrence id derived from the pane and the live herdr
sequence.

## Verified against herdr 0.9.3

Before building the plugin we linked a scratch probe plugin and observed the real
events on a running herdr (0.9.3, protocol 22).

Probe manifest:

```toml
id = "scratch.event-probe"
name = "Event Probe"
version = "0.1.0"
min_herdr_version = "0.7.0"
platforms = ["linux", "macos"]

[[events]]
on = "pane.agent_status_changed"
command = ["sh", "probe.sh"]
```

The probe appended `$HERDR_PLUGIN_EVENT_JSON` to a log. Driving a pane with
`herdr pane report-agent` and a real `opencode` agent (told to reply `pong`)
produced:

```text
{"event":"pane_agent_status_changed","data":{"type":"pane_agent_status_changed","pane_id":"wS:p1","workspace_id":"wS","agent_status":"working","agent":"probe"}}
{"event":"pane_agent_status_changed","data":{"type":"pane_agent_status_changed","pane_id":"wS:p1","workspace_id":"wS","agent_status":"blocked","agent":"probe"}}
{"event":"pane_agent_status_changed","data":{"type":"pane_agent_status_changed","pane_id":"wS:p1","workspace_id":"wS","agent_status":"idle","agent":"probe"}}
{"event":"pane_agent_status_changed","data":{"type":"pane_agent_status_changed","pane_id":"wS:p1","workspace_id":"wS","agent_status":"done","agent":"opencode"}}
```

Findings that shaped the plugin:

- The manifest matches `on = "pane.agent_status_changed"` (dots), while the wire
  payload's own `event`/`type` is `pane_agent_status_changed` (underscores). The
  hook ignores the envelope's name and reads `data`.
- Every transition fires, including routine `working`, `idle` and `unknown`
  churn, so the hook **must** filter — herdr does not. `done` is emitted by a
  real agent finishing a turn; `herdr pane report-agent` only accepts
  `idle|working|blocked|unknown`, so there is no way to fake `done` with it.
- **The event carries no sequence.** Its keys are `pane_id`, `workspace_id`,
  `agent_status`, `agent` (and optionally `display_agent`, `state_labels`,
  `title`). The occurrence discriminator is not on the wire, so the hook reads
  the live sequence from `herdr agent get <pane_id>`, which returns
  `state_change_seq` and `completion_seq` (a `done`/`idle` transition sets both).

End-to-end check of the finished plugin (with a stub `herald` so nothing was
sent) captured:

```text
send --status success --title "Agent done: live-probe" \
  --body "herdr done in wT/wT:p1 at /home/marcustut/herald" \
  --agent opencode --occurrence-id herdr:wT:p1:done:345

send --status blocked --title "Agent blocked: live-probe" \
  --body "herdr blocked in wT/wT:p1 at /home/marcustut/herald" \
  --agent live-probe --occurrence-id herdr:wT:p1:blocked:346
```

## Install

From a checkout of this repository:

```sh
herdr plugin link /path/to/herald/integrations/herdr
# or, once published: herdr plugin install marcustut/herald/integrations/herdr
herdr plugin list
```

`plugin link` keeps the working tree as the plugin root, so a `git pull` is
enough to update it. `plugin install` copies a GitHub checkout instead.

## Configure (scoped token)

The plugin process runs as the herdr user and **cannot read a pane's
environment** (the pane's `HERALD_TOKEN` is not visible to it). It reads its
credentials from its own config directory instead:

```sh
CFG=$(herdr plugin config-dir herald.notify)
cat > "$CFG/herald.env" <<'EOF'
HERALD_GATEWAY=https://herald.example.com
HERALD_PROJECT=herdr
HERALD_TOKEN=heraldacct_<scoped-token>
EOF
chmod 600 "$CFG/herald.env"
```

- Use a **scoped** token (`herald token create --scoped ...`, shape
  `heraldacct_<secret>`), because the plugin sends for whichever pane changed and
  therefore must name the target `HERALD_PROJECT`.
- `HERALD_GATEWAY`, `HERALD_PROJECT` and `HERALD_TOKEN` are required; the hook
  exports them and calls `herald send`, which reads them from the environment.
- Optional: `HERALD_BIN=/path/to/herald` if `herald` is not on the plugin's
  `PATH` (it falls back to `herald`).
- `herald.env` is dotenv-style and is sourced by the hook, so it must be
  shell-safe and user-owned (`chmod 600`). It is never committed.

The plugin deliberately does **not** store the token in the plugin root, which
for a GitHub install is a managed, replaceable checkout.

## What it emits

Only two transitions produce a notification:

| herdr status | Herald `--status` | title |
|---|---|---|
| `blocked` | `blocked` | `Agent blocked: <name>` |
| `done` | `success` | `Agent done: <name>` |

Everything else (`idle`, `working`, `unknown`) is ignored. The body carries the
`workspace/pane`, the agent name and its working directory; `--agent` is the
slug-sanitized agent name (herdr agent kinds/names may contain characters Herald
rejects).

## Idempotency

The occurrence id is:

```text
herdr:<pane_id>:<status>:<seq>
```

- For `done`, `<seq>` is the pane's `completion_seq`; for `blocked` it is
  `state_change_seq`. Both come from `herdr agent get <pane_id>` at hook time,
  because the event itself carries no sequence.
- pane ids are never reused by herdr, and the sequence advances on every
  transition, so each real transition maps to a distinct occurrence id.
- Repeating an occurrence id returns the original event id server-side and does
  not fan out twice (see the README's Idempotency section), so a duplicate
  delivery or a plugin restart cannot double-send.
- If `herdr agent get` does not answer (the agent was already released), the hook
  falls back to a checksum of the event JSON. That still dedupes byte-identical
  duplicate deliveries, but two genuine, identical transitions on the same pane
  would collapse. This is a safety net, not the normal path.

## Failure isolation

The hook is deliberately boring so it can never wedge herdr:

- It always exits `0`, whether it sent, skipped, or failed.
- `herald send` runs under `timeout 10`, so an unreachable gateway cannot block
  the herdr event loop.
- A missing config directory, missing `herald.env`, missing token, or a failing
  `herald send` is logged to stderr (visible via
  `herdr plugin log list --plugin herald.notify`) and dropped — no retries, no
  crash. Herald's own queue is the durable retry layer once a request is
  accepted.

## Tests

The hook is unit-tested without a running herdr or gateway. The test harness
stubs `HERDR_BIN_PATH` and `HERALD_BIN` and feeds sample event JSON from the
verification above:

```sh
go test ./integrations/herdr/
```

Covered: only `done`/`blocked` send; `done`→`success` and `blocked`→`blocked`;
occurrence ids come from `completion_seq`/`state_change_seq`; duplicate
deliveries reuse the id; distinct completions get distinct ids; concurrent
duplicates share one id; `idle`/`working`/`unknown` and malformed events send
nothing; missing config/token sends nothing; a failing `herald` still exits `0`;
the `agent get` fallback key is stable; agent ids are slug-sanitized. A manifest
test pins the hook to `pane.agent_status_changed`.

### Manual verification

```sh
# 1. link and configure as above
herdr plugin link /path/to/herald/integrations/herdr
CFG=$(herdr plugin config-dir herald.notify); $EDITOR "$CFG/herald.env"

# 2. watch the hook log
herdr plugin log list --plugin herald.notify

# 3. drive a real completion: start an agent in a scratch pane and let it finish
herdr workspace create --label herald-test
herdr agent start probe --kind opencode --pane <pane-from-create>
herdr agent prompt probe "Reply with exactly: pong" --wait --until done
# → expect one email, and a log line mentioning herdr:<pane>:done:<seq>

# 4. confirm the event payloads yourself (optional, as in the probe above):
#    a temp plugin whose hook appends $HERDR_PLUGIN_EVENT_JSON to a file
```

## Uninstall

```sh
herdr plugin unlink herald.notify          # leaves files alone
# or: herdr plugin uninstall herald.notify # removes a GitHub-managed checkout

rm -rf "$(herdr plugin config-dir herald.notify)"   # drop the token config
```
