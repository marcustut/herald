---
name: herald
description: Notify the owner by email that work is done, using the herald CLI. Invoke ONLY when the user has explicitly asked to be told about the outcome of a task — e.g. "notify me when you're done", "email me when it finishes", "let me know if it fails", "ping me when this is done", "send me a report when done". Do not invoke on ordinary task completion.
---

# Notify the owner when the work is done

Use this only when the user explicitly asked to be notified. Then send **exactly
one** event, with `herald send`, when the work reaches a terminal state.

## Consent

Herald's policy is **opt-in, ask first**: send only when the user has explicitly
asked to be notified. If they did not ask, send nothing — a configured token is
not consent. This is the same policy the `send_event` MCP tool and the lifecycle
hooks follow; the canonical version is `docs/consent.md` in the herald repository.

- Send only on a **user-requested terminal outcome** — `success`, `failure` or
  `blocked`. One event per outcome.
- Never notify on every turn, progress step or tool call.
- Notify at each checkpoint **only** if the user explicitly asked for that.

## When to send

- The user asked to be notified **and** the requested work is finished. Send
  once, at the end — not after every step, not as a progress update.
- If the work spans several turns, send only on the turn it actually completes.
- If the work fails, or you get blocked and cannot proceed, still send (the user
  asked to be told), with `--status failure` or `--status blocked`.
- If the user asked to be notified at each checkpoint ("notify me after each
  step"), send at each checkpoint instead of only at the end.

## Command

```sh
herald send \
  --status success \
  --title "<short, specific summary>" \
  --body "<what changed and what to check>" \
  --link "PR=https://github.com/…"
```

Statuses: `success`, `failure`, `blocked`.

For a longer or multi-line body, pipe JSON instead:

```sh
herald send --json <<'JSON'
{ "status": "success", "title": "docs/deploy-host-github-key", "body": "…" }
JSON
```

## Rules

- Ask first. Opt-in is the default: no explicit request means no event.
- One event per terminal outcome. Do not stream progress.
- An event must never trigger another notification: do not send in response to
  receiving a Herald event, and do not re-notify an outcome you already sent.
- `--title` is required and should be short and specific.
- Never paste secrets. The gateway redacts secret-looking strings, but do not
  rely on it.
- Respect the project's quota. On `429 rate_limited`, back off and retry later
  rather than hammering.
- Each `herald send` is a new event. To retry the *same* logical event safely
  (e.g. the send may have timed out), add `--occurrence-id <stable-id>`: repeating
  it returns the original event instead of creating a duplicate. Different events
  must use different ids (or none).
- Best-effort: if `herald send` fails, mention it briefly and carry on. Never
  let notifying block or fail the actual work.

## Setup

The project provides `herald.toml` (project slug + gateway URL). The bearer
token comes from `HERALD_TOKEN` in the environment, or from the `0600`
credential file that `herald login` writes (process env wins). `HERALD_AGENT`
and `HERALD_HOST` optionally identify the agent and the machine. Never choose
the recipient yourself — the gateway pins it from the token.
