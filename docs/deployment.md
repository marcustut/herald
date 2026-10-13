# Deploying Herald on NixOS

Herald ships a NixOS module (`nix/module.nix`, exposed as `nixosModules.default`)
that runs the HTTP API and the delivery dispatcher as two systemd services, a
local PostgreSQL role/database, and an optional nginx + ACME front door. The
module is host-neutral: everything site-specific (domain, secrets, SMTP
credentials, database URL) is configuration, not code.

This is the same path used by the live deployment at
`https://herald.marcustut.me/api`; the observed runtime state and end-to-end
checks are recorded in [deployment-verification.md](deployment-verification.md).

```
nginx :443 ──/api/──► herald serve 127.0.0.1:8787 ──► PostgreSQL (queue + audit)
                                                        │ claim (SKIP LOCKED)
                                                        ▼
                                            herald dispatch ──► SMTP / webhook
```

## Module options

| option | type | default | purpose |
|---|---|---|---|
| `services.herald.enable` | bool | — | enable the module |
| `services.herald.package` | package | — (required) | the `herald` package from the flake |
| `services.herald.configFile` | path | — (required) | the non-secret `gateway.toml` |
| `services.herald.environmentFile` | path? | `null` | systemd EnvironmentFile (sops) |
| `services.herald.user` / `.group` | str | `herald` | the services' system user/group |
| `services.herald.database.name` | str | `herald` | database name (defaults to the user so `ensureDBOwnership` applies) |
| `services.herald.database.create` | bool | `true` | create the role + database via `services.postgresql` |
| `services.herald.dispatcher.enable` | bool | `true` | run the queue consumer as a second unit |
| `services.herald.dispatcher.workers` | positive int | `1` | concurrent claim loops |
| `services.herald.dispatcher.interval` | str | `1s` | dispatcher poll interval |
| `services.herald.domain` | str? | `null` | public hostname; enables the nginx/ACME vhost |
| `services.herald.port` | port | `8787` | loopback port; must match `server.addr` |
| `services.herald.basePath` | str | `/api` | URL prefix nginx strips before proxying |

Both units run as an unprivileged user with `NoNewPrivileges`, `ProtectSystem =
"strict"`, `ProtectHome`, `PrivateTmp`, `PrivateDevices`, restricted address
families and `Restart = "on-failure"`.

## 1. Add the flake input

```nix
{
  inputs.herald = {
    url = "git+ssh://git@github.com/marcustut/herald";
    inputs.nixpkgs.follows = "nixpkgs";   # optional: reuse your nixpkgs
  };
}
```

Lock it and note the pinned revision, which is what a rollback returns to:

```sh
nix flake update herald            # older Nix: nix flake lock --update-input herald
```

## 2. Import the module

```nix
{
  imports = [
    inputs.herald.nixosModules.default
  ];

  services.herald = {
    enable = true;
    package = inputs.herald.packages.${pkgs.system}.herald;
    configFile = ./herald/gateway.toml;              # non-secret, committed
    environmentFile = config.sops.secrets.herald-env.path;
    domain = "herald.example.com";
    # port = 8787; basePath = "/api";               # defaults
  };
}
```

## 3. `gateway.toml` (non-secret only)

Never put a credential here. Start from `gateway.example.toml`:

```toml
[server]
addr = "127.0.0.1:8787"     # loopback; nginx is the only public surface
tenant = "owner"

[smtp]
addr = "127.0.0.1:587"      # your submission listener
from = "herald@example.com"
server_name = "mail.example.com"  # cert name; required when dialling by IP

[projects.axis]
sinks = ["email"]

[sinks.email]
kind = "email"
recipient = "you@example.com"
```

Keep `addr` on loopback. `server_name` must match the SMTP server's certificate
when the submission host is dialled by IP (otherwise delivery fails with
`x509: cannot validate certificate … doesn't contain any IP SANs`).

No submission server of your own? [smtp-gmail.md](smtp-gmail.md) shows how to get
credentials from a Gmail account and point Herald at `smtp.gmail.com:587`.

## 4. Secrets via sops-nix

The systemd services get exactly three variables from an `EnvironmentFile`; the
file is read by systemd as root, so `owner = "root"`, mode `0400` is fine.

```dotenv
HERALD_DATABASE_URL=postgres:///herald?host=/run/postgresql
HERALD_SMTP_USERNAME=herald
HERALD_SMTP_PASSWORD=<the submission principal's password>
```

```nix
sops.secrets.herald-env = {
  sopsFile = ./secrets/herald.env;
  format = "dotenv";
  key = "";
  owner = "root";
  group = "root";
  mode = "0400";
};
```

The SMTP principal's email must equal `from` in `gateway.toml` when the server
enforces sender matching.

## 5. PostgreSQL

With the default `database.create = true` the module creates the login role
(named `user`) and the database, with `ensureDBOwnership`, so peer auth over the
unix socket just works:

```
HERALD_DATABASE_URL=postgres:///herald?host=/run/postgresql
```

Set `database.create = false` to use an externally managed database; then the
DSN must carry working credentials. The API, dispatcher and `token` commands all
connect to the same database and run migrations on startup (see
[migrations.md](migrations.md)).

## 6. nginx and ACME

Setting `domain` creates an nginx virtual host with `forceSSL = true` and
`enableACME = true`, proxying `https://<domain><basePath>/…` to
`127.0.0.1:<port>`. Requirements:

- DNS `A`/`AAAA` for the domain points at the host (ACME HTTP-01).
- Ports 80 and 443 are open to the internet.
- `server.addr`'s port matches `services.herald.port`.
- `basePath = "/"` serves at the root; any other value is stripped before
  proxying (so `/api/v1/events` reaches the API as `/v1/events`).

## 7. Separate dispatcher

`dispatcher.enable` (default `true`) creates the second unit,
`herald-dispatcher`, running `herald dispatch --workers N --interval <i>`. Scale
`workers` or restart the consumer without touching the API. Set
`dispatcher.enable = false` only if you run the consumer elsewhere (do **not**
use `serve --dispatch` in production; it exists for local dev).

## 8. Build and switch

The package is `buildGoModule`; on the first build against a new `go.sum`, set
`vendorHash` in `flake.nix` to the hash the build reports.

```sh
sudo nixos-rebuild switch --flake /etc/nixos#server
systemctl status herald herald-dispatcher
curl -fsS https://herald.example.com/api/healthz
```

Create a token for a project (matches a `[projects.<name>]` in `gateway.toml`):

```sh
sudo -u herald herald token create \
  --database-url 'postgres:///herald?host=/run/postgresql' --project axis
# → herald_axis_<secret>   (shown once; store it in that project's secret store)
```

Then submit from a client:

```sh
HERALD_GATEWAY=https://herald.example.com/api HERALD_TOKEN=… \
  herald send --status success --title "deploy finished"
```

## Upgrade

1. Back up the database (below) — migrations are forward-only.
2. `nix flake update herald` (older Nix: `nix flake lock --update-input herald`)
3. `sudo nixos-rebuild switch --flake /etc/nixos#server`

Migrations run automatically at startup, so the binary can never run against an
older schema. Reverting the *binary* alone can leave it behind the schema; see
[migrations.md](migrations.md).

## Rollback

The package, config and units are all immutable store paths, so a rollback is
atomic and returns every piece together.

```sh
# one generation back
sudo nixos-rebuild switch --rollback

# list generations, then pin a specific one
sudo nix-env -p /nix/var/nix/profiles/system --list-generations
sudo nix-env -p /nix/var/nix/profiles/system --switch-generation 145
sudo /nix/var/nix/profiles/system/bin/switch-to-configuration switch
```

Rolling back to a generation that predates a database migration is not enough on
its own: restore the matching database backup too (see
[migrations.md](migrations.md)). Boot the previous generation from the
bootloader menu if the new one does not come up.

## Backup and restore

The database is the only mutable state Herald owns; the nix store is immutable
and secrets live in sops-encrypted files in the config repo.

```sh
# while the service is running (consistent logical dump)
sudo -u postgres pg_dump --format=custom --file=/var/backup/herald-$(date +%F).dump herald

# restore into a fresh database (same PostgreSQL major version)
sudo -u postgres createdb herald_restore
sudo -u postgres pg_restore --dbname=herald_restore --no-owner /var/backup/herald-2026-10-13.dump
```

Run the restore/re-migrate smoke test in [migrations.md](migrations.md) before
trusting a backup. `pg_dumpall` captures all roles if you also want the grant
state; otherwise recreate the role with `database.create = true` and re-import
the dump.

## Health, logs and operations

- `GET <basePath>/healthz` → `200 ok` (liveness); `GET <basePath>/readyz` →
  `200 ready` or `503 unavailable` (readiness).
- `journalctl -u herald -u herald-dispatcher -f` for API and delivery logs.
- `systemctl restart herald herald-dispatcher` is safe; both units are enabled
  at boot and restart on failure with a 2s delay.

## Security notes

- The API binds loopback only; expose it through nginx with TLS.
- Secrets are only in the sops `EnvironmentFile` (`0400 root:root`); the
  committed `gateway.toml` holds no credentials.
- Event content is untrusted input; the gateway redacts secrets before
  persistence and never treats requested headers as authoritative.
