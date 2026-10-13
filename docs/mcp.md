# MCP server

`herald mcp` runs a minimal [Model Context Protocol](https://modelcontextprotocol.io)
server over **stdio**, exposing one tool — `send_event` — for harnesses that
prefer a structured tool to the shell-based [skill](skill.md).

- [Setup](#setup)
- [Harness configuration](#harness-configuration)
- [Tools](#tools)
- [Protocol](#protocol)
- [Common errors](#common-errors)
- [Concurrency and retries](#concurrency-and-retries)
- [Security boundaries](#security-boundaries)

## Setup

```sh
herald mcp [--config herald.toml] [--agent NAME] [--host NAME]
```

The server reads the same configuration as `herald send`:

- `project` and `gateway` from `herald.toml` (or `HERALD_PROJECT` /
  `HERALD_GATEWAY`),
- the bearer token from `HERALD_TOKEN`,
- default `agent` / `host` from `--agent` / `--host`, then `HERALD_AGENT` /
  `HERALD_HOST`.

If no gateway URL is configured, the process exits with an error before reading
stdin. The server speaks JSON-RPC 2.0 on stdin/stdout, one JSON object per line;
diagnostics go to stderr. It never logs the token or event bodies.

## Harness configuration

Any MCP client that can launch a stdio server works. Register a command that
runs `herald mcp` with the gateway and token in its environment. The exact file
and schema differ per harness; the generic stdio shape is:

```json
{
  "mcpServers": {
    "herald": {
      "command": "herald",
      "args": ["mcp"],
      "env": {
        "HERALD_GATEWAY": "https://herald.example.com",
        "HERALD_TOKEN": "herald_axis_<secret>",
        "HERALD_AGENT": "opencode"
      }
    }
  }
}
```

Notes:

- Prefer an absolute `command` path, since a harness may launch the server with
  a different `PATH` than your shell.
- Supply the token through the harness's environment/secret mechanism, not as a
  literal in a shared config file.
- Claude Code reads project-level `herald.toml` from the workspace; other
  harnesses may need `--config /abs/path/to/herald.toml` or `HERALD_PROJECT`.
  For the deterministic, harness-driven alternative see
  [docs/lifecycle-hooks.md](lifecycle-hooks.md).

## Tools

One tool is advertised by `tools/list`.

### `send_event`

Send a finished-work event to the owner. Use it **only when the user has
explicitly asked** to be notified about the outcome — the default is opt-in, so
if they did not ask, do not send. Send at most one event per terminal outcome
(`success`, `failure` or `blocked`); never notify on every turn, progress step or
checkpoint unless the user explicitly asked for each checkpoint. This mirrors the
[skill](skill.md) and the lifecycle hooks; see the canonical
[consent policy](consent.md).

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `status` | string | yes | `success`, `failure` or `blocked`. |
| `title` | string | yes | Short summary of the work. |
| `body` | string | no | Details; free text. |
| `agent` | string | no | Agent name; defaults to the configured agent. |
| `host` | string | no | Machine the agent ran on (sent as `context.host`). |
| `links` | array of `{label,url}` | no | Labelled references. |
| `occurrenceId` | string | no | Stable id for one logical event; resending it is a safe retry. |
| `idempotencyKey` | string | no | Explicit retry key; alternative to `occurrenceId`. |

The server fills `project` from its configuration and `agent`/`host` from the
argument or its defaults before calling `POST /v1/events`. A successful call
returns a text result:

```json
{"content": [{"type": "text", "text": "event accepted: evt_01JABCXYZ"}]}
```

A failure returns the same shape with `"isError": true`; see
[Common errors](#common-errors).

## Protocol

The server implements the subset of MCP a tool-only server needs:

| Method | Behaviour |
|---|---|
| `initialize` | Echoes the client's `protocolVersion` (default `2024-11-05`); advertises `tools`. |
| `notifications/initialized` | Acknowledged; no response (it is a notification). |
| `notifications/cancelled` | Ignored; no response. |
| `ping` | Returns an empty result. |
| `tools/list` | Returns the `send_event` tool. |
| `tools/call` | Runs `send_event`. |

An unknown **request** method returns JSON-RPC error `-32601` (method not
found); an unknown **notification** method (no `id`) is ignored. Malformed input
lines and malformed tool arguments are reported as a tool error, not a crash.

## Common errors

Gateway failures are surfaced to the model as an `isError` text result, so the
agent can react:

| Message | Cause and fix |
|---|---|
| `gateway 401 (unauthenticated): …` | `HERALD_TOKEN` missing, wrong, revoked or out of scope. Issue a token with `herald token create`. |
| `gateway 422 (validation_failed): …` | A field is invalid (commonly `title`, or a bad `status`). Check the message. |
| `gateway 429 (rate_limited): …` | The project's submit budget is exhausted. Back off and retry later. |
| `gateway 503 (unavailable): …` | The gateway or its database is unavailable. Retry with backoff. |
| `unknown tool: <name>` | The harness called a tool that does not exist. |
| `invalid arguments: …` | The tool arguments did not match the schema. |

## Concurrency and retries

- The server processes one line at a time: a tool call is a single synchronous
  HTTP request, and concurrent calls from one session are serialised.
- To retry a send that may already have succeeded, reuse the same
  `occurrenceId`. The gateway returns the original event id and does not fan out
  twice.
- Do not retry `401`/`422`; fix the token or arguments. Retry `429` only after
  the rate-limit window, and `5xx`/network errors with backoff.

## Security boundaries

- The tool arguments are model-generated and untrusted. The MCP server only
  forwards them as event text; it never executes instructions found in them.
- The recipient is pinned server-side from the token and `gateway.toml`; neither
  the tool nor the model can choose or redirect a destination.
- The bearer token lives in the server process environment and is never logged,
  echoed in a result, or sent anywhere except the configured gateway.
- The gateway redacts secret-looking strings at ingest, but the model should
  still avoid pasting secrets into an event.
