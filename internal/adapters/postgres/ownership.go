package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// Ownership is a PostgreSQL app.ProjectStore, app.DestinationStore and
// app.SinkDirectory. Every read and mutation is scoped by tenant (and project):
// a caller can only ever address rows it owns, even when it knows a foreign id.
type Ownership struct{ pool *pgxpool.Pool }

func NewOwnership(pool *pgxpool.Pool) *Ownership { return &Ownership{pool: pool} }

func (s *Ownership) PutProject(ctx context.Context, p domain.Project) error {
	// The primary key is (tenant_id, id), so a tenant can only create or rename
	// its own project; another tenant's same-named project is a distinct row.
	_, err := s.pool.Exec(ctx, `
		INSERT INTO projects (tenant_id, id, name, created_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, id) DO UPDATE SET name = EXCLUDED.name`,
		string(p.TenantID), string(p.ID), p.Name, p.CreatedAt)
	if err != nil {
		return fmt.Errorf("put project: %w", err)
	}
	return nil
}

func (s *Ownership) Project(ctx context.Context, tenant domain.TenantID, id domain.ProjectID) (domain.Project, bool, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, tenant_id, name, created_at FROM projects
		WHERE tenant_id = $1 AND id = $2`, string(tenant), string(id))
	var (
		p        domain.Project
		pid      string
		tenantID string
	)
	if err := row.Scan(&pid, &tenantID, &p.Name, &p.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Project{}, false, nil
		}
		return domain.Project{}, false, fmt.Errorf("project: %w", err)
	}
	p.ID = domain.ProjectID(pid)
	p.TenantID = domain.TenantID(tenantID)
	return p, true, nil
}

func (s *Ownership) Projects(ctx context.Context, tenant domain.TenantID) ([]domain.Project, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, tenant_id, name, created_at FROM projects
		WHERE tenant_id = $1
		ORDER BY created_at DESC, id`, string(tenant))
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	var out []domain.Project
	for rows.Next() {
		var (
			p             domain.Project
			pid, tenantID string
		)
		if err := rows.Scan(&pid, &tenantID, &p.Name, &p.CreatedAt); err != nil {
			return nil, err
		}
		p.ID = domain.ProjectID(pid)
		p.TenantID = domain.TenantID(tenantID)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Ownership) DeleteProject(ctx context.Context, tenant domain.TenantID, id domain.ProjectID) error {
	// Destinations reference (tenant_id, id) with ON DELETE CASCADE, so a
	// project delete removes only that tenant's project and its destinations.
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM projects WHERE tenant_id = $1 AND id = $2`, string(tenant), string(id))
	if err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return app.ErrProjectNotFound
	}
	return nil
}

func (s *Ownership) PutDestination(ctx context.Context, d domain.Destination) error {
	// On conflict the ownership predicate must hold; otherwise the update is
	// skipped and zero rows are affected, so a known foreign id is never
	// re-pointed or overwritten.
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO destinations (id, tenant_id, project_id, kind, target, enabled, verified_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (id) DO UPDATE SET
			kind = EXCLUDED.kind,
			target = EXCLUDED.target,
			enabled = EXCLUDED.enabled,
			verified_at = EXCLUDED.verified_at
		WHERE destinations.tenant_id = EXCLUDED.tenant_id
		  AND destinations.project_id = EXCLUDED.project_id`,
		string(d.ID), string(d.TenantID), string(d.ProjectID), string(d.Kind), d.Target, d.Enabled, nullTime(d.VerifiedAt), d.CreatedAt)
	if err != nil {
		return fmt.Errorf("put destination: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return app.ErrDestinationNotFound
	}
	return nil
}

func (s *Ownership) Destination(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, id domain.DestinationID) (domain.Destination, bool, error) {
	row := s.pool.QueryRow(ctx, selectDestination+`
		WHERE id = $1 AND tenant_id = $2 AND project_id = $3`,
		string(id), string(tenant), string(project))
	d, err := scanDestination(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Destination{}, false, nil
	}
	if err != nil {
		return domain.Destination{}, false, err
	}
	return d, true, nil
}

func (s *Ownership) DestinationsByProject(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) ([]domain.Destination, error) {
	rows, err := s.pool.Query(ctx, selectDestination+`
		WHERE tenant_id = $1 AND project_id = $2
		ORDER BY created_at DESC, id`,
		string(tenant), string(project))
	if err != nil {
		return nil, fmt.Errorf("list destinations: %w", err)
	}
	defer rows.Close()

	var out []domain.Destination
	for rows.Next() {
		d, err := scanDestination(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Ownership) DeleteDestination(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, id domain.DestinationID) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM destinations WHERE id = $1 AND tenant_id = $2 AND project_id = $3`,
		string(id), string(tenant), string(project))
	if err != nil {
		return fmt.Errorf("delete destination: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return app.ErrDestinationNotFound
	}
	return nil
}

// MarkVerified records that ownership of the destination's target was proven.
// The owner comes from the caller (a stored verification token), so a token can
// only ever verify the destination it was minted for.
func (s *Ownership) MarkVerified(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, id domain.DestinationID, now time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE destinations SET verified_at = $4
		WHERE id = $1 AND tenant_id = $2 AND project_id = $3`,
		string(id), string(tenant), string(project), now)
	if err != nil {
		return fmt.Errorf("mark destination verified: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return app.ErrDestinationNotFound
	}
	return nil
}

// EnabledSinks resolves the enabled destinations a project's events fan out to,
// by tenant and project. It satisfies app.SinkDirectory for a hosted deployment.
// An unverified destination is never delivered to, so it is omitted here.
func (s *Ownership) EnabledSinks(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) ([]domain.SinkID, error) {
	destinations, err := s.DestinationsByProject(ctx, tenant, project)
	if err != nil {
		return nil, err
	}
	var ids []domain.SinkID
	for _, d := range destinations {
		if d.Enabled && d.Verified() {
			ids = append(ids, domain.SinkID(d.ID))
		}
	}
	return ids, nil
}

const selectDestination = `SELECT id, tenant_id, project_id, kind, target, enabled, verified_at, created_at FROM destinations`

func scanDestination(row pgx.Row) (domain.Destination, error) {
	var (
		d          domain.Destination
		id         string
		tenantID   string
		projectID  string
		kind       string
		verifiedAt *time.Time
	)
	if err := row.Scan(&id, &tenantID, &projectID, &kind, &d.Target, &d.Enabled, &verifiedAt, &d.CreatedAt); err != nil {
		return domain.Destination{}, err
	}
	d.ID = domain.DestinationID(id)
	d.TenantID = domain.TenantID(tenantID)
	d.ProjectID = domain.ProjectID(projectID)
	d.Kind = domain.DestinationKind(kind)
	d.VerifiedAt = timeOrZero(verifiedAt)
	return d, nil
}

var (
	_ app.ProjectStore     = (*Ownership)(nil)
	_ app.DestinationStore = (*Ownership)(nil)
	_ app.SinkDirectory    = (*Ownership)(nil)
)
