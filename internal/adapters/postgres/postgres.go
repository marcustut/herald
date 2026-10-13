// Package postgres implements the credential, report and delivery stores on
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

const schema = `
CREATE TABLE IF NOT EXISTS credentials (
	token_hash   text PRIMARY KEY,
	tenant_id    text NOT NULL,
	kind         text NOT NULL,
	projects     text[] NOT NULL,
	created_at   timestamptz NOT NULL,
	revoked_at   timestamptz,
	last_used_at timestamptz
);

CREATE TABLE IF NOT EXISTS reports (
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
	UNIQUE (tenant_id, idempotency_key)
);

CREATE TABLE IF NOT EXISTS deliveries (
	id              text PRIMARY KEY,
	report_id       text NOT NULL REFERENCES reports(id),
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
CREATE INDEX IF NOT EXISTS deliveries_report ON deliveries (report_id);
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

// Migrate creates the schema if it does not exist.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
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

// Reports is a PostgreSQL app.ReportStore and app.DeliveryStore.
type Reports struct{ pool *pgxpool.Pool }

func NewReports(pool *pgxpool.Pool) *Reports { return &Reports{pool: pool} }

func (s *Reports) Save(ctx context.Context, rep domain.Report, deliveries []domain.Delivery) error {
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
		INSERT INTO reports (id, tenant_id, project_id, agent_id, status, title, body, links, context, idempotency_key, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		string(rep.ID), string(rep.TenantID), string(rep.ProjectID), string(rep.AgentID),
		string(rep.Status), rep.Title, rep.Body, links, labels, string(rep.IdempotencyKey), rep.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return app.ErrIdempotencyConflict
		}
		return fmt.Errorf("insert report: %w", err)
	}

	for _, d := range deliveries {
		_, err = tx.Exec(ctx, `
			INSERT INTO deliveries (id, report_id, sink_id, state, attempts, next_attempt_at, lease_until, last_error, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			string(d.ID), string(d.ReportID), string(d.SinkID), string(d.State), d.Attempts,
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

func (s *Reports) ByID(ctx context.Context, id domain.ReportID) (domain.Report, bool, error) {
	row := s.pool.QueryRow(ctx, selectReport+` WHERE id = $1`, string(id))
	rep, err := scanReport(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Report{}, false, nil
	}
	return rep, err == nil, err
}

func (s *Reports) ByIdempotencyKey(ctx context.Context, tenant domain.TenantID, key domain.IdempotencyKey) (domain.Report, bool, error) {
	row := s.pool.QueryRow(ctx, selectReport+` WHERE tenant_id = $1 AND idempotency_key = $2`, string(tenant), string(key))
	rep, err := scanReport(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Report{}, false, nil
	}
	return rep, err == nil, err
}

const selectReport = `SELECT id, tenant_id, project_id, agent_id, status, title, body, links, context, idempotency_key, created_at FROM reports`

func scanReport(row pgx.Row) (domain.Report, error) {
	var (
		rep     domain.Report
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
		return domain.Report{}, err
	}
	rep.ID = domain.ReportID(id)
	rep.TenantID = domain.TenantID(tenant)
	rep.ProjectID = domain.ProjectID(project)
	rep.AgentID = domain.AgentID(agent)
	rep.Status = domain.Status(status)
	rep.IdempotencyKey = domain.IdempotencyKey(key)
	if err := json.Unmarshal(links, &rep.Links); err != nil {
		return domain.Report{}, fmt.Errorf("decode links: %w", err)
	}
	if err := json.Unmarshal(context, &rep.Context); err != nil {
		return domain.Report{}, fmt.Errorf("decode context: %w", err)
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
RETURNING id, report_id, sink_id, state, attempts, next_attempt_at, lease_until, last_error, created_at, updated_at`

func (s *Reports) Claim(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]domain.Delivery, error) {
	if limit <= 0 {
		limit = 1
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

func (s *Reports) Update(ctx context.Context, d domain.Delivery) error {
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
		d        domain.Delivery
		id       string
		reportID string
		sinkID   string
		state    string
		lease    *time.Time
	)
	if err := row.Scan(&id, &reportID, &sinkID, &state, &d.Attempts, &d.NextAttemptAt, &lease, &d.LastError, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return domain.Delivery{}, err
	}
	d.ID = domain.DeliveryID(id)
	d.ReportID = domain.ReportID(reportID)
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
	_ app.ReportStore   = (*Reports)(nil)
	_ app.DeliveryStore = (*Reports)(nil)
)
