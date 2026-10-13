# Agent skill

`herald skill` prints a ready-made [agent skill](../internal/skill/SKILL.md) —
`herald-notify` — that teaches a coding agent to notify the owner by email when
work finishes. It is the shell-based integration: any harness that can run a
command can install it.

- [When the skill fires](#when-the-skill-fires)
- [Install](#install)
- [Setup](#setup)
- [What the agent runs](#what-the-agent-runs)
- [Common errors](#common-errors)
- [Concurrency and retries](#concurrency-and-retries)
- [Security boundaries](#security-boundaries)
- [Testing](#testing)

## When the skill fires

The skill is deliberately conservative: it triggers **only when the user
explicitly asks to be told about the outcome** — "notify me when you're done",
"email me when it finishes", "let me know if it fails". It does not fire on
ordinary task completion, and it never streams progress.

This is the shared [notification consent policy](consent.md) — opt-in, ask first
— that the MCP tool and the lifecycle hooks follow too.

When it does fire, the agent sends **exactly one** event at the terminal state,
using `success`, `failure` or `blocked`. If the user asks to be notified at each
checkpoint, it sends at each checkpoint instead.

If you want a notification that does not depend on the model remembering, use a
deterministic [lifecycle hook](lifecycle-hooks.md) instead (or as well).

## Install

Print the skill and place it where the harness reads skills:

```sh
herald skill > ~/.claude/skills/herald-notify/SKILL.md   # Claude Code
```

For other harnesses, write the output to wherever that harness loads skills or
agent instructions from (for example an `AGENTS.md` include or a skills
directory). The content is plain Markdown with YAML front matter, so it is
portable. Confirm the command works first:

```sh
herald version
herald skill | head
```

## Setup

The skill runs `herald send`, so the project needs the same configuration as the
[CLI](cli.md):

- `herald.toml` in the project root with `project` and `gateway`:
  ```toml
  project = "axis"
  gateway = "https://herald.example.com"
  ```
- `HERALD_TOKEN` in the environment (never committed). Optionally
  `HERALD_AGENT` and `HERALD_HOST` identify the agent and the machine.
- `herald` on the agent's `PATH`, or referenced by absolute path.

```sh
export HERALD_TOKEN="herald_axis_<secret>"
export HERALD_AGENT="opencode"
```

## What the agent runs

The skill instructs the agent to send one event, for example:

```sh
herald send \
  --status success \
  --title "<short, specific summary>" \
  --body "<what changed and what to check>" \
  --link "PR=https://github.com/example/repo/pull/42"
```

or, for a multi-line body, pipe JSON:

```sh
herald send --json <<'JSON'
{ "status": "success", "title": "docs/deploy", "body": "…" }
JSON
```

Each call is a new event. To retry the *same* logical event safely, the skill
reuses `--occurrence-id <stable-id>`; repeating it returns the original event
instead of creating a duplicate.

## Common errors

The same gateway errors as the CLI; the skill tells the agent to mention a
failure briefly and carry on rather than block the work.

| Code | Status | Cause and fix |
|---|---|---|
| `unauthenticated` | 401 | `HERALD_TOKEN` missing, wrong or revoked. Issue a token with `herald token create`. |
| `validation_failed` | 422 | Commonly an empty or too-long `title`. The message names the field. |
| `rate_limited` | 429 | The project's submit budget is exhausted; back off and retry later. |
| `unavailable` | 503 | Gateway or database unavailable; retry with backoff. |

## Concurrency and retries

- `herald send` is a single synchronous request and is safe to run concurrently.
- A retried send should reuse its `--occurrence-id` so the gateway returns the
  original event rather than duplicating it.
- Notification is **best-effort**: if the send fails, the agent reports it and
  carries on — it must never block or fail the actual work.

## Security boundaries

- **Do not choose a recipient.** The gateway pins it from the token; the skill
  never lets the agent set one.
- **Never paste secrets.** The gateway redacts secret-looking strings at ingest,
  but the skill tells the agent not to rely on that.
- **Event text is untrusted.** Titles, bodies and links are data, never
  instructions, on every Herald surface.

## Testing

The skill content is pinned by a unit test:

```sh
go test ./internal/skill/
```
