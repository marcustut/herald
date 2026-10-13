# ADR 0001: Hosted multi-tenant isolation and resource ownership

- Status: Accepted
- Milestone: [M2] Herald Cloud Private Beta (issue #28)
- Depends on: versioned migrations (#18)

## Problem

Herald v1 is single-owner. `gateway.toml` names one tenant (`server.tenant`,
conventionally `owner`), every project is a TOML `[projects.*]` table, and every
sink is a global `[sinks.*]` entry. The database already carries a `tenant_id`
on `credentials`, `events` and `deliveries`, and credentials resolve to a
`domain.Principal{TenantID, ProjectID}`, but there is no durable model for *who
owns what*: no account, no user, no project row, and no destination row. The
only place a project or destination exists is the config file.

The hosted product needs durable ownership so that:

- an account can contain many tenants, and a tenant many projects;
- one tenant can never read or mutate another tenant's resources;
- tokens and destinations resolve by **tenant and project**, never from
  client-supplied ownership;
- the self-hosted single-tenant binary keeps working unchanged.

This ADR defines the ownership model, the schema that backs it, and the
compatibility guarantees. It does **not** define signup/login (#29), project or
dashboard APIs (#30/#33), destination verification (#31), managed email (#32),
quotas (#35) or CLI login (#34).

## Existing constraints

- **Layering is strict.** `internal/domain` is pure (no infrastructure
  imports); `internal/app` holds use cases and ports; adapters implement ports;
  `internal/cli` composes them. The ownership model must live in the domain with
  validation, and the stores must implement app ports.
- **Event is canonical.** The `Event` aggregate and the `POST /v1/events`
  contract are frozen; `POST /v1/reports` remains a deprecated alias. `Report`
  must not be reintroduced.
- **Migrations are append-only.** Migration 1 is the baseline; its checksum is
  recorded and verified. New schema must arrive as a new version that is safe on
  a fresh database and on an existing Event database, with no data loss
  ([docs/migrations.md](../migrations.md)).
- **Tenant columns predate the model.** `events`, `deliveries` and
  `credentials` already store `tenant_id`, and `events`/`deliveries` store
  `project_id` as free text. Existing rows reference projects that exist only in
  TOML; they cannot be retro-fitted with foreign keys without either inventing
  rows or dropping data.
- **Recipients are pinned server-side.** A destination is resolved from the
  credential plus admin config, never from the request body
  ([docs/security.md](../security.md)). The new model must preserve this.
- **No new dependencies** without justification; the standard library and the
  existing `pgx` are sufficient.

## Alternatives considered

1. **Reuse `tenant_id`/`project_id` text columns only, no new tables.** Smallest
   diff, but gives no place to hang account/user identity, no integrity for
   "project belongs to tenant", and no way to enumerate a tenant's destinations.
   It also leaves ownership as a string convention rather than a checked
   relation. Rejected.

2. **One generic `resources` table with a `kind` discriminator.** Avoids five
   tables but loses typed foreign keys, makes isolation queries stringly typed,
   and fights the codebase's one-struct-per-aggregate style. Rejected.

3. **Add foreign keys to `events`/`credentials`/`deliveries` in place.** Would
   enforce ownership at the database, but existing rows reference projects that
   do not exist as rows, so the migration would either fail or have to backfill
   projects from event data — a lossy guess. Rejected for this milestone;
   revisit when the hosted control plane provisions projects before writes.

4. **Scope every query by (tenant, project) in application code only.** The
   chosen direction, because it matches the existing store interfaces (which
   already take `tenantID`/`projectID`) and needs no data migration. The new
   tables add a checked tenant→project→destination chain for hosted resources,
   while the pre-existing `events`/`credentials` tables keep their current,
   already tenant-scoped queries.

5. **A separate database or schema per tenant.** Strong physical isolation, but
   heavy for a private beta and a large operational change. Rejected for now;
   the ownership model is forward-compatible with per-tenant export/deletion.

## Proposed design

### Ownership hierarchy

```
Account ──owns──▶ User            (human identity; authenticates to the control plane)
   │
   └────owns──▶ Tenant ──owns──▶ Project ──owns──▶ Destination
                   │                 │
                   │                 └── Events (existing) ──▶ Deliveries (existing)
                   └── Credentials (existing)
```

- **Account** is the top-level billing and administrative unit. It owns users
  and tenants.
- **User** is a human identity inside an account. Reserved for #29; no
  authentication is implemented here.
- **Tenant** is the isolation boundary. Every credential, project, event,
  delivery and destination carries a tenant id, and all reads/writes are scoped
  by it.
- **Project** is a named event stream owned by exactly one tenant. A project
  slug (`ProjectID`) is unique **within** a tenant, so two tenants may each own
  a project called `axis`.
- **Destination** is a delivery target owned by exactly one `(tenant, project)`.
  It is the durable form of a configured sink (`kind` = `email` | `webhook`,
  `target` = recipient address or URL).
- **Credential** (existing) is issued for a tenant and a set of projects.
- **Event / Delivery** (existing) remain tenant- and project-scoped.
- **Device** is reserved for M3 (agent/device pairing and per-device
  credentials). No table and no type are added now; the tenant/project chain is
  the join point a future `devices` table will hang off, mirroring
  `destinations`.

### Isolation rules

1. A resource id is only ever addressed together with its owning tenant (and
   project where applicable). Every store method takes `tenant`/`project`, and a
   lookup that does not match returns not-found — never another tenant's row.
2. Writes cannot re-point ownership. An upsert of an existing destination whose
   row belongs to a different tenant/project affects zero rows and returns
   not-found.
3. `Principal` is derived from the credential: the tenant comes from the stored
   credential, the project from the token (project token) or from the
   credential's authorized scope (scoped token). No request field can widen
   either. This is unchanged from v1 and is now backed by durable project rows.
4. The config-driven sink directory is keyed by `(tenant, project)`, so a
   request for a project under the wrong tenant resolves to no sinks.

### Ports

- `app.ProjectStore`: tenant-scoped `PutProject`, `Project`, `DeleteProject`.
- `app.DestinationStore`: tenant- and project-scoped `PutDestination`,
  `Destination`, `DestinationsByProject`, `DeleteDestination`.
- `postgres.Ownership` implements both, plus `app.SinkDirectory`, so a hosted
  deployment (wired in #31/#33) resolves enabled destinations by tenant and
  project from the database.

## Trade-offs

- **Application-level scoping, not database row-level security.** Every store
  method must remember its tenant/project predicate. This is simpler and
  portable, but it is a discipline, not an invariant. Tests in
  `internal/adapters/postgres` assert the isolation boundary directly. A future
  ADR may add Postgres RLS as defence in depth.
- **Two sources of destinations.** Self-host keeps `gateway.toml`; hosted uses
  the `destinations` table. Both satisfy `app.SinkDirectory`; a deployment uses
  one.
- **No foreign keys on the legacy tables.** `events`/`deliveries`/`credentials`
  keep free-text tenant/project ids, so a hosted bug could in principle store an
  event for a project that does not exist. Provisioning (#29/#30) will create
  projects before writes; adding FKs is deferred to avoid a lossy backfill.
- **Account/user tables are currently migration-only.** They exist so #29 can
  provision against a stable schema without another migration; no code reads
  them yet. This is deliberate staging, not dead code to be removed.
- **Per-project limits are keyed by slug only.** With multiple tenants the same
  slug shares one budget. This is an existing limitation tracked by quotas
  (#35), not a regression.

## Compatibility impact

- **`POST /v1/reports`.** Untouched. The alias, `Deprecation`/`Link` headers and
  response shape are unchanged; the submit path still builds the canonical
  `Event`.
- **Self-hosted `gateway.toml`.** Unchanged and still the source of truth for
  projects and sinks. The binary runs with no ownership rows: migrating adds
  empty tables and changes no behaviour. `NewDirectory` now also checks the
  tenant, which is always `server.tenant` for tokens minted by `token create`,
  so existing deployments resolve the same sinks.
- **Existing databases.** Migration 2 only `CREATE TABLE IF NOT EXISTS`; it
  reads and writes no existing rows. Events, deliveries, credentials and
  `schema_migrations` are preserved byte-for-byte.
- **Existing event/credential rows.** Not rewritten. They keep the free-text
  tenant/project ids they already have.
- **Limits, rate limiting, dispatch.** Unchanged; the dispatcher remains a
  trusted, cross-tenant worker.

## Migration implications

- Version 2 (`ownership`) adds `accounts`, `tenants`, `users`, `projects` and
  `destinations`, with `tenants.account_id → accounts(id)`,
  `users.account_id → accounts(id)`, `projects.tenant_id → tenants(id)` and
  `destinations (tenant_id, project_id) → projects(tenant_id, id)`. All
  references cascade on delete.
- It is idempotent, records its checksum, and is applied under the existing
  advisory lock by `serve`, `dispatch` and `token create` at startup.
- Upgrade paths tested in `internal/adapters/postgres/migrations_test.go`:
  fresh database, legacy `reports` database, and an Event database that predates
  `schema_migrations` (rows preserved, both migration rows recorded).
- Roll-forward only, per [docs/migrations.md](../migrations.md). No down
  migration.

## Open questions / owner decisions surfaced

1. **Project identity.** This ADR uses the slug as `ProjectID` (unique per
   tenant) to preserve the Event contract. Public IDs work for the hosted
   control plane, but a slug is mutable-looking; if projects will be renamed,
   #30 should decide whether to introduce an opaque id alongside the slug.
2. **Destination id namespace.** Destination ids are generated globally unique
   today. If a future destination API lets clients choose ids, they must be
   namespaced or the composite key must change.
3. **User email uniqueness.** Assumed unique per account. If a user may belong
   to several accounts, #29 must change the unique key to `(account_id, email)`
   → global identity.
4. **Tenant deletion semantics.** Deletes cascade today. #32/#33 must decide
   whether tenant deletion should soft-delete, retain an audit trail, or require
   an export first.
