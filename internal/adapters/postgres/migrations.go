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
// same shape. Version 2 adds the durable ownership model.
var migrations = []migration{
	{version: 1, name: "baseline", sql: baselineSchema},
	{version: 2, name: "ownership", sql: ownershipSchema},
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
