package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/marcustut/herald/internal/app"
)

// Suppressions is a PostgreSQL app.SuppressionStore. The suppression list is
// global to the deployment (not tenant-scoped): it protects the shared sending
// reputation every tenant relies on.
type Suppressions struct{ pool *pgxpool.Pool }

func NewSuppressions(pool *pgxpool.Pool) *Suppressions { return &Suppressions{pool: pool} }

func (s *Suppressions) Suppressed(ctx context.Context, address string) (bool, error) {
	var ok bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM suppressions WHERE address = $1)`, address).Scan(&ok); err != nil {
		return false, fmt.Errorf("suppression lookup: %w", err)
	}
	return ok, nil
}

func (s *Suppressions) IngestProviderEvent(ctx context.Context, e app.ProviderEvent) (bool, error) {
	// The event-id insert and the suppression upsert share one transaction: the
	// suppression is applied only when the id is new, so a replayed webhook
	// cannot re-apply it, and a crash leaves neither half applied.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("ingest provider event begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		INSERT INTO email_provider_events (event_id, kind, address, permanent, received_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (event_id) DO NOTHING`,
		e.ID, string(e.Kind), e.Address, e.Permanent, e.ReceivedAt)
	if err != nil {
		return false, fmt.Errorf("record provider event: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil // already ingested: a replay
	}
	if e.Suppresses() {
		if _, err := tx.Exec(ctx, `
			INSERT INTO suppressions (address, reason, created_at)
			VALUES ($1, $2, $3)
			ON CONFLICT (address) DO NOTHING`,
			e.Address, e.Reason(), e.ReceivedAt); err != nil {
			return false, fmt.Errorf("suppress address: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("ingest provider event commit: %w", err)
	}
	return true, nil
}

var _ app.SuppressionStore = (*Suppressions)(nil)
