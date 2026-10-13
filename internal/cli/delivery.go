package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// openDeliveryStore is the operator-inspection seam: tests replace it with an
// in-memory store, production connects to PostgreSQL.
var openDeliveryStore = func(ctx context.Context, dsn string) (app.DeliveryStore, func(), error) {
	st, err := openStores(ctx, dsn, false, nil)
	if err != nil {
		return nil, nil, err
	}
	closeFn := st.close
	if closeFn == nil {
		closeFn = func() {}
	}
	return st.deliveries, closeFn, nil
}

func runDelivery(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: herald delivery <list|show|retry|requeue|reclaim> ...")
		return 2
	}
	switch args[0] {
	case "list":
		return runDeliveryList(ctx, args[1:], stdout, stderr)
	case "show":
		return runDeliveryShow(ctx, args[1:], stdout, stderr)
	case "retry":
		return runDeliveryRetry(ctx, args[1:], stdout, stderr)
	case "requeue":
		return runDeliveryRequeue(ctx, args[1:], stdout, stderr)
	case "reclaim":
		return runDeliveryReclaim(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown delivery subcommand %q\n", args[0])
		return 2
	}
}

// openDelivery resolves the store from --database-url / HERALD_DATABASE_URL.
func openDelivery(ctx context.Context, stderr io.Writer, dsn string) (app.DeliveryStore, func(), int) {
	if dsn == "" {
		fmt.Fprintln(stderr, "error: --database-url or HERALD_DATABASE_URL is required")
		return nil, nil, 1
	}
	store, closeFn, err := openDeliveryStore(ctx, dsn)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return nil, nil, 1
	}
	return store, closeFn, 0
}

func runDeliveryList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("delivery list", stderr)
	dsn := fs.String("database-url", os.Getenv("HERALD_DATABASE_URL"), "PostgreSQL connection string")
	state := fs.String("state", "", "filter by state (pending|inflight|delivered|dead)")
	event := fs.String("event", "", "filter by event id")
	sink := fs.String("sink", "", "filter by sink id")
	limit := fs.Int("limit", 100, "maximum rows (0 = no limit)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *state != "" && !validState(domain.DeliveryState(*state)) {
		fmt.Fprintf(stderr, "error: unknown state %q\n", *state)
		return 2
	}
	store, closeFn, code := openDelivery(ctx, stderr, *dsn)
	if code != 0 {
		return code
	}
	defer closeFn()

	rows, err := store.ListDeliveries(ctx, app.DeliveryFilter{
		State:   domain.DeliveryState(*state),
		EventID: domain.EventID(*event),
		SinkID:  domain.SinkID(*sink),
	}, *limit)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tEVENT\tSINK\tATT\tNEXT ATTEMPT\tLAST ERROR")
	for _, d := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			d.ID, d.State, d.EventID, d.SinkID, d.Attempts, formatTime(d.NextAttemptAt), oneLine(d.LastError))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

func runDeliveryShow(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("delivery show", stderr)
	dsn := fs.String("database-url", os.Getenv("HERALD_DATABASE_URL"), "PostgreSQL connection string")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: herald delivery show <id> [--database-url URL]")
		return 2
	}
	store, closeFn, code := openDelivery(ctx, stderr, *dsn)
	if code != 0 {
		return code
	}
	defer closeFn()

	d, ok, err := store.DeliveryByID(ctx, domain.DeliveryID(fs.Arg(0)))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if !ok {
		fmt.Fprintf(stderr, "error: delivery %q not found\n", fs.Arg(0))
		return 1
	}

	w := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintf(w, "id:\t%s\n", d.ID)
	fmt.Fprintf(w, "event:\t%s\n", d.EventID)
	fmt.Fprintf(w, "sink:\t%s\n", d.SinkID)
	fmt.Fprintf(w, "state:\t%s\n", d.State)
	fmt.Fprintf(w, "attempts:\t%d\n", d.Attempts)
	fmt.Fprintf(w, "next attempt:\t%s\n", formatTime(d.NextAttemptAt))
	fmt.Fprintf(w, "lease until:\t%s\n", formatTime(d.LeaseUntil))
	fmt.Fprintf(w, "last error:\t%s\n", oneLine(d.LastError))
	fmt.Fprintf(w, "created:\t%s\n", formatTime(d.CreatedAt))
	fmt.Fprintf(w, "updated:\t%s\n", formatTime(d.UpdatedAt))
	if err := w.Flush(); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

func runDeliveryRetry(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("delivery retry", stderr)
	dsn := fs.String("database-url", os.Getenv("HERALD_DATABASE_URL"), "PostgreSQL connection string")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: herald delivery retry <id> [--database-url URL]")
		return 2
	}
	store, closeFn, code := openDelivery(ctx, stderr, *dsn)
	if code != 0 {
		return code
	}
	defer closeFn()

	id := domain.DeliveryID(fs.Arg(0))
	n, err := store.Requeue(ctx, app.DeliveryFilter{ID: id}, systemClock{}.Now())
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if n > 0 {
		fmt.Fprintln(stdout, "requeued", id)
		return 0
	}
	// Nothing changed: either the row is gone or it is protected from a re-send.
	d, ok, err := store.DeliveryByID(ctx, id)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if !ok {
		fmt.Fprintf(stderr, "error: delivery %q not found\n", id)
		return 1
	}
	fmt.Fprintf(stdout, "%s is %s; not requeued (already delivered or leased by a worker)\n", id, d.State)
	return 0
}

func runDeliveryRequeue(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("delivery requeue", stderr)
	dsn := fs.String("database-url", os.Getenv("HERALD_DATABASE_URL"), "PostgreSQL connection string")
	state := fs.String("state", string(domain.DeliveryDead), "state to re-drive (dead|pending)")
	event := fs.String("event", "", "only this event id")
	sink := fs.String("sink", "", "only this sink id")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *state != string(domain.DeliveryDead) && *state != string(domain.DeliveryPending) {
		fmt.Fprintf(stderr, "error: --state must be %q or %q\n", domain.DeliveryDead, domain.DeliveryPending)
		return 2
	}
	store, closeFn, code := openDelivery(ctx, stderr, *dsn)
	if code != 0 {
		return code
	}
	defer closeFn()

	n, err := store.Requeue(ctx, app.DeliveryFilter{
		State:   domain.DeliveryState(*state),
		EventID: domain.EventID(*event),
		SinkID:  domain.SinkID(*sink),
	}, systemClock{}.Now())
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "requeued %d %s delivery(s)\n", n, *state)
	return 0
}

func runDeliveryReclaim(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("delivery reclaim", stderr)
	dsn := fs.String("database-url", os.Getenv("HERALD_DATABASE_URL"), "PostgreSQL connection string")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	store, closeFn, code := openDelivery(ctx, stderr, *dsn)
	if code != 0 {
		return code
	}
	defer closeFn()

	n, err := store.ReclaimExpired(ctx, systemClock{}.Now())
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "reclaimed %d expired lease(s)\n", n)
	return 0
}

func validState(s domain.DeliveryState) bool {
	switch s {
	case domain.DeliveryPending, domain.DeliveryInflight, domain.DeliveryDelivered, domain.DeliveryDead:
		return true
	default:
		return false
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
