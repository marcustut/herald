package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/marcustut/herald/internal/adapters/hash"
	"github.com/marcustut/herald/internal/adapters/httpapi"
	"github.com/marcustut/herald/internal/adapters/id"
	"github.com/marcustut/herald/internal/adapters/memory"
	"github.com/marcustut/herald/internal/adapters/secret"
	"github.com/marcustut/herald/internal/adapters/sink"
	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/config"
	"github.com/marcustut/herald/internal/domain"
)

// runServe runs the HTTP API. In production the dispatcher runs as a separate
// process (herald dispatch); --dispatch runs it in-process for development.
func runServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("serve", stderr)
	configPath := fs.String("config", "gateway.toml", "path to gateway.toml")
	dsn := fs.String("database-url", os.Getenv("HERALD_DATABASE_URL"), "PostgreSQL connection string")
	dev := fs.Bool("dev", false, "use in-memory stores (no persistence)")
	runDispatcher := fs.Bool("dispatch", false, "also run the delivery dispatcher in this process (dev only)")
	issueToken := fs.String("issue-token", "", "issue a project token for this project at startup")
	interval := fs.Duration("interval", time.Second, "dispatcher poll interval when --dispatch is set")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.LoadGateway(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	logger := slog.New(slog.NewTextHandler(stderr, nil))

	st, err := openStores(ctx, *dsn, *dev, logger)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if st.close != nil {
		defer st.close()
	}

	smtpUser := os.Getenv("HERALD_SMTP_USERNAME")
	smtpPass := os.Getenv("HERALD_SMTP_PASSWORD")
	sinks, err := sink.Build(cfg, smtpUser, smtpPass)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	if *issueToken != "" {
		issuer := app.Issuer{Store: st.credentials, Hasher: hash.SHA256{}, Secrets: secret.Generator{}, Clock: systemClock{}}
		raw, err := issuer.IssueProjectToken(ctx, domain.TenantID(cfg.Server.Tenant), domain.ProjectID(*issueToken))
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintln(stdout, raw)
	}

	api := &httpapi.Server{
		Auth: app.Authenticator{Store: st.credentials, Hasher: hash.SHA256{}},
		Submit: app.SubmitEvent{
			Events: st.events,
			Sinks:  sink.NewDirectory(cfg),
			IDs:    id.Random{},
			Clock:  systemClock{},
		},
		Limiter: memory.NewRateLimiter(cfg.EffectiveLimits(), systemClock{}, st.deliveries),
		Logger:  logger,
	}

	if cfg.Cloud.Enabled {
		if st.identities == nil {
			fmt.Fprintln(stderr, "error: cloud.enabled requires a database (set --database-url or HERALD_DATABASE_URL)")
			return 1
		}
		if cfg.Cloud.BounceWebhook && os.Getenv("HERALD_BOUNCE_WEBHOOK_SECRET") == "" {
			fmt.Fprintln(stderr, "error: cloud.bounce_webhook requires HERALD_BOUNCE_WEBHOOK_SECRET")
			return 1
		}
		// Hosted deployments resolve a project's destinations from the database
		// instead of gateway.toml; self-host keeps the config-driven directory.
		if st.sinkDirectory == nil {
			fmt.Fprintln(stderr, "error: cloud.enabled requires the ownership store (set --database-url or HERALD_DATABASE_URL)")
			return 1
		}
		api.Submit.Sinks = st.sinkDirectory
		api.Cloud = cloudAPI(cfg, st, logger)
		logger.Info("cloud auth enabled", "base_url", cfg.Cloud.BaseURL)
	}

	runCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *runDispatcher {
		dispatcher := app.Dispatcher{
			Deliveries:  st.deliveries,
			Events:      st.events,
			Sinks:       dispatchSinks(cfg, st, sinks, smtpUser, smtpPass),
			Clock:       systemClock{},
			Lease:       time.Minute,
			MaxAttempts: 8,
			Backoff:     domain.Backoff{Base: time.Second, Cap: 5 * time.Minute},
			Batch:       20,
			Logger:      logger,
		}
		go func() { _ = dispatcher.Run(runCtx, *interval) }()
	}

	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-runCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	logger.Info("herald listening", "addr", cfg.Server.Addr, "projects", len(cfg.Projects))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped", "error", err)
		return 1
	}
	return 0
}

// cloudAPI composes the hosted sign-in surface: postgres-backed identities,
// sessions and magic-link tokens, the shared SMTP mailer, and the in-memory
// sign-in limiter. The session cookie is Secure whenever the configured public
// origin is https.
func cloudAPI(cfg config.Gateway, st stores, logger *slog.Logger) *httpapi.Cloud {
	mailer := sink.NewMailer(sink.EmailConfig{
		Addr:          cfg.SMTP.Addr,
		From:          cfg.SMTP.From,
		FromName:      cfg.SMTP.FromName,
		Username:      os.Getenv("HERALD_SMTP_USERNAME"),
		Password:      os.Getenv("HERALD_SMTP_PASSWORD"),
		TLSServerName: cfg.SMTP.ServerName,
	})
	cloud := &httpapi.Cloud{
		Auth: app.Auth{
			Identities: st.identities,
			Tokens:     st.loginTokens,
			Sessions:   st.sessions,
			Mailer:     mailer,
			Secrets:    secret.Generator{},
			Hasher:     hash.SHA256{},
			IDs:        id.Random{},
			Clock:      systemClock{},
			LinkBase:   cfg.Cloud.BaseURL,
		},
		Projects: app.Projects{
			Store:       st.projects,
			Credentials: st.credentials,
			Issuer:      app.Issuer{Store: st.credentials, Hasher: hash.SHA256{}, Secrets: secret.Generator{}, Clock: systemClock{}},
			Clock:       systemClock{},
		},
		Destinations: &app.Destinations{
			Projects: st.projects,
			Store:    st.destinations,
			Tokens:   st.destVerifications,
			Mailer:   mailer,
			Secrets:  secret.Generator{},
			Hasher:   hash.SHA256{},
			IDs:      id.Random{},
			Clock:    systemClock{},
			LinkBase: cfg.Cloud.BaseURL,
		},
		Events:       st.eventLog,
		Limiter:      memory.NewLoginLimiter(httpapi.DefaultAuthRequests, httpapi.DefaultAuthWindow, systemClock{}),
		CookieSecure: strings.HasPrefix(cfg.Cloud.BaseURL, "https://"),
		Logger:       logger,
	}
	// The bounce/complaint ingestion route is only mounted when the use case is
	// wired and the shared secret is set; runServe has already refused an
	// enabled gate with no secret.
	if cfg.Cloud.BounceWebhook {
		cloud.EmailEvents = &app.EmailEvents{Suppressions: st.suppressions, Clock: systemClock{}}
		cloud.BounceSecret = os.Getenv("HERALD_BOUNCE_WEBHOOK_SECRET")
	}
	return cloud
}
