package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/marcustut/herald/internal/adapters/sink"
	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/config"
	"github.com/marcustut/herald/internal/domain"
)

// runDispatch runs only the delivery queue consumer, so it can be scaled
// independently of the HTTP API.
func runDispatch(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("dispatch", stderr)
	configPath := fs.String("config", "gateway.toml", "path to gateway.toml")
	dsn := fs.String("database-url", os.Getenv("HERALD_DATABASE_URL"), "PostgreSQL connection string")
	workers := fs.Int("workers", 1, "number of concurrent claim loops")
	batch := fs.Int("batch", 20, "deliveries claimed per loop")
	interval := fs.Duration("interval", time.Second, "poll interval")
	lease := fs.Duration("lease", time.Minute, "claim lease duration")
	maxAttempts := fs.Int("max-attempts", 8, "attempts before a delivery is dead")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dsn == "" {
		fmt.Fprintln(stderr, "error: --database-url or HERALD_DATABASE_URL is required")
		return 1
	}

	cfg, err := config.LoadGateway(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	logger := slog.New(slog.NewTextHandler(stderr, nil))

	st, err := openStores(ctx, *dsn, false, logger)
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

	dispatcher := app.Dispatcher{
		Deliveries:  st.deliveries,
		Events:      st.events,
		Sinks:       sinks,
		Clock:       systemClock{},
		Lease:       *lease,
		MaxAttempts: *maxAttempts,
		Backoff:     domain.Backoff{Base: time.Second, Cap: 5 * time.Minute},
		Batch:       *batch,
		Logger:      logger,
	}

	runCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("herald dispatcher started", "workers", *workers, "interval", interval.String())
	var wg sync.WaitGroup
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = dispatcher.Run(runCtx, *interval)
		}()
	}
	wg.Wait()
	return 0
}
