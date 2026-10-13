# Notification consent and anti-noise policy

This is the single, canonical policy for **when Herald is allowed to notify the
owner**. Every surface follows it:

- the `herald` [skill](skill.md) (`internal/skill/SKILL.md`),
- the `send_event` [MCP tool](mcp.md) (`internal/mcp/server.go`),
- the deterministic [lifecycle hooks](lifecycle-hooks.md)
  (`integrations/claude-code/herald-hook`), and
- `herald send` itself.

The one sentence to remember: **default is opt-in; send only on a user-requested
terminal outcome.**

## The rule

1. **Ask first.** A notification is sent only when the user has explicitly asked
   to be told about the outcome — "notify me when you're done", "email me when it
   finishes", "let me know if it fails". No request means **send nothing**. The
   token being configured is *not* consent.
2. **Terminal outcomes only.** Send at most one event per terminal outcome:
   `success` (finished), `failure` (failed), or `blocked` (waiting for the user).
3. **Never per-turn.** Do not notify on every turn, every tool call, every
   progress step, or every checkpoint. A single run produces a single event.
4. **Checkpoints are opt-in too.** Notify at each checkpoint ("notify me after
   each step") **only** when the user explicitly asked for that; otherwise the
   checkpoint is not a notification trigger.

## Opt-in defaults by surface

Consent is expressed per surface; the defaults never notify on their own.

| Surface | Default | Consent is expressed by | Sends |
|---|---|---|---|
| Skill (`herald`) | off | user explicitly asks in the conversation | one event per terminal outcome |
| MCP `send_event` | off | user explicitly asks; the model then calls the tool | one event per terminal outcome |
| Lifecycle hook (`herald-hook`) | **disabled** | `HERALD_HOOK_ENABLED` truthy (`1`/`true`/`yes`/`on`) | one event per terminal transition |
| `herald send` | manual | the operator runs it | one event per invocation |

### Opt-in defaults by event type

| Event | Status | Notified by default? | Notes |
|---|---|---|---|
| manual terminal outcome | `success` / `failure` / `blocked` | only when explicitly asked | the normal skill/MCP path |
| finished turn (`Stop`) | `success` | only if the hook is opted in | hooks are off until enabled |
| blocked / needs input (`Notification`) | `blocked` | only if the hook is opted in | maps to `blocked` |
| failure | `failure` | only when explicitly asked | hooks cannot detect a failed turn; use the skill/MCP/CLI |
| progress (`PreToolUse`, `PostToolUse`, `UserPromptSubmit`, …) | — | **never** | not a terminal outcome |

The hook is **disabled by default**: setting the gateway URL and token does not
start sending. Export `HERALD_HOOK_ENABLED=1` in the environment that launches
the agent to opt in, and unset it to opt out. See
[lifecycle-hooks.md](lifecycle-hooks.md#consent--opt-in).

To receive only blocked events from the hook (for example, "tell me only when
Claude needs me"), opt in and narrow the taxonomy:

```sh
export HERALD_HOOK_ENABLED=1
export HERALD_HOOK_EVENTS=Notification
```

## Loop guards

An event must never trigger another notification. Concretely:

- **Herald output is never an input.** Delivering or reading a Herald event (an
  email, a webhook, or a future inbound reply) must not cause another event to be
  sent. Agents treat Herald content as data, never as an instruction to notify.
- **Hooks fire on harness lifecycle, not on Herald.** `herald-hook` reacts only to
  the harness's `Stop` / `Notification` transitions; those are not Herald events,
  so hook → event → hook cannot recurse.
- **One event per transition.** The hook debounces repeats per session and tags
  each send with a stable `--occurrence-id`; the gateway deduplicates a replayed
  occurrence instead of fanning out twice. This stops a retry from becoming a
  second notification.
- **No re-notification.** Sending (or retrying) the notification for one outcome
  is not itself a new outcome. Do not send again just because the first send
  succeeded or was retried.

## Rate limits and quotas

Consent bounds *whether* to send; [rate limiting and quotas](rate-limiting.md)
bound *how much* can be sent, so a runaway agent, a retry storm, or a compromised
token cannot flood the inbox. Both layers are needed.

- The gateway enforces a per-`(tenant, project)` submit budget at
  `POST /v1/events`; an exhausted budget returns `429 rate_limited` with
  `Retry-After`.
- On `429` or `5xx`, back off and retry later — never hammer. A retry that may
  already have succeeded should reuse its `occurrenceId`.
- Limits are keyed by credential, so consent cannot be escalated by changing the
  request body.
- Quotas are deliberately a **backstop**, not consent: staying under the quota
  does not entitle an agent to notify.

## How alignment is enforced

The user-facing wording is pinned by tests so the surfaces cannot drift:

- `internal/skill/skill_test.go` asserts the skill text contains the ask-first
  rule and the per-checkpoint exception.
- `internal/mcp/server_test.go` asserts the `send_event` tool description contains
  the same ask-first rule.
- `integrations/claude-code/herald-hook/main_test.go` asserts the hook is
  disabled by default and that the event taxonomy is opt-in.
