# Self-hosting Herald with Docker Compose

This guide stands up the whole stack — the HTTP API, the delivery-queue
dispatcher, and PostgreSQL — with one command. It is the container equivalent of
the NixOS path (see `nix/module.nix`); both are supported.

```
agents / CLI / MCP  ──POST /v1/events──►  herald (API)  ──►  postgres (queue)
                                              │                    │
                                              └── claim ──► herald-dispatcher ──► email / webhook
```

The API and the dispatcher are **separate processes by design**: scale or
restart the queue consumer without touching the API.

## Prerequisites

- Docker Engine with the Compose plugin (`docker compose version`).
- A reachable SMTP submission host if you want email delivery.
- A hostname/port to publish the API on (put a TLS-terminating proxy in front;
  the container serves plain HTTP).

## Quickstart

1. **Get the files** (this repository):

   ```sh
   git clone https://github.com/marcustut/herald
   cd herald
   ```

2. **Create the two local config files** (both are gitignored — never commit
   them):

   ```sh
   cp .env.example .env
   cp gateway.docker.example.toml gateway.toml
   ```

3. **Edit `.env`** — set at least `HERALD_POSTGRES_PASSWORD` and a matching
   `HERALD_DATABASE_URL`, plus the SMTP credentials:

   ```dotenv
   HERALD_DATABASE_URL=postgres://herald:change-me@postgres:5432/herald?sslmode=disable
   HERALD_POSTGRES_PASSWORD=change-me
   HERALD_SMTP_USERNAME=herald
   HERALD_SMTP_PASSWORD=...
   ```

   The host part of `HERALD_DATABASE_URL` is the Compose service name
   (`postgres`), not `localhost`.

4. **Edit `gateway.toml`** — the `[smtp]` host, `from`, and each `[sinks.*]`
   recipient. Keep `addr = "0.0.0.0:8787"` (binding `127.0.0.1` inside the
   container makes the port unreachable).

5. **Start the stack**:

   ```sh
   docker compose up -d
   docker compose ps        # herald should become (healthy)
   ```

   Migrations run automatically on startup; the database schema is created on
   first boot.

6. **Create a token** for a project (matches a `[projects.<name>]` in
   `gateway.toml`):

   ```sh
   docker compose run --rm herald token create --project axis
   # → herald_axis_<secret>   (shown once; store it in that repo's secret store)
   ```

7. **Submit an event** (`agent` and `idempotencyKey` are required; the CLI/MCP
   fill them in for you):

   ```sh
   curl -sS -X POST http://localhost:8787/v1/events \
     -H "Authorization: Bearer herald_axis_<secret>" \
     -H "Content-Type: application/json" \
     -d '{"project":"axis","status":"success","title":"hello from docker","agent":"claude-code","idempotencyKey":"demo-1"}'
   # → 202 {"id":"..."}
   ```

   The dispatcher delivers it to the project's sinks within a second or two
   (`herald-dispatcher` polls). `/v1/reports` remains a deprecated alias of
   `/v1/events`.

## Required and optional environment

| Variable | Required | Purpose |
|---|---|---|
| `HERALD_DATABASE_URL` | yes | PostgreSQL DSN used by the API, dispatcher, and `token` commands. |
| `HERALD_POSTGRES_PASSWORD` | yes | Password for the bundled PostgreSQL. |
| `HERALD_SMTP_USERNAME` / `HERALD_SMTP_PASSWORD` | for email sinks | SMTP submission credentials. Never placed in `gateway.toml`. |
| `HERALD_POSTGRES_USER` / `HERALD_POSTGRES_DB` | no | Default to `herald`. |
| `HERALD_PORT` | no | Host port for the API. Defaults to `8787`. |
| `HERALD_DISPATCH_WORKERS` | no | Concurrent dispatcher claim loops. Defaults to `2`. |

`HERALD_POSTGRES_PASSWORD` and `HERALD_DATABASE_URL` must agree. Compose fails
fast (refuses to start) if a required variable is unset — there are **no
insecure default secrets**, and the API never runs with the in-memory `--dev`
stores in this setup.

## Health and readiness

- **`GET /healthz`** — liveness. Always `200 ok` while the process is up. Used
  by the Compose healthcheck.
- **`GET /readyz`** — readiness. Returns `200 ready`, or `503 unavailable` when
  the service is not ready. Point load balancers and orchestration probes here.

```sh
curl -fsS http://localhost:8787/healthz
curl -fsS http://localhost:8787/readyz
```

The dispatcher has no HTTP surface; watch it with `docker compose logs`.

## Operations

### Upgrade

```sh
git pull
docker compose build --pull      # rebuild the image from the new source
docker compose up -d             # recreate containers; migrations run on start
```

Migrations are idempotent and forward-only; still take a backup first (below).
Always rebuild rather than relying on an old local image.

### Backup

Logical backup (portable, recommended):

```sh
docker compose exec -T postgres pg_dump -U herald herald | gzip > "herald-$(date +%F).sql.gz"
```

Raw volume backup (fast, same PostgreSQL major version only):

```sh
docker run --rm \
  -v herald_herald-db:/data:ro \
  -v "$PWD":/backup \
  alpine tar czf /backup/herald-db.tgz -C /data .
```

### Restore

```sh
# into a running, empty database:
gunzip -c herald-2026-01-01.sql.gz | docker compose exec -T postgres psql -U herald -d herald
```

The named volume is `herald_herald-db` (Compose project `herald`). It is the
only stateful volume; the API and dispatcher are stateless and can be recreated
freely.

## Troubleshooting

**First, check interpolation and the resolved config:**

```sh
docker compose config          # renders env interpolation; errors on unset vars
docker compose logs -f herald herald-dispatcher postgres
```

- **`docker compose up` exits with `set HERALD_... in .env`** — a required
  variable is missing. Fill in `.env`.
- **`herald` container restarts / `unhealthy`** — check logs. A common cause is
  `gateway.toml` failing validation (e.g. a project references an undefined
  sink, or the file is missing). The error is logged verbatim.
- **API unreachable on the published port** — `server.addr` in `gateway.toml`
  must be `0.0.0.0:8787`; `127.0.0.1` is only reachable inside the container.
- **`401 unauthenticated`** — the bearer token's project does not match the
  `project` in the request, or the token was revoked. Recreate it with
  `docker compose run --rm herald token create --project <name>`.
- **`503 unavailable` / `/readyz` not ready** — the API cannot reach
  PostgreSQL. Verify `HERALD_DATABASE_URL` matches `HERALD_POSTGRES_*` and that
  `postgres` is `(healthy)`. Migrations run against this same DSN.
- **Events accepted but no email** — the dispatcher never got a working SMTP
  path. A container cannot reach the host's `127.0.0.1`; use a real hostname
  (`host.docker.internal`, an IP, or another Compose service). Ensure
  `HERALD_SMTP_USERNAME`/`HERALD_SMTP_PASSWORD` are set and that
  `[smtp] server_name` matches the certificate the server presents (needed when
  dialling by IP). Read the failure in
  `docker compose logs herald-dispatcher`; a delivery is retried with backoff and
  becomes `dead` after 8 attempts.
- **Webhook sink fails TLS** — outbound TLS needs the headers in
  `ca-certificates`; already included in the image.
- **Reset everything (destroys data)** — `docker compose down -v`.

## Security notes

- The API is plain HTTP. Terminate TLS at a reverse proxy (Caddy, nginx,
  Traefik) and do not publish `8787` to the public internet.
- Secrets live only in `.env`/the environment; `.env`, `gateway.toml`, and `.env.*`
  are gitignored, and the image is built without them (`.dockerignore`).
- The image runs as a non-root user with no build toolchain.
