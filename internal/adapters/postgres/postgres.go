// Package postgres implements the credential, event and delivery stores on
// PostgreSQL. The delivery queue uses SELECT ... FOR UPDATE SKIP LOCKED so many
// dispatcher instances can drain it safely.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// baselineSchema is migration version 1 (see migrations.go). It is idempotent
// and guarded so it applies cleanly to a fresh database, the legacy reports
// schema, and an already-migrated Event database.
const baselineSchema = `
-- Rename the pre-Event schema (reports / deliveries.report_id) in place so
-- existing data survives the Report -> Event rename. Guarded and idempotent;
-- a fresh database skips straight to the CREATE statements below.
DO $$
BEGIN
	IF to_regclass('public.reports') IS NOT NULL AND to_regclass('public.events') IS NULL THEN
		ALTER TABLE reports RENAME TO events;
	END IF;
	IF EXISTS (SELECT 1 FROM information_schema.columns
	           WHERE table_schema = 'public' AND table_name = 'deliveries'
	             AND column_name = 'report_id') THEN
		ALTER TABLE deliveries RENAME COLUMN report_id TO event_id;
	END IF;
END $$;

ALTER INDEX IF EXISTS deliveries_report RENAME TO deliveries_event;

CREATE TABLE IF NOT EXISTS credentials (
	token_hash   text PRIMARY KEY,
	tenant_id    text NOT NULL,
	kind         text NOT NULL,
	projects     text[] NOT NULL,
	created_at   timestamptz NOT NULL,
	revoked_at   timestamptz,
	last_used_at timestamptz
);

CREATE TABLE IF NOT EXISTS events (
	id              text PRIMARY KEY,
	tenant_id       text NOT NULL,
	project_id      text NOT NULL,
	agent_id        text NOT NULL,
	status          text NOT NULL,
	title           text NOT NULL,
	body            text NOT NULL,
	links           jsonb NOT NULL,
	context         jsonb NOT NULL,
	idempotency_key text NOT NULL,
	created_at      timestamptz NOT NULL,
	UNIQUE (tenant_id, project_id, idempotency_key)
);

CREATE TABLE IF NOT EXISTS deliveries (
	id              text PRIMARY KEY,
	event_id       text NOT NULL REFERENCES events(id),
	sink_id         text NOT NULL,
	state           text NOT NULL,
	attempts        integer NOT NULL,
	next_attempt_at timestamptz NOT NULL,
	lease_until     timestamptz,
	last_error      text NOT NULL DEFAULT '',
	created_at      timestamptz NOT NULL,
	updated_at      timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS deliveries_due ON deliveries (state, next_attempt_at);
CREATE INDEX IF NOT EXISTS deliveries_event ON deliveries (event_id);

-- Scope idempotency to (tenant, project, key) so two projects can reuse a key.
-- The legacy constraint was unique per tenant, which is strictly narrower, so
-- widening it can never conflict with existing rows. Guarded and idempotent.
DO $$
DECLARE
	old_name text;
BEGIN
	SELECT conname INTO old_name
	FROM pg_constraint
	WHERE conrelid = 'public.events'::regclass
	  AND contype = 'u'
	  AND pg_get_constraintdef(oid) = 'UNIQUE (tenant_id, idempotency_key)';
	IF old_name IS NOT NULL THEN
		EXECUTE format('ALTER TABLE events DROP CONSTRAINT %I', old_name);
	END IF;

	IF NOT EXISTS (
		SELECT 1 FROM pg_constraint
		WHERE conrelid = 'public.events'::regclass
		  AND contype = 'u'
		  AND pg_get_constraintdef(oid) = 'UNIQUE (tenant_id, project_id, idempotency_key)'
	) THEN
		ALTER TABLE events ADD CONSTRAINT events_tenant_project_idempotency_key
			UNIQUE (tenant_id, project_id, idempotency_key);
	END IF;
END $$;
`

// Open connects to PostgreSQL and verifies the connection.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// Credentials is a PostgreSQL app.CredentialStore.
type Credentials struct{ pool *pgxpool.Pool }

func NewCredentials(pool *pgxpool.Pool) *Credentials { return &Credentials{pool: pool} }

func (s *Credentials) Put(ctx context.Context, c app.Credential) error {
	projects := make([]string, len(c.Projects))
	for i, p := range c.Projects {
		projects[i] = string(p)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO credentials (token_hash, tenant_id, kind, projects, created_at, revoked_at, last_used_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (token_hash) DO UPDATE SET
			tenant_id = EXCLUDED.tenant_id,
			kind = EXCLUDED.kind,
			projects = EXCLUDED.projects,
			revoked_at = EXCLUDED.revoked_at,
			last_used_at = EXCLUDED.last_used_at`,
		c.Hash, string(c.TenantID), string(c.Kind), projects, c.CreatedAt, nullTime(c.RevokedAt), nullTime(c.LastUsedAt))
	if err != nil {
		return fmt.Errorf("put credential: %w", err)
	}
	return nil
}

func (s *Credentials) ByHash(ctx context.Context, hash string) (app.Credential, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT token_hash, tenant_id, kind, projects, created_at, revoked_at, last_used_at
		FROM credentials WHERE token_hash = $1`, hash)
	c, err := scanCredential(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Credential{}, app.ErrCredentialNotFound
	}
	return c, err
}

func (s *Credentials) ProjectCredential(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) (app.Credential, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT token_hash, tenant_id, kind, projects, created_at, revoked_at, last_used_at
		FROM credentials
		WHERE tenant_id = $1 AND kind = 'project' AND $2 = ANY(projects)
		LIMIT 1`, string(tenant), string(project))
	c, err := scanCredential(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Credential{}, app.ErrCredentialNotFound
	}
	return c, err
}

func (s *Credentials) ByTenant(ctx context.Context, tenant domain.TenantID) ([]app.Credential, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT token_hash, tenant_id, kind, projects, created_at, revoked_at, last_used_at
		FROM credentials
		WHERE tenant_id = $1
		ORDER BY created_at DESC, token_hash`, string(tenant))
	if err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}
	defer rows.Close()

	var out []app.Credential
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanCredential(row pgx.Row) (app.Credential, error) {
	var (
		c          app.Credential
		tenant     string
		kind       string
		projects   []string
		revokedAt  *time.Time
		lastUsedAt *time.Time
	)
	if err := row.Scan(&c.Hash, &tenant, &kind, &projects, &c.CreatedAt, &revokedAt, &lastUsedAt); err != nil {
		return app.Credential{}, err
	}
	c.TenantID = domain.TenantID(tenant)
	c.Kind = domain.TokenKind(kind)
	c.Projects = make([]domain.ProjectID, len(projects))
	for i, p := range projects {
		c.Projects[i] = domain.ProjectID(p)
	}
	c.RevokedAt = timeOrZero(revokedAt)
	c.LastUsedAt = timeOrZero(lastUsedAt)
	return c, nil
}

var _ app.CredentialStore = (*Credentials)(nil)

// Events is a PostgreSQL app.EventStore and app.DeliveryStore.
type Events struct{ pool *pgxpool.Pool }

func NewEvents(pool *pgxpool.Pool) *Events { return &Events{pool: pool} }

func (s *Events) Save(ctx context.Context, rep domain.Event, deliveries []domain.Delivery) error {
	links, err := json.Marshal(rep.Links)
	if err != nil {
		return fmt.Errorf("encode links: %w", err)
	}
	labels, err := json.Marshal(rep.Context)
	if err != nil {
		return fmt.Errorf("encode context: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, `
		INSERT INTO events (id, tenant_id, project_id, agent_id, status, title, body, links, context, idempotency_key, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		string(rep.ID), string(rep.TenantID), string(rep.ProjectID), string(rep.AgentID),
		string(rep.Status), rep.Title, rep.Body, links, labels, string(rep.IdempotencyKey), rep.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return app.ErrIdempotencyConflict
		}
		return fmt.Errorf("insert event: %w", err)
	}

	for _, d := range deliveries {
		_, err = tx.Exec(ctx, `
			INSERT INTO deliveries (id, event_id, sink_id, state, attempts, next_attempt_at, lease_until, last_error, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			string(d.ID), string(d.EventID), string(d.SinkID), string(d.State), d.Attempts,
			d.NextAttemptAt, nullTime(d.LeaseUntil), d.LastError, d.CreatedAt, d.UpdatedAt)
		if err != nil {
			return fmt.Errorf("insert delivery: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func (s *Events) ByID(ctx context.Context, id domain.EventID) (domain.Event, bool, error) {
	row := s.pool.QueryRow(ctx, selectEvent+` WHERE id = $1`, string(id))
	rep, err := scanEvent(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Event{}, false, nil
	}
	return rep, err == nil, err
}

func (s *Events) ByIdempotencyKey(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, key domain.IdempotencyKey) (domain.Event, bool, error) {
	row := s.pool.QueryRow(ctx, selectEvent+` WHERE tenant_id = $1 AND project_id = $2 AND idempotency_key = $3`, string(tenant), string(project), string(key))
	rep, err := scanEvent(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Event{}, false, nil
	}
	return rep, err == nil, err
}

const selectEvent = `SELECT id, tenant_id, project_id, agent_id, status, title, body, links, context, idempotency_key, created_at FROM events`

func scanEvent(row pgx.Row) (domain.Event, error) {
	var (
		rep     domain.Event
		id      string
		tenant  string
		project string
		agent   string
		status  string
		links   []byte
		context []byte
		key     string
	)
	if err := row.Scan(&id, &tenant, &project, &agent, &status, &rep.Title, &rep.Body, &links, &context, &key, &rep.CreatedAt); err != nil {
		return domain.Event{}, err
	}
	rep.ID = domain.EventID(id)
	rep.TenantID = domain.TenantID(tenant)
	rep.ProjectID = domain.ProjectID(project)
	rep.AgentID = domain.AgentID(agent)
	rep.Status = domain.Status(status)
	rep.IdempotencyKey = domain.IdempotencyKey(key)
	if err := json.Unmarshal(links, &rep.Links); err != nil {
		return domain.Event{}, fmt.Errorf("decode links: %w", err)
	}
	if err := json.Unmarshal(context, &rep.Context); err != nil {
		return domain.Event{}, fmt.Errorf("decode context: %w", err)
	}
	return rep, nil
}

const claimQuery = `
UPDATE deliveries SET
	state = 'inflight',
	attempts = attempts + 1,
	lease_until = $2,
	updated_at = $1
WHERE id IN (
	SELECT id FROM deliveries
	WHERE state = 'pending' AND next_attempt_at <= $1
	ORDER BY next_attempt_at
	FOR UPDATE SKIP LOCKED
	LIMIT $3
)
RETURNING id, event_id, sink_id, state, attempts, next_attempt_at, lease_until, last_error, created_at, updated_at`

func (s *Events) Claim(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]domain.Delivery, error) {
	if limit <= 0 {
		limit = 1
	}
	// Recover any worker that claimed a delivery and then died before it could
	// record the outcome, so its lease does not strand the row forever.
	if _, err := s.ReclaimExpired(ctx, now); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, claimQuery, now, now.Add(lease), limit)
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	defer rows.Close()

	var claimed []domain.Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		claimed = append(claimed, d)
	}
	return claimed, rows.Err()
}

const reclaimQuery = `
UPDATE deliveries SET
	state = 'pending', next_attempt_at = $1, lease_until = NULL, updated_at = $1
WHERE state = 'inflight' AND lease_until IS NOT NULL AND lease_until <= $1`

func (s *Events) ReclaimExpired(ctx context.Context, now time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, reclaimQuery, now)
	if err != nil {
		return 0, fmt.Errorf("reclaim expired: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// outstandingQuery joins events for the (tenant, project) scope; the unique
// index on events (tenant_id, project_id, idempotency_key) is a usable prefix,
// so the count is indexed rather than a full scan.
const outstandingQuery = `
SELECT count(*) FROM deliveries d
JOIN events e ON e.id = d.event_id
WHERE e.tenant_id = $1 AND e.project_id = $2
  AND d.state IN ('pending', 'inflight')`

func (s *Events) Outstanding(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, outstandingQuery, string(tenant), string(project)).Scan(&n); err != nil {
		return 0, fmt.Errorf("outstanding deliveries: %w", err)
	}
	return n, nil
}

func (s *Events) Backlog(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM deliveries WHERE state IN ('pending', 'inflight')`).Scan(&n); err != nil {
		return 0, fmt.Errorf("delivery backlog: %w", err)
	}
	return n, nil
}

const selectDelivery = `SELECT id, event_id, sink_id, state, attempts, next_attempt_at, lease_until, last_error, created_at, updated_at FROM deliveries`

func (s *Events) DeliveryByID(ctx context.Context, id domain.DeliveryID) (domain.Delivery, bool, error) {
	row := s.pool.QueryRow(ctx, selectDelivery+` WHERE id = $1`, string(id))
	d, err := scanDelivery(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Delivery{}, false, nil
	}
	return d, err == nil, err
}

// LIMIT NULLIF($5, 0) lets a zero limit mean "no limit" (Postgres treats
// LIMIT NULL as unlimited).
const listDeliveryQuery = selectDelivery + `
WHERE ($1 = '' OR id = $1)
  AND ($2 = '' OR state = $2)
  AND ($3 = '' OR event_id = $3)
  AND ($4 = '' OR sink_id = $4)
ORDER BY created_at DESC, id
LIMIT NULLIF($5, 0)`

func (s *Events) ListDeliveries(ctx context.Context, f app.DeliveryFilter, limit int) ([]domain.Delivery, error) {
	if limit < 0 {
		limit = 0
	}
	rows, err := s.pool.Query(ctx, listDeliveryQuery,
		string(f.ID), string(f.State), string(f.EventID), string(f.SinkID), limit)
	if err != nil {
		return nil, fmt.Errorf("list deliveries: %w", err)
	}
	defer rows.Close()

	var out []domain.Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// The outer filter is ANDed with the safety clause, so a delivered row can never
// be selected and an inflight row is only picked up once its lease has expired.
const requeueQuery = `
UPDATE deliveries SET
	state = 'pending', attempts = 0, next_attempt_at = $2, lease_until = NULL, last_error = '', updated_at = $2
WHERE ($1 = '' OR id = $1)
  AND ($3 = '' OR state = $3)
  AND ($4 = '' OR event_id = $4)
  AND ($5 = '' OR sink_id = $5)
  AND (state IN ('dead', 'pending')
       OR (state = 'inflight' AND lease_until IS NOT NULL AND lease_until <= $2))`

func (s *Events) Requeue(ctx context.Context, f app.DeliveryFilter, now time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, requeueQuery,
		string(f.ID), now, string(f.State), string(f.EventID), string(f.SinkID))
	if err != nil {
		return 0, fmt.Errorf("requeue: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func (s *Events) Update(ctx context.Context, d domain.Delivery) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE deliveries SET
			state = $2, attempts = $3, next_attempt_at = $4, lease_until = $5, last_error = $6, updated_at = $7
		WHERE id = $1`,
		string(d.ID), string(d.State), d.Attempts, d.NextAttemptAt, nullTime(d.LeaseUntil), d.LastError, d.UpdatedAt)
	if err != nil {
		return fmt.Errorf("update delivery: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return app.ErrDeliveryNotFound
	}
	return nil
}

func scanDelivery(row pgx.Row) (domain.Delivery, error) {
	var (
		d       domain.Delivery
		id      string
		eventID string
		sinkID  string
		state   string
		lease   *time.Time
	)
	if err := row.Scan(&id, &eventID, &sinkID, &state, &d.Attempts, &d.NextAttemptAt, &lease, &d.LastError, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return domain.Delivery{}, err
	}
	d.ID = domain.DeliveryID(id)
	d.EventID = domain.EventID(eventID)
	d.SinkID = domain.SinkID(sinkID)
	d.State = domain.DeliveryState(state)
	d.LeaseUntil = timeOrZero(lease)
	return d, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func timeOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

var (
	_ app.EventStore    = (*Events)(nil)
	_ app.DeliveryStore = (*Events)(nil)
)
