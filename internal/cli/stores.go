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
	events      app.EventStore
	deliveries  app.DeliveryStore
	// Hosted-auth stores are nil in dev/memory mode; the cloud surface requires
	// a real database, so serve refuses to enable it without one.
	identities  app.IdentityStore
	sessions    app.SessionStore
	loginTokens app.LoginTokenStore
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
		mem := memory.NewEvents()
		return stores{credentials: memory.NewCredentials(), events: mem, deliveries: mem}, nil
	}
	pool, err := postgres.Open(ctx, dsn)
	if err != nil {
		return stores{}, err
	}
	if err := postgres.Migrate(ctx, pool); err != nil {
		pool.Close()
		return stores{}, err
	}
	events := postgres.NewEvents(pool)
	return stores{
		credentials: postgres.NewCredentials(pool),
		events:      events,
		deliveries:  events,
		identities:  postgres.NewIdentities(pool),
		sessions:    postgres.NewSessions(pool),
		loginTokens: postgres.NewLoginTokens(pool),
		close:       pool.Close,
	}, nil
}
