# herald

A small self-hosted gateway that lets coding agents (across all your projects)
email you a notification when they finish work. Agents POST over HTTPS; the gateway
authenticates the token, pins the destination, redacts secrets, then queues and
delivers to one or more sinks (email via SMTP, or an outbound webhook).

The recipient/sink is resolved server-side from the token — agents set no
headers and cannot redirect mail.

## Architecture

```
agents / CLI / MCP / herdr plugin
        │  HTTPS  POST /v1/events    (Bearer token)
        ▼
   herald serve ── authn · redact · fan-out ──► PostgreSQL (queue + audit)
        │                                            │
        │  claim (SKIP LOCKED)                       │
        ▼                                            ▼
   dispatcher ──► email sink (SMTP → Stalwart) · webhook sink (Slack, …)
```

- **No separate broker:** PostgreSQL is the durable queue (`SELECT … FOR UPDATE
  SKIP LOCKED`), the audit log, and the credential store.
- **At-least-once delivery** with per-attempt backoff, lease reclaim, and a dead
  state. Sinks receive an `Idempotency-Key`.

## Build & test

Requires Go (pinned in `mise.toml`). Everything is standard library except
`BurntSushi/toml` and `jackc/pgx`.

```sh
mise install              # Go + golangci-lint
go build ./...
go test ./...             # unit tests, 100% statement coverage
golangci-lint run ./...
```

PostgreSQL integration tests run when a database is available:

```sh
HERALD_TEST_DATABASE_URL='postgres://postgres@localhost:5432/herald_test?sslmode=disable' \
  go test ./internal/adapters/postgres/
```

## Run

```sh
# 1. create a token for a project (writes to PostgreSQL)
herald token create --project axis --database-url "$HERALD_DATABASE_URL"
# → herald_axis_<secret>   (shown once; put it in that repo's env/secret store)

# 2. run the gateway
herald serve --config gateway.toml --database-url "$HERALD_DATABASE_URL"

# 3. run the delivery dispatcher (a separate process in production, so you can
#    scale queue consumers independently of the API)
herald dispatch --config gateway.toml --database-url "$HERALD_DATABASE_URL" --workers 2
```

Emails are sent as `From: Herald <herald@marcustut.me>`. Set `[smtp] from_name`
in `gateway.toml` to change the display name (`""` sends the bare address).

From inside a project (with `herald.toml` and `HERALD_TOKEN` set):

```sh
herald send --status success --title "deploy finished" --link "PR=https://…"
echo '{"status":"failure","title":"build broke"}' | herald send --json
```

## Commands

| command | purpose |
|---|---|
| `herald serve` | run the HTTP API |
| `herald dispatch` | run the delivery-queue consumer (`--workers N`) |
| `herald send` | submit an event from the CLI (also used by hooks/CI) |
| `herald token create/revoke` | manage project and scoped credentials |
| `herald mcp` | MCP server (stdio) exposing a `send_event` tool |
| `herald skill` | print the agent SKILL.md for installation |

## Idempotency

Every submission is a **new event** by default, even when its text matches an
earlier one. To make a retry of one logical occurrence safe, send a stable
`occurrenceId`:

- HTTP: `{"occurrenceId":"deploy-2026-10-13", "status":"success", …}`
- CLI: `herald send --occurrence-id deploy-2026-10-13 --status success …`
- MCP: the `occurrenceId` argument of the `send_event` tool.

Repeating the same occurrence id (scoped to tenant + project) returns the
original event id and does not fan out twice. A raw `idempotencyKey` is still
accepted for backward compatibility and is likewise scoped to tenant + project,
so two projects can reuse a key without suppressing each other. Omit both and
each call creates a distinct event.

## Integration surfaces

- **Skill + CLI** — `herald skill` prints a SKILL.md that teaches an agent to
  notify **only when you explicitly ask** ("notify me when you're done", "email
  me when it finishes") — not on every task. Any harness that can run a shell
  can use it:
  ```sh
  herald skill > ~/.claude/skills/herald-notify/SKILL.md   # Claude Code
  # or place it wherever your harness reads skills / AGENTS.md
  ```
- **MCP** — `herald mcp` for harnesses that prefer a structured tool.
- **herdr plugin** — event-driven, hooks `pane.agent_status_changed` (issue #1).

See [DECISIONS-AND-BLOCKERS.md](DECISIONS-AND-BLOCKERS.md).
