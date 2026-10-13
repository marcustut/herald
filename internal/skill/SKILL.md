---
name: herald-notify
description: Notify the owner by email that work is done, using the herald CLI. Invoke ONLY when the user has explicitly asked to be told about the outcome of a task — e.g. "notify me when you're done", "email me when it finishes", "let me know if it fails", "ping me when this is done", "send me a report when done". Do not invoke on ordinary task completion.
---

# Notify the owner when the work is done

Use this only when the user explicitly asked to be notified. Then send **exactly
one** event, with `herald send`, when the work reaches a terminal state.

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

- One event per terminal outcome. Do not stream progress.
- `--title` is required and should be short and specific.
- Never paste secrets. The gateway redacts secret-looking strings, but do not
  rely on it.
- Retries are safe: the CLI derives a stable idempotency key from the content,
  so sending the same event twice does not duplicate it.
- Best-effort: if `herald send` fails, mention it briefly and carry on. Never
  let notifying block or fail the actual work.

## Setup

The project provides `herald.toml` (project slug + gateway URL) and sets
`HERALD_TOKEN` in the environment. `HERALD_AGENT` and `HERALD_HOST` optionally
identify the agent and the machine. Never choose the recipient yourself — the
gateway pins it from the token.
