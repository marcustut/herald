package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migration is one append-only schema step. Add new steps to the end of
// migrations with a fresh version; never edit an applied step, because its
// checksum is recorded in schema_migrations and verified on every startup.
type migration struct {
	version int
	name    string
	sql     string
}

// migrations is the ordered schema history. Version 1 is the baseline: it is the
// original idempotent schema blob (create-if-not-exists plus the guarded
// reports->events / report_id->event_id rename), so a fresh database, a legacy
// reports database, and an already-migrated Event database all converge on the
// same shape. Version 2 adds the durable ownership model. Version 3 adds the
// hosted sign-in state: single-use magic-link tokens, browser sessions, and a
// global uniqueness rule on user email. Version 4 adds destination
// verification. Version 5 adds the email-safety tables: the platform-wide
// suppression list and the provider-event ingestion log that makes bounce and
// complaint webhooks replay-safe.
var migrations = []migration{
	{version: 1, name: "baseline", sql: baselineSchema},
	{version: 2, name: "ownership", sql: ownershipSchema},
	{version: 3, name: "auth", sql: authSchema},
	{version: 4, name: "destination_verification", sql: destinationVerificationSchema},
	{version: 5, name: "email_safety", sql: emailSafetySchema},
}

// ownershipSchema is migration version 2. It adds the account/user/tenant/
// project/destination ownership chain. It is purely additive: it creates new
// tables and never touches events, deliveries, credentials or schema_migrations,
// so an existing self-host database upgrades with no data loss. Foreign keys
// cascade so deleting a parent removes its owned children; the legacy event and
// credential tables keep their free-text tenant/project ids and gain no FK (their
// rows predate project rows, see docs/adr/0001-multi-tenant-ownership.md).
const ownershipSchema = `
CREATE TABLE IF NOT EXISTS accounts (
	id         text PRIMARY KEY,
	name       text NOT NULL,
	created_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS tenants (
	id         text PRIMARY KEY,
	account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	name       text NOT NULL,
	created_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS tenants_account ON tenants (account_id);

CREATE TABLE IF NOT EXISTS users (
	id         text PRIMARY KEY,
	account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	email      text NOT NULL,
	created_at timestamptz NOT NULL,
	UNIQUE (account_id, email)
);

CREATE TABLE IF NOT EXISTS projects (
	tenant_id  text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
	id         text NOT NULL,
	name       text NOT NULL,
	created_at timestamptz NOT NULL,
	PRIMARY KEY (tenant_id, id)
);

CREATE TABLE IF NOT EXISTS destinations (
	id         text PRIMARY KEY,
	tenant_id  text NOT NULL,
	project_id text NOT NULL,
	kind       text NOT NULL,
	target     text NOT NULL,
	enabled    boolean NOT NULL DEFAULT true,
	created_at timestamptz NOT NULL,
	FOREIGN KEY (tenant_id, project_id) REFERENCES projects(tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS destinations_project ON destinations (tenant_id, project_id);
`

// authSchema is migration version 3. It is purely additive: it adds the
// magic-link and session tables the hosted sign-in flow needs and makes the
// user email globally unique, so an address always resolves to one identity.
// The ownership tables carried no rows before this migration (accounts/users
// were staged by version 2), so the unique index cannot collide with existing
// data. Sessions cascade away with the user or account they belong to, which
// makes account deletion a full revocation.
const authSchema = `
CREATE UNIQUE INDEX IF NOT EXISTS users_email ON users (email);

CREATE TABLE IF NOT EXISTS login_tokens (
	token_hash text PRIMARY KEY,
	email      text NOT NULL,
	created_at timestamptz NOT NULL,
	expires_at timestamptz NOT NULL,
	used_at    timestamptz
);
CREATE INDEX IF NOT EXISTS login_tokens_email ON login_tokens (email);

CREATE TABLE IF NOT EXISTS sessions (
	token_hash text PRIMARY KEY,
	user_id    text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	tenant_id  text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
	csrf_hash  text NOT NULL,
	created_at timestamptz NOT NULL,
	expires_at timestamptz NOT NULL,
	revoked_at timestamptz
);
CREATE INDEX IF NOT EXISTS sessions_account ON sessions (account_id);
`

// destinationVerificationSchema is migration version 4. It records destination
// verification. `verified_at` is nullable, so every existing and freshly
// created destination starts unverified and is delivered to only once its
// ownership is proven. The token table stores only the hash of a single-use
// verification token, carries the owning (tenant, project) so confirmation
// proves ownership from the stored row rather than the request, and cascades
// away with its destination. It is purely additive: no existing row is read or
// rewritten.
const destinationVerificationSchema = `
ALTER TABLE destinations ADD COLUMN IF NOT EXISTS verified_at timestamptz;

CREATE TABLE IF NOT EXISTS destination_verifications (
	token_hash     text PRIMARY KEY,
	destination_id text NOT NULL REFERENCES destinations(id) ON DELETE CASCADE,
	tenant_id      text NOT NULL,
	project_id     text NOT NULL,
	email          text NOT NULL,
	created_at     timestamptz NOT NULL,
	expires_at     timestamptz NOT NULL,
	used_at        timestamptz
);
CREATE INDEX IF NOT EXISTS destination_verifications_destination ON destination_verifications (destination_id);
`

// emailSafetySchema is migration version 5. `suppressions` is the platform-wide
// do-not-send list: an address is added by a permanent bounce or a complaint and
// the hosted delivery path refuses it from then on. `email_provider_events`
// records each ingested provider notification by its id, so a replayed bounce or
// complaint webhook is a no-op (the suppression is applied in the same
// transaction only when the id is new). Only the fields Herald acts on are
// stored; the raw provider payload, which may quote message content and
// addresses, is never persisted. It is purely additive.
const emailSafetySchema = `
CREATE TABLE IF NOT EXISTS suppressions (
	address    text PRIMARY KEY,
	reason     text NOT NULL,
	created_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS email_provider_events (
	event_id    text PRIMARY KEY,
	kind        text NOT NULL,
	address     text NOT NULL,
	permanent   boolean NOT NULL,
	received_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS email_provider_events_address ON email_provider_events (address);
`

const schemaMigrationsDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
	version    integer PRIMARY KEY,
	name       text NOT NULL,
	checksum   text NOT NULL,
	applied_at timestamptz NOT NULL DEFAULT now()
);`

// migrateLockID serializes concurrent migrations so that serve and dispatch can
// start at the same time without racing on DDL or recording a step twice.
const migrateLockID int64 = 0x686572616c64

// Migrate applies every unapplied migration in order. It holds a session-level
// advisory lock for the whole run, fails loudly if an already-applied step's
// checksum changed, and applies each pending step in its own transaction so a
// failed step leaves the database at the last committed version.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("migrate acquire: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLockID); err != nil {
		return fmt.Errorf("migrate lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrateLockID)
	}()

	if _, err := conn.Exec(ctx, schemaMigrationsDDL); err != nil {
		return fmt.Errorf("migrate: create schema_migrations: %w", err)
	}

	applied, err := appliedMigrations(ctx, conn)
	if err != nil {
		return err
	}
	known := make(map[int]bool, len(migrations))
	for _, m := range migrations {
		known[m.version] = true
	}
	for version := range applied {
		if !known[version] {
			return fmt.Errorf("migrate: database has migration %d applied but this binary does not know it (downgrade?)", version)
		}
	}

	for _, m := range migrations {
		sum := checksum(m.sql)
		if got, ok := applied[m.version]; ok {
			if got != sum {
				return fmt.Errorf("migrate: migration %d (%s) checksum mismatch: applied %s, expected %s; an applied migration changed", m.version, m.name, got, sum)
			}
			continue
		}
		if err := applyMigration(ctx, conn, m, sum); err != nil {
			return err
		}
	}
	return nil
}

func appliedMigrations(ctx context.Context, conn *pgxpool.Conn) (map[int]string, error) {
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("migrate: read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[int]string)
	for rows.Next() {
		var (
			version int
			sum     string
		)
		if err := rows.Scan(&version, &sum); err != nil {
			return nil, fmt.Errorf("migrate: scan schema_migrations: %w", err)
		}
		applied[version] = sum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate: read schema_migrations: %w", err)
	}
	return applied, nil
}

func applyMigration(ctx context.Context, conn *pgxpool.Conn, m migration, sum string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migrate %d begin: %w", m.version, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, m.sql); err != nil {
		return fmt.Errorf("migrate %d (%s): %w", m.version, m.name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
		m.version, m.name, sum); err != nil {
		return fmt.Errorf("migrate %d record: %w", m.version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("migrate %d commit: %w", m.version, err)
	}
	return nil
}

func checksum(sql string) string {
	sum := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(sum[:])
}
