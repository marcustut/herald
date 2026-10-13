# CLI

`herald` is one binary: it runs the gateway (`serve`, `dispatch`), submits
events (`send`), manages credentials (`token`), inspects the delivery queue
(`delivery`), and exposes the agent surfaces (`mcp`, `skill`).

- [Build and install](#build-and-install)
- [Configuration](#configuration)
- [Commands](#commands)
  - [`herald login`](#herald-login)
  - [`herald send`](#herald-send)
  - [`herald token`](#herald-token)
  - [`herald serve`](#herald-serve)
  - [`herald dispatch`](#herald-dispatch)
  - [`herald delivery`](#herald-delivery)
  - [`herald mcp` / `herald skill`](#herald-mcp--herald-skill)
- [Exit codes](#exit-codes)
- [Common gateway errors](#common-gateway-errors)
- [Concurrency and retries](#concurrency-and-retries)
- [Security boundaries](#security-boundaries)

## Build and install

Requires the Go toolchain pinned in [`mise.toml`](../mise.toml):

```sh
mise install
go build ./...                       # verify
go build -o "$HOME/.local/bin/herald" ./cmd/herald
```

Release binaries and container images are described in
[docs/releasing.md](releasing.md) and
[docs/self-hosting-docker.md](self-hosting-docker.md).

## Configuration

Inside a project, `herald send` reads `herald.toml` (a non-secret file meant to
be committed):

```toml
# herald.toml
project = "axis"
gateway = "https://herald.example.com"
```

| Variable | Used by | Meaning |
|---|---|---|
| `HERALD_TOKEN` | `send`, `mcp` | Bearer token. **Never commit it.** |
| `HERALD_GATEWAY` | `send`, `mcp` | Gateway origin; overrides `herald.toml`. |
| `HERALD_PROJECT` | `send`, `mcp` | Project slug; overrides `herald.toml`. |
| `HERALD_CONFIG_DIR` | `login`, `send`, `mcp` | Directory holding the credential file (default `$XDG_CONFIG_HOME/herald`). |
| `HERALD_AGENT` | `send`, `mcp` | Default `--agent`. |
| `HERALD_HOST` | `send`, `mcp` | Default `--host` (sent as `context.host`). |
| `HERALD_DATABASE_URL` | `serve`, `dispatch`, `token`, `delivery` | PostgreSQL DSN. |
| `HERALD_TENANT` | `token` | Tenant id (default `owner`). |
| `HERALD_SMTP_USERNAME` / `HERALD_SMTP_PASSWORD` | `serve`, `dispatch` | Email sink credentials. |

Precedence for `send`/`mcp`: `herald.toml` supplies `project` and `gateway`;
`HERALD_GATEWAY` and `HERALD_PROJECT` override them. The token resolves as
**process env > credential file > `herald.toml`** (`herald.toml` never holds a
token):

1. `HERALD_TOKEN` in the environment.
2. The user credential file written by [`herald login`](#herald-login), at
   `$HERALD_CONFIG_DIR/credentials` or `$XDG_CONFIG_HOME/herald/credentials`
   (on Linux, `~/.config/herald/credentials`). It is written `0600`; it is a
   single bearer token, so keep it on a machine you trust.
3. Nothing else.

If the gateway URL is unset, the command exits with an error before any network
call. `--config` defaults to `herald.toml` in the current directory; a missing
file is not an error.

### Noninteractive CI

CI and other noninteractive callers must get the token from the environment and
never prompt. `HERALD_TOKEN` is the supported path:

```sh
# GitHub Actions (the repo's committed herald.toml supplies project + gateway)
env:
  HERALD_TOKEN: ${{ secrets.HERALD_TOKEN }}
```

`herald login --token "$HERALD_TOKEN" --gateway "$HERALD_GATEWAY" --project axis`
also stores a token without prompting, if you prefer to write the credential
file in a setup step. Either way, a missing gateway or token is an error, not a
prompt.

## Commands

`herald --help` prints the full usage. Exit codes are listed
[below](#exit-codes).

### `herald login`

Onboard a machine: store a bearer token where every shell and agent session can
find it, and record the gateway and project. It reuses the existing project
token (#30); it never invents a new credential type.

```sh
# Hosted: sign in with a magic link and mint a project token
herald login --gateway https://cloud.example.com --project axis --email you@example.com

# Self-host: paste a token the operator issued
herald login --gateway http://localhost:8787 --project axis

# Noninteractive: store a token you already have, never prompt
herald login --gateway "$HERALD_GATEWAY" --project axis --token "$HERALD_TOKEN"

# Verify end to end after storing
herald login --gateway https://cloud.example.com --project axis --test
```

| Flag | Meaning |
|---|---|
| `--gateway` | Gateway origin (defaults to `HERALD_GATEWAY` or `herald.toml`). |
| `--project` | Project slug (defaults to `HERALD_PROJECT`, `herald.toml`, then the git repo name). |
| `--token` | Store this token and do not prompt (defaults to `HERALD_TOKEN`). |
| `--email` | Hosted sign-in: mail a magic link to this address. |
| `--test` | Submit one test event after storing the token. |
| `--config` | `herald.toml` to write (default `herald.toml`). |

What it writes:

- The token to the **credential file**, mode `0600` (see
  [Configuration](#configuration)). It is never printed.
- `project` and `gateway` to `herald.toml` (no secret), so the project is
  configured for `send`, `mcp` and `skill`.

Token source precedence is `--token`, then `HERALD_TOKEN`, then `--email`, then
an interactive prompt for a pasted token. Only the prompt path reads stdin, so
`--token` / `HERALD_TOKEN` are safe for CI.

**Hosted magic-link flow.** With `--email`, the CLI requests a sign-in link,
prints an instruction to paste the link from the inbox, redeems it to a session,
creates the project if it does not exist, then mints a **single-project token**
(least privilege) and discards the session. It never opens a browser, so it
works on a headless host. Do not open the link in a browser first — the link is
single-use, and the CLI needs to redeem it. See
[docs/control-plane.md](control-plane.md#cli-login).

### `herald send`

Submit one event from the CLI — the same `POST /v1/events` path as
[docs/http-api.md](http-api.md).

```sh
herald send --status success --title "deploy finished"
herald send --status failure --title "build broke" --body "see CI log" \
  --link "CI=https://ci.example.com/runs/42"
```

| Flag | Meaning |
|---|---|
| `--status` | `success`, `failure` or `blocked` (required). |
| `--title` | Short summary (required). |
| `--body` | Details. |
| `--link` | `LABEL=URL`, repeatable. |
| `--agent` | Agent name; defaults to `HERALD_AGENT`, else `herald`. |
| `--host` | Machine; defaults to `HERALD_HOST`. |
| `--occurrence-id` | Stable id so a retry reuses the same event. |
| `--idempotency-key` | Explicit retry key (alternative to `--occurrence-id`). |
| `--json` | Read a JSON event from stdin. |
| `--config` | Path to `herald.toml` (default `herald.toml`). |

On success the event id is printed to stdout and the exit code is `0`. Pipe
JSON for a longer body:

```sh
echo '{"status":"failure","title":"build broke","agent":"ci"}' | herald send --json
```

With `--json`, `status`, `title`, `body` and `links` come from the JSON.
`--occurrence-id` / `--idempotency-key` override their JSON counterparts, and
`agent` falls back to `--agent` / `HERALD_AGENT` when the JSON omits it.
`agent` defaults to `herald` when none of those provides one.

For retrying a send that may already have succeeded, pass a stable
`--occurrence-id`: repeating it returns the original event id instead of
creating a duplicate.

### `herald token`

Issue and revoke credentials against PostgreSQL — there is no HTTP provisioning
endpoint.

```sh
# A token fixed to one project:
herald token create --database-url "$HERALD_DATABASE_URL" --project axis
# → herald_axis_<secret>      (shown once)

# A scoped token that may send for several projects:
herald token create --database-url "$HERALD_DATABASE_URL" --scoped axis,herald
# → heraldacct_<secret>       (shown once)

herald token revoke --database-url "$HERALD_DATABASE_URL" --project axis
```

| Flag | Meaning |
|---|---|
| `--database-url` | DSN (or `HERALD_DATABASE_URL`). |
| `--project` | Project slug for a project token. |
| `--scoped` | Comma-separated projects for a scoped token. |
| `--tenant` | Tenant id (default `HERALD_TENANT`, else `owner`). |

The raw token is printed **once** and stored only as a SHA-256 hash. Put it in a
secret store, never in a committed file. `--project` and `--scoped` are mutually
exclusive.

### `herald serve`

Run the HTTP API. In production the dispatcher runs separately (see below).

```sh
herald serve --config gateway.toml --database-url "$HERALD_DATABASE_URL"
```

| Flag | Meaning |
|---|---|
| `--config` | Path to `gateway.toml` (default `gateway.toml`). |
| `--database-url` | DSN (or `HERALD_DATABASE_URL`). |
| `--dev` | In-memory stores; no persistence (development only). |
| `--dispatch` | Also run the dispatcher in-process (development only). |
| `--issue-token` | Issue a project token at startup and print it. |
| `--interval` | Dispatcher poll interval with `--dispatch` (default `1s`). |

### `herald dispatch`

Run the delivery-queue consumer as its own process, so you can scale it
independently of the API.

```sh
herald dispatch --config gateway.toml --database-url "$HERALD_DATABASE_URL" --workers 2
```

| Flag | Default | Meaning |
|---|---|---|
| `--workers` | `1` | Concurrent claim loops. |
| `--batch` | `20` | Deliveries claimed per loop. |
| `--interval` | `1s` | Poll interval. |
| `--lease` | `1m` | Claim lease duration. |
| `--max-attempts` | `8` | Attempts before a delivery becomes `dead`. |

### `herald delivery`

Inspect and operate the durable queue from the SQL-backed store.

```sh
herald delivery list --state dead --limit 20
herald delivery show <delivery-id>
herald delivery retry <delivery-id>
herald delivery requeue --state dead
herald delivery reclaim
```

| Command | Purpose |
|---|---|
| `list` | List rows, filtered by `--state`, `--event`, `--sink`, newest first (`--limit`, default `100`; `0` = all). |
| `show` | Print one delivery's state, attempts, lease and last error. |
| `retry` | Re-drive one row. A delivered or currently-leased row is left alone. |
| `requeue` | Bulk re-drive `dead` (default) or `pending` rows, optionally by `--event` / `--sink`. |
| `reclaim` | Return expired `inflight` leases to `pending` (a crashed worker's rows). |

All take `--database-url` / `HERALD_DATABASE_URL`. See
[docs/runbook-deliveries.md](runbook-deliveries.md).

### `herald mcp` / `herald skill`

Agent integration surfaces; see [docs/mcp.md](mcp.md) and [docs/skill.md](skill.md).

## Exit codes

| Code | Meaning |
|---|---|
| `0` | Success. |
| `1` | Runtime error (bad config, gateway error, database error). |
| `2` | Usage error: unknown command, bad or missing flag. |

Gateway errors from `send`/`mcp` are printed to stderr as
`error: gateway <status> (<code>): <message>`.

## Common gateway errors

| Code | Status | Cause and fix |
|---|---|---|
| `unauthenticated` | 401 | `HERALD_TOKEN` missing, wrong, revoked, or not scoped to `project`. Run `herald login` (or issue/rotate a token with `herald token create`). |
| `validation_failed` | 422 | A field is missing or out of range (commonly `title`). The message names the offending `field`. |
| `rate_limited` | 429 | The project's submit budget is exhausted. Honour `Retry-After` and back off; see [docs/rate-limiting.md](rate-limiting.md). |
| `unsupported_media_type` | 415 | The HTTP client sent a non-JSON content type (rare from the CLI). |
| `payload_too_large` | 413 | The event body exceeds the server cap (default 256 KiB). |
| `unavailable` | 503 | The gateway or its database is unavailable. Retry with backoff. |

## Concurrency and retries

- `herald send` is a single synchronous HTTP request and is safe to run
  concurrently from many processes; the gateway serialises writes in PostgreSQL.
- A `202` means accepted; delivery is asynchronous. To retry safely after a
  timeout, reuse the same `--occurrence-id`; the gateway returns the original
  event id and does not fan out twice.
- Do **not** retry a `401` or `422` blindly — fix the token or the payload first.
  A `429` should be retried only after `Retry-After`; a `5xx`/network error may
  be retried with backoff.

## Security boundaries

- `HERALD_TOKEN` is a secret. Keep it in the environment, a secret manager, or
  the `0600` credential file written by `herald login` (one bearer token, kept
  outside the repository); never in `herald.toml`, a repository, or a log line.
  The CLI never prints it.
- The recipient is pinned server-side from the token and `gateway.toml`. The CLI
  cannot choose where a notification goes.
- Event text is untrusted. Never paste secrets; the gateway redacts
  secret-looking strings at ingest, but that is a safety net, not a guarantee.
