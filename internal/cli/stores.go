package cli

import (
	"context"
	"log/slog"

	"github.com/marcustut/herald/internal/adapters/memory"
	"github.com/marcustut/herald/internal/adapters/postgres"
	"github.com/marcustut/herald/internal/app"
)

type stores struct {
	credentials app.CredentialStore
	reports     app.ReportStore
	deliveries  app.DeliveryStore
	close       func()
}

// openStores returns PostgreSQL-backed stores, or in-memory stores in dev mode.
// In-memory stores are per-process, so they are only useful when serve and
// dispatch run in the same process (serve --dispatch).
func openStores(ctx context.Context, dsn string, dev bool, logger *slog.Logger) (stores, error) {
	if dev || dsn == "" {
		if dsn == "" && !dev && logger != nil {
			logger.Warn("no database configured; using in-memory stores")
		}
		mem := memory.NewReports()
		return stores{credentials: memory.NewCredentials(), reports: mem, deliveries: mem}, nil
	}
	pool, err := postgres.Open(ctx, dsn)
	if err != nil {
		return stores{}, err
	}
	if err := postgres.Migrate(ctx, pool); err != nil {
		pool.Close()
		return stores{}, err
	}
	reports := postgres.NewReports(pool)
	return stores{
		credentials: postgres.NewCredentials(pool),
		reports:     reports,
		deliveries:  reports,
		close:       pool.Close,
	}, nil
}
