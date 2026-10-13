# Quickstart: first notification in under 10 minutes

Stand up a gateway, point a project at it, and receive a notification. This is
the fast path; [self-hosting-docker.md](self-hosting-docker.md) covers
production operations (TLS, backups, upgrades) in depth.

**Time budget:** gateway ≈3 min · token ≈2 min · project init ≈1 min · send
≈1 min · agent wiring ≈2 min.

You need Docker with Compose, and `herald` on your PATH (`go build ./...`, then
use `./herald` or `go install ./cmd/herald`).

## 1. Start the gateway (≈3 min)

```sh
git clone https://github.com/marcustut/herald
cd herald
cp .env.example .env
cp gateway.docker.example.toml gateway.toml
```

Edit `.env` so `HERALD_DATABASE_URL` and `HERALD_POSTGRES_PASSWORD` agree
(`change-me` is fine for a local run), and fill the SMTP settings if you want
email — see [smtp-gmail.md](smtp-gmail.md) if you need credentials.
Then edit `gateway.toml`: set a `[sinks.*]` recipient and a
`[projects.<name>]` for your project. Start it:

```sh
docker compose up -d
docker compose ps          # wait for herald (healthy)
curl -fsS http://localhost:8787/readyz
```

Migrations run automatically on first boot.

## 2. Create a token (≈2 min)

A token is per project and shown once. Issue it on the gateway host:

```sh
docker compose run --rm herald token create --project axis
# → herald_axis_<secret>
```

Copy that value into your secret store and export it wherever you run agents —
never commit it:

```sh
export HERALD_TOKEN=herald_axis_<secret>
```

If someone else operates the gateway, ask them for the token; they run the same
command. The token is what the gateway uses to pin the recipient — agents cannot
redirect notifications.

**Hosted Herald Cloud instead?** Skip the manual token and sign in from the CLI.
It mints a project token for you and stores it in a `0600` credential file, so
you do not have to export anything:

```sh
herald login --gateway https://cloud.example.com --project axis --email you@example.com
```

`herald send` then finds the token automatically. See
[cloud-auth.md](cloud-auth.md#cli-login).

## 3. Initialize the project (≈1 min)

In the repository you want notifications from:

```sh
herald init --gateway http://localhost:8787
# → wrote herald.toml (project "my-repo" → http://localhost:8787)
```

`herald init` detects the project name from git (the `origin` remote's
repository name), writes a **secret-free** `herald.toml`, and prints the token
and skill/MCP setup steps. Commit `herald.toml`. Override anything explicitly:

```sh
herald init --project axis --gateway https://herald.example.com   # explicit
herald init --force                                               # replace existing
```

`herald.toml` holds only `project` and `gateway`; the token comes from
`HERALD_TOKEN` or the `0600` credential file written by `herald login`, and is
never written to `herald.toml`. Use `--no-guidance` for quiet scripting.

## 4. Send a test event (≈1 min)

```sh
herald send --status success --title "hello from herald"
# → evt_...
```

The dispatcher delivers it to the project's sinks within a second or two. If
email is configured, check your inbox. Prefer raw HTTP? The endpoint is
`POST /v1/events` with a Bearer token (`/v1/reports` is a deprecated alias):

```sh
curl -sS -X POST http://localhost:8787/v1/events \
  -H "Authorization: Bearer $HERALD_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"project":"axis","status":"success","title":"hello","agent":"me"}'
```

## 5. Let an agent notify you (≈2 min)

Install the skill so an agent emails you **only when you ask** ("notify me when
you're done"), not on every task:

```sh
herald skill > ~/.claude/skills/herald/SKILL.md
# or place it wherever your harness reads skills/AGENTS.md
```

Harnesses that prefer a structured tool can register the MCP server
(`herald mcp`), which exposes a `send_event` tool. Run it in a directory with a
`herald.toml` and `HERALD_TOKEN` set. Ask the agent to "notify me when this is
done" and it will call Herald.

## Troubleshooting

| Symptom | Fix |
|---|---|
| `HERALD_TOKEN is missing` | Export the token from step 2, or store it with `herald login`. |
| `gateway URL is missing` | Run `herald init --gateway <url>` or set `HERALD_GATEWAY`. |
| `herald.toml already exists` | Review it, then re-run with `--force`. |
| `401 unauthenticated` | The token's project does not match `project` in `herald.toml`, or it was revoked. Re-issue it. |
| `503 unavailable` / `/readyz` not ready | The API cannot reach PostgreSQL; check `.env` and `docker compose ps`. |
| Events accepted but no email | The dispatcher has no working SMTP path ([smtp-gmail.md](smtp-gmail.md)); `docker compose logs herald-dispatcher`. |

Full Docker operations, security notes, and non-Docker (NixOS) hosting live in
[self-hosting-docker.md](self-hosting-docker.md).
