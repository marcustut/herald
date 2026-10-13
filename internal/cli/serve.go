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
	"syscall"
	"time"

	"github.com/marcustut/herald/internal/adapters/hash"
	"github.com/marcustut/herald/internal/adapters/httpapi"
	"github.com/marcustut/herald/internal/adapters/id"
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

	sinks, err := sink.Build(cfg, os.Getenv("HERALD_SMTP_USERNAME"), os.Getenv("HERALD_SMTP_PASSWORD"))
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
		Logger: logger,
	}

	runCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *runDispatcher {
		dispatcher := app.Dispatcher{
			Deliveries:  st.deliveries,
			Events:      st.events,
			Sinks:       sinks,
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
