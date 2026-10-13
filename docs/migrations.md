# Database migrations

Herald versions its PostgreSQL schema with an ordered, append-only list of
migration steps in `internal/adapters/postgres/migrations.go`. There is no
separate migrate command: `serve`, `dispatch` and `token create` all call
`postgres.Migrate` during startup, so a deploy can never run against a schema
older than the binary.

## How migrations run

- `schema_migrations` records what has been applied:

  | column       | meaning                                             |
  |--------------|-----------------------------------------------------|
  | `version`    | monotonic step number (primary key)                 |
  | `name`       | human-readable step name                            |
  | `checksum`   | SHA-256 of the step's SQL, hex encoded              |
  | `applied_at` | when the step committed (`now()`)                   |

- On every startup `Migrate`:
  1. takes the session-level advisory lock `pg_advisory_lock(0x686572616c64)`,
     so concurrent `serve`/`dispatch`/`token` processes serialize and only one
     applies each step;
  2. creates `schema_migrations` if it is missing;
  3. reads the applied versions and **verifies every recorded checksum**;
  4. applies each pending step in its own transaction, recording the step in the
     same transaction, then releases the lock.
- Because each step commits on its own, a failure stops the run at the last
  committed version; fix the cause and restart, and the remaining steps apply.
- The lock is held on a pooled connection for the whole run and released in the
  same process. If a process is killed, PostgreSQL drops the lock with the
  session.

### Baseline (version 1)

`baseline` is the original idempotent schema blob. It creates the tables with
`IF NOT EXISTS` and contains guarded `reports` → `events` and
`report_id` → `event_id` renames plus the idempotency-constraint widening. It
therefore converges the three states a database can be in:

- a fresh database (tables created),
- a legacy `reports` database (renamed in place, rows preserved),
- an already-migrated Event database that predates `schema_migrations` (no-op,
  then the baseline is recorded).

### Sessions and magic links (version 3)

`auth` adds the hosted sign-in state: `login_tokens` (single-use, hashed
magic-link tokens with an expiry), `sessions` (hashed session and CSRF values,
cascading with their user/account/tenant) and a global unique index on
`users(email)`, so an address resolves to exactly one identity. It is purely
additive; the ownership tables were staged empty by version 2, so the unique
index cannot collide with existing rows.

### Checksum verification

If the SQL of an already-applied step no longer hashes to the recorded
checksum, startup fails with `migration N (...) checksum mismatch`. This is a
guard, not a bug: **never edit an applied migration**. Add a new step instead.

## Changing the schema

1. Append a step to `migrations` with the next `version` and a short `name`.
2. Keep it idempotent where practical, and prefer `IF EXISTS` / `IF NOT EXISTS`
   guarded DDL so an interrupted deploy is safe to retry.
3. Do not add a down migration. Combine the step with a matching change to the
   store code in the same commit.
4. Add or extend the integration tests under
   `internal/adapters/postgres/migrations_test.go` (they run only when
   `HERALD_TEST_DATABASE_URL` is set).

## Rollback-or-forward-fix

Herald has no automatic downgrade. If a schema change is bad, choose one:

- **Forward-fix (preferred).** Write the next migration that corrects the
  schema, ship it, and let startup apply it. This is the only path that keeps
  `schema_migrations` consistent across all instances.
- **Roll back the data.** Restore the pre-migration backup (below), then deploy a
  binary whose migration history matches the restored schema. A binary from an
  older release will refuse to run against a newer schema (`database has
  migration N applied but this binary does not know it`), so restore *and*
  roll the binary back together.

Never delete rows from `schema_migrations` by hand to force a re-run, and never
edit an applied step's SQL — both defeat the checksum guard and can leave
instances at different versions.

## Backup/restore smoke test

Run this before deploying a schema change, and periodically in staging. It
verifies a dump can be restored and re-migrated cleanly.

```sh
# 1. back up the live database
pg_dump --format=custom --file=herald-pre-migrate.dump "$HERALD_DATABASE_URL"

# 2. restore into a scratch database
createdb herald_restore
pg_restore --dbname=herald_restore --no-owner herald-pre-migrate.dump

# 3. run the new binary against the restore; Migrate must succeed (a no-op when
#    the dump already matches) and the checksum guard must pass. Any DB-backed
#    command runs Migrate; `token create` is a throwaway write in the scratch DB.
HERALD_DATABASE_URL='postgres:///herald_restore' herald token create --project smoke >/dev/null

# 4. inspect what is recorded
psql 'postgres:///herald_restore' \
  -c 'SELECT version, name, checksum, applied_at FROM schema_migrations ORDER BY version'

# 5. spot-check data survived the restore
psql 'postgres:///herald_restore' \
  -c 'SELECT count(*) FROM events; SELECT count(*) FROM deliveries; SELECT count(*) FROM credentials'

dropdb herald_restore
rm herald-pre-migrate.dump
```

If step 3 fails with a checksum mismatch, the binary and the restored history
disagree — stop and reconcile before touching the live database.
