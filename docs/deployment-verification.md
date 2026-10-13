# Deployment verification (issue #12)

This records the end-to-end verification of the **live** Herald deployment, so a
future re-run has a concrete procedure and a baseline to compare against.

Observed on the production host through SSH on **2026-10-13**, times in UTC
(`date -u`). The procedure is host-neutral; the results below are the actual
ones, not a template.

- Host: `server`, NixOS `26.05.20260914.c3eea5b` (Yarara)
- Public endpoint: `https://herald.marcustut.me/api`
- Deployed flake input: `herald` pinned to
  `16178d7024f0f7ebd60a097d3645868552dfb7d8` (`refs/heads/main`, 9 commits
  behind the revision this doc was written against)
- Config repo: `marcustut/nixos` (`hosts/server/herald.nix`,
  `hosts/server/herald/gateway.toml`)
- System generation: `146` (the rollback target is `145`)

## Procedure

Each step is independent; run them in this order.

### 1. Services and startup

```sh
systemctl list-units --all 'herald*'
systemctl status herald --no-pager
systemctl status herald-dispatcher --no-pager
systemctl is-enabled herald herald-dispatcher
```

### 2. Public HTTPS, TLS and health

From an external host:

```sh
curl -fsS https://herald.marcustut.me/api/healthz
curl -fsS https://herald.marcustut.me/api/readyz
echo | openssl s_client -connect herald.marcustut.me:443 \
  -servername herald.marcustut.me 2>/dev/null \
  | openssl x509 -noout -subject -issuer -dates -ext subjectAltName
```

### 3. Auth is enforced

```sh
curl -sS -o /dev/null -w '%{http_code}\n' -X POST \
  https://herald.marcustut.me/api/v1/events \
  -H 'Content-Type: application/json' -d '{"status":"success","title":"x","agent":"y"}'
```

### 4. CLI submission → DB → dispatcher → mailbox

Issue a throwaway project token, submit over the public HTTPS endpoint, watch it
land, then revoke the token.

```sh
DBURL='postgres:///herald?host=/run/postgresql'
TOKEN=$(sudo -u herald herald token create --database-url "$DBURL" --project axis)
HASH=$(printf %s "$TOKEN" | sha256sum | cut -d' ' -f1)   # for targeted revoke

# run from a directory the service user can read (not root's home); add
# --config /path/to/absent.toml if needed so the default herald.toml is not stat'd
cd /tmp
HERALD_GATEWAY=https://herald.marcustut.me/api HERALD_PROJECT=axis HERALD_TOKEN=$TOKEN \
  herald send --status success --title "deploy verification issue-12" \
    --agent herald-cli --idempotency-key deploy-verify-YYYYMMDDThhmmssZ

# after ~5s: event and delivery rows
sudo -u postgres psql -d herald -c \
  "SELECT e.id,e.title,d.state,d.attempts,d.updated_at \
   FROM events e JOIN deliveries d ON d.event_id=e.id \
   WHERE e.id='<id>'"

# SMTP handoff
journalctl -u stalwart.service --no-pager --since '-60s' | grep -i delivery

# targeted revoke (RevokeProject picks an arbitrary project token; do not use it)
sudo -u postgres psql -d herald -c \
  "UPDATE credentials SET revoked_at=now() WHERE token_hash='$HASH' AND revoked_at IS NULL;"
```

Repeat the same `herald send` with the same idempotency key: it must return the
same event id and add no rows.

### 5. Deprecated alias

```sh
curl -sS -w '\n%{http_code}\n' -X POST \
  https://herald.marcustut.me/api/v1/reports \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"status":"success","title":"legacy alias verify","agent":"herald-cli","idempotencyKey":"…"}'
```

### 6. Sender identity

```sh
# principal email must equal the From in gateway.toml, and the server must match it
grep -n -A4 'name = "herald"' /nix/store/*stalwart*/stalwart.toml
grep -n 'must-match-sender' /nix/store/*stalwart*/stalwart.toml
cat /nix/store/*gateway.toml
```

### 7. Database ownership and permissions

```sh
sudo -u postgres psql -d herald -c '\dt'
sudo -u postgres psql -d herald -c \
  "SELECT table_name,grantee,string_agg(privilege_type,',' ORDER BY privilege_type) \
   FROM information_schema.role_table_grants \
   WHERE table_schema='public' AND table_name IN ('events','deliveries','credentials') \
   GROUP BY 1,2"
sudo -u postgres psql -Atc \
  "SELECT rolname,rolsuper,rolcreatedb,rolcanlogin FROM pg_roles WHERE rolname='herald'"
```

### 8. Environment secrecy

```sh
stat -c '%a %U:%G %n' /run/secrets/herald-env
systemctl show herald -p EnvironmentFiles
# confirm the committed config carries no credentials
grep -in 'password\|secret\|token' /nix/store/*gateway.toml || echo 'no secrets in gateway.toml'
```

### 9. Controlled restart

```sh
systemctl restart herald herald-dispatcher
sleep 3
systemctl is-active herald herald-dispatcher
curl -fsS http://127.0.0.1:8787/readyz
```

## Results (2026-10-13, UTC)

| # | Check | Observed |
|---|---|---|
| 1 | `herald.service` | active (running); `enabled`; started 05:18:13Z |
| 1 | `herald-dispatcher.service` | active (running); `enabled`; started 05:18:13Z |
| 2 | `GET /api/healthz` | `200` `ok` |
| 2 | `GET /api/readyz` | `200` `ready` |
| 2 | TLS certificate | `CN=herald.marcustut.me`, issuer Let's Encrypt, SAN `DNS:herald.marcustut.me`, valid 2026-10-13T02:37:14Z–2027-01-11T02:37:13Z (fresh ACME issuance) |
| 3 | unauthenticated `POST /v1/events` and `/v1/reports` | `401` `{"error":{"code":"unauthenticated","message":"missing bearer token"}}` |
| 4 | CLI `send` over public HTTPS at 06:26:59Z | `202`, event `evt_676eabbc24af47ffe3bb537cce51afcc` stored (`axis`, `herald-cli`, `success`) |
| 4 | delivery row | `state=delivered`, `attempts=1`, at 06:26:59.749Z |
| 4 | Stalwart | `Queued message submission` → `Delivery completed` at 06:26:59Z |
| 4 | idempotent retry (same key) | same event id, still 1 event / 1 delivery |
| 5 | `POST /v1/reports` at 06:27:25Z | `202`, event `evt_9fe46e50f68b7aa3b439a727d238cbc6`, delivered 06:27:26.301Z |
| 6 | sender identity | principal `herald` email `herald@marcustut.me`; `smtp-submission` on `:587` with `must-match-sender = true`; gateway `from = "herald@marcustut.me"` — all match |
| 7 | database | `herald` db owned by `herald`; `events`/`deliveries`/`credentials` owned by `herald`; role `herald` not superuser, not createdb, login enabled; connected over the unix socket (peer auth) |
| 8 | environment | `/run/secrets/herald-env` is `400 root:root`; the unit's `EnvironmentFiles=/run/secrets/herald-env`; `gateway.toml` (store path) contains no secret |
| 9 | restart at 06:27:16Z | both units active again within the second (PIDs 106222 / 106226), `readyz=200`; `NRestarts=0` (clean restart, no failures) |

Historical evidence in the same database: the first smoke event
(`2026-10-13T02:01:51Z`) went to `dead` after 8 attempts because the sink dialled
`127.0.0.1` and the cert has no IP SAN — the fix was
`server_name = "mail.marcustut.me"`, after which deliveries succeed on the first
attempt (02:05Z, 03:35Z, 05:18Z, and the runs above). This is why the example
config documents `server_name`.

## Not verified

- **No redeploy of the current `main`.** Production runs revision `16178d7`
  (9 commits behind). Confirmed by the deployed binary's CLI (no
  `--occurrence-id`; `--idempotency-key` instead) and by the absence of the
  `schema_migrations` table. The versioned-migrations feature (#18) and
  `occurrenceId` are therefore present in the repo but **not exercised in
  production**; the first deploy of current `main` will create
  `schema_migrations` and record the `baseline` step.
- **Response body of the delivered mail.** Delivery was confirmed through the
  dispatcher state and Stalwart's `Delivery completed` log; the message body was
  not read over IMAP (that needs the mailbox password, a secret, which was not
  retrieved).
- **Rate limiting / quotas.** Not exercised; the limits feature is newer than the
  deployed revision.
- **Restore from backup.** The backup/restore smoke test in
  [migrations.md](migrations.md) was not run against this database.

## Side effects of this verification

- Two verification events were added to the audit log:
  `evt_676eabbc24af47ffe3bb537cce51afcc` (`deploy verification issue-12`) and
  `evt_9fe46e50f68b7aa3b439a727d238cbc6` (`legacy alias verify issue-12`); both
  delivered normally.
- Four throwaway project tokens were issued and **revoked** immediately after
  use (two attempts aborted before submitting); rows remain with `revoked_at`
  set. The two original active credentials are untouched. No other production
  state changed.
