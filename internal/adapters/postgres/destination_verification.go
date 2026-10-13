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

// DestinationVerifications is a PostgreSQL app.DestinationVerificationStore.
type DestinationVerifications struct{ pool *pgxpool.Pool }

func NewDestinationVerifications(pool *pgxpool.Pool) *DestinationVerifications {
	return &DestinationVerifications{pool: pool}
}

func (s *DestinationVerifications) PutDestinationVerification(ctx context.Context, r app.DestinationVerificationRecord) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO destination_verifications (token_hash, destination_id, tenant_id, project_id, email, created_at, expires_at, used_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (token_hash) DO NOTHING`,
		r.Hash, string(r.DestinationID), string(r.TenantID), string(r.ProjectID), r.Email,
		r.CreatedAt, r.ExpiresAt, nullTime(r.UsedAt)); err != nil {
		return fmt.Errorf("put destination verification: %w", err)
	}
	return nil
}

func (s *DestinationVerifications) ConsumeDestinationVerification(ctx context.Context, hash string, now time.Time) (app.DestinationVerificationRecord, error) {
	// The UPDATE ... WHERE used_at IS NULL is the single-use guarantee: the row
	// is atomically claimed, so two concurrent redemptions cannot both succeed.
	row := s.pool.QueryRow(ctx, `
		UPDATE destination_verifications SET used_at = $2
		WHERE token_hash = $1 AND used_at IS NULL AND expires_at > $2
		RETURNING token_hash, destination_id, tenant_id, project_id, email, created_at, expires_at, used_at`, hash, now)
	var (
		r         app.DestinationVerificationRecord
		dstID     string
		tenantID  string
		projectID string
		usedAt    *time.Time
	)
	if err := row.Scan(&r.Hash, &dstID, &tenantID, &projectID, &r.Email, &r.CreatedAt, &r.ExpiresAt, &usedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return app.DestinationVerificationRecord{}, app.ErrDestinationVerificationNotFound
		}
		return app.DestinationVerificationRecord{}, fmt.Errorf("consume destination verification: %w", err)
	}
	r.DestinationID = domain.DestinationID(dstID)
	r.TenantID = domain.TenantID(tenantID)
	r.ProjectID = domain.ProjectID(projectID)
	r.UsedAt = timeOrZero(usedAt)
	return r, nil
}

var _ app.DestinationVerificationStore = (*DestinationVerifications)(nil)
