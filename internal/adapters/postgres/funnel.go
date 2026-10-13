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

// Funnel derives the beta activation funnel's per-account answer from rows
// Herald already keeps (accounts, tenants, events, deliveries). It writes
// nothing and adds no data collection, so the funnel is privacy-aware by
// construction: the only inputs are an opaque tenant id and counts.
type Funnel struct{ pool *pgxpool.Pool }

func NewFunnel(pool *pgxpool.Pool) *Funnel { return &Funnel{pool: pool} }

// Activation reports whether a tenant's most recent successful delivery is its
// first distinct delivered event, and when the owning account was created. A
// tenant with no account (a self-host tenant) reports Known=false.
func (s *Funnel) Activation(ctx context.Context, tenant domain.TenantID) (app.DeliveryActivation, error) {
	var (
		created   time.Time
		delivered int
	)
	err := s.pool.QueryRow(ctx, `
		SELECT a.created_at,
		       (SELECT count(DISTINCT d.event_id)
		        FROM deliveries d
		        JOIN events e ON e.id = d.event_id
		        WHERE e.tenant_id = $1 AND d.state = 'delivered')
		FROM tenants t
		JOIN accounts a ON a.id = t.account_id
		WHERE t.id = $1`, string(tenant)).Scan(&created, &delivered)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.DeliveryActivation{}, nil
	}
	if err != nil {
		return app.DeliveryActivation{}, fmt.Errorf("activation: %w", err)
	}
	return app.DeliveryActivation{
		Known:            true,
		First:            delivered == 1,
		AccountCreatedAt: created,
	}, nil
}

var _ app.ActivationStore = (*Funnel)(nil)
