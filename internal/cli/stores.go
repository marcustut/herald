package cli

import (
	"context"
	"log/slog"

	"github.com/marcustut/herald/internal/adapters/memory"
	"github.com/marcustut/herald/internal/adapters/postgres"
	"github.com/marcustut/herald/internal/adapters/sink"
	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/config"
	"github.com/marcustut/herald/internal/domain"
)

type stores struct {
	credentials app.CredentialStore
	events      app.EventStore
	deliveries  app.DeliveryStore
	// eventLog is the tenant-scoped, read-only dashboard query surface. It is
	// the same PostgreSQL Events store as events/deliveries, typed for the
	// narrower read port; nil in dev/memory mode.
	eventLog app.EventLog
	// Hosted-auth stores are nil in dev/memory mode; the cloud surface requires
	// a real database, so serve refuses to enable it without one.
	identities  app.IdentityStore
	sessions    app.SessionStore
	loginTokens app.LoginTokenStore
	// Ownership stores back the hosted project/token/destination surface and
	// are nil in dev/memory mode (the tables are hosted-only). sinkDirectory
	// resolves a hosted project's enabled destinations from the database.
	projects          app.ProjectStore
	destinations      app.DestinationStore
	destVerifications app.DestinationVerificationStore
	sinkDirectory     app.SinkDirectory
	// suppressions is the platform-wide do-not-send list. It is hosted-only
	// (nil in dev/self-host mode), so the delivery guard is only applied when a
	// database backs the hosted surface.
	suppressions app.SuppressionStore
	// funnel derives the beta funnel's first-delivery signal from existing
	// rows; nil in dev/memory mode.
	funnel app.ActivationStore
	close  func()
}

// dispatchSinks selects the dispatcher's sink source. A hosted deployment
// resolves each delivery's destination from the database, so removal,
// disablement and verification are enforcement points at delivery time, and
// applies the suppression list; a self-host deployment keeps the sinks built
// from gateway.toml.
func dispatchSinks(cfg config.Gateway, st stores, static map[domain.SinkID]app.Sink, smtpUser, smtpPass string) app.SinkSource {
	if cfg.Cloud.Enabled && st.destinations != nil {
		return sink.NewDestinationSource(st.destinations, cfg, smtpUser, smtpPass).WithSuppressions(st.suppressions)
	}
	return app.SinkMap(static)
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
	ownership := postgres.NewOwnership(pool)
	return stores{
		credentials:       postgres.NewCredentials(pool),
		events:            events,
		deliveries:        events,
		eventLog:          events,
		projects:          ownership,
		identities:        postgres.NewIdentities(pool),
		sessions:          postgres.NewSessions(pool),
		loginTokens:       postgres.NewLoginTokens(pool),
		destinations:      ownership,
		destVerifications: postgres.NewDestinationVerifications(pool),
		sinkDirectory:     ownership,
		suppressions:      postgres.NewSuppressions(pool),
		funnel:            postgres.NewFunnel(pool),
		close:             pool.Close,
	}, nil
}
