# Herald — decisions and blockers

Status: v1 implemented and tested (`gofmt`/`vet`/`golangci-lint` clean,
`go test -race` green; domain/app/httpapi covered). Not deployed.

This file records decisions made while building, what your answers resolved, and
what still needs you before deployment. See `README.md` for usage.

## Decisions locked (implemented)

- **Shape:** one Go binary `herald` with `serve`, `dispatch`, `send`, `token`,
  `mcp`. Layers: `internal/domain` (pure) → `internal/app` (use cases + ports) →
  `internal/adapters/*` → `cmd/herald`.
- **Queue:** PostgreSQL as queue + audit + credential store
  (`SELECT … FOR UPDATE SKIP LOCKED`). At-least-once, capped backoff, lease
  reclaim, dead-letter after 8 attempts or a permanent error.
- **Separate dispatcher:** the queue consumer runs as its own process
  (`herald dispatch --workers N`), so you can scale it independently of the API
  (`herald serve`). `serve --dispatch` exists for local dev only.
- **Credentials:** per-project tokens `herald_<project>_<secret>`; one scoped
  token `heraldacct_<secret>` for a multi-project sender (herdr plugin). SHA-256
  at rest; raw shown once. Agent/host are asserted by the trusted client, never
  trusted from the body.
- **Redaction at ingest** (the DB holds the redacted copy).
- **Token issuance** via `herald token …` against PostgreSQL (no HTTP
  provisioning endpoint).
- **HTTP API:** `POST /v1/events` → `202 {"id":…}` (`POST /v1/reports` is a deprecated alias); error codes
  `unauthenticated` 401 / `invalid_json` 400 / `unsupported_media_type` 415 /
  `payload_too_large` 413 / `validation_failed` 422 / `unavailable` 503.

## Resolved by your answers

### 1 & 2 — sending domain and SMTP

**Finding:** the box's Stalwart (`marcustut/nixos:hosts/server/stalwart.nix`)
has local domain `marcustut.me` only (`lookup.default.domain`), and routing is
`is_local_domain('*', rcpt_domain) → local`, else SES relay. Submission enforces
`must-match-sender = true`, so the authenticated principal's email must equal the
`From`.

- `mailer@herald.marcustut.me` is **not usable** as-is: `herald.marcustut.me`
  isn't a configured local domain, so it would route to the SES relay and need
  MX/DNS for the subdomain.
- **Use `herald@marcustut.me`**, which is local — but it needs a new principal
  in the in-memory directory.

Add to `local-keys`? No. Add the principal to
`hosts/server/stalwart.nix` → `directory."in-memory".principals`:

```nix
{
  class = "individual";
  name = "herald";
  secret = "%{file:/run/credentials/stalwart.service/herald-pw}%";
  email = [ "herald@marcustut.me" ];
}
```

and add the credential alongside the others:

```nix
credentials.herald-pw = config.sops.secrets.stalwart-herald-pw.path;
```

SMTP delivery uses the existing submission listener on `127.0.0.1:587`
(PLAIN auth, in-memory directory). `gateway.example.toml` now reflects
`from = "herald@marcustut.me"`.

### 4 — sops (matches the box)

The box uses sops-nix with dotenv files under `secrets/`. To add Herald:

1. `sops secrets/herald.env` (dotenv) with:
   ```
   HERALD_DATABASE_URL=postgres:///herald?host=/run/postgresql
   HERALD_SMTP_USERNAME=herald
   HERALD_SMTP_PASSWORD=<the stalwart "herald" principal secret>
   ```
   and `sops secrets/stalwart-herald-pw` (binary, same password) for Stalwart.
2. Extend `.sops.yaml` `path_regex` with `herald\.env|stalwart-herald-pw`.
3. In `hosts/server/sops.nix`, add a dotenv secret `herald-env` and a binary
   secret `stalwart-herald-pw`, then:
   ```nix
   services.herald.environmentFile = config.sops.secrets.herald-env.path;
   ```
   (systemd reads the EnvironmentFile as root, so `owner = "root"` `0400` is
   fine, matching the existing secrets.)

### 5 — PostgreSQL (local, dispatcher separate)

The box runs PostgreSQL 18 with `local all all peer` and a scram rule for
loopback. Herald uses peer auth over the socket:

```
HERALD_DATABASE_URL=postgres:///herald?host=/run/postgresql
```

The module creates the role + database (`ensureDatabases`/`ensureUsers`, DB name
defaults to the user so `ensureDBOwnership` applies). The dispatcher is a second
systemd unit (`herald-dispatcher`), `--workers` configurable.

## Deployment (still to do — item 3)

The `nix/module.nix` handles it: set `services.herald.domain` and it creates an
nginx vhost with `forceSSL`/`enableACME` proxying to `127.0.0.1:<port>`, exactly
like `axis.nix`. Host-side wiring:

1. Add the flake input: `herald.url = "git+ssh://git@github.com/marcustut/herald";`
2. Add `inputs.herald.nixosModules.default` to the server's `extraModules`.
3. `hosts/server/herald.nix`:
   ```nix
   { config, ... }:
   {
     services.herald = {
       enable = true;
       configFile = ./herald/gateway.toml;   # or a store path
       domain = "herald.marcustut.me";
       environmentFile = config.sops.secrets.herald-env.path;
     };
   }
   ```
4. Import `./herald.nix` in `hosts/server/configuration.nix`.
5. Build the package: replace the `vendorHash = pkgs.lib.fakeHash` placeholder in
   `flake.nix` with the hash `nix build` reports.
6. Open 443 (already open) and point DNS `herald.marcustut.me` at the box for the
   ACME cert.

## Recorded as GitHub issues

- **#1** herdr plugin: verify `pane.agent_status_changed` and build the plugin.
- **#2** Per-project rate limiting and quotas.
- **#3** Inbound replies (humans reply to agents by email).
- **#4** Immediate vs digest delivery.

## Design: inbound replies (issue #3)

Today a reply lands in `herald@marcustut.me`'s mailbox on Stalwart and is
ignored. To make Herald bidirectional:

1. Send from a real readable mailbox (`herald@marcustut.me`) — already the plan
   — with `Reply-To: herald+<event-id>@marcustut.me` (plus-addressing) and a
   stable `Message-ID: <event-id@herald.marcustut.me>`.
2. Read the mailbox over **IMAP** (`imaps :993`), as the `herald` principal.
   Note: the box has **no JMAP listener**, so IMAP (or Stalwart's admin API) is
   the transport — not JMAP. That's a small change from the earlier "JMAP is
   nicer" note.
3. Correlate a reply via `In-Reply-To`/`References` against the stored
   `Message-ID`, with the plus-address as fallback → resolve to the event's
   project + agent.
4. Deliver to the owning agent:
   - MCP tool `get_replies(eventId, since)` (poll-shaped), and/or
   - an optional webhook push when the agent can receive one ("realtime").
5. Security: inbound mail is untrusted — bound size/attachments, guard loops,
   and rely on the mailbox being local + single-owner (spoofing risk low but not
   zero).

The MCP "realtime" caveat: MCP is request/response; server→client notifications
are not reliably surfaced to the model, so agents realistically poll
`get_replies`. A webhook is the only true push, and only works if the agent runs
an HTTP endpoint.

## Open questions

- MCP distribution: how `herald mcp` gets registered in each harness's config.
- Token lifetime / rotation policy (none enforced yet).
- Whether replies become a new event type or a separate `replies` table.

## Known limits

- `net/smtp` is frozen and has no context support; a send isn't cancelled
  mid-flight by request cancellation.
- Dispatcher polls (default 1s); `LISTEN/NOTIFY` could cut latency if it matters.
- In-memory stores (`--dev`) are per-process; only useful with `serve --dispatch`.
- The `cli` package's `serve`/`dispatch` loops and the DB-backed `token`
  commands are the least-covered code; exercise `herald token create` against the
  real database before wiring agents.
