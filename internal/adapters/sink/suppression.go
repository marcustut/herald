package sink

import (
	"context"
	"errors"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// suppressionGuard wraps an email sink and refuses delivery to an address on
// the platform-wide suppression list. The list is global to the deployment, so
// one hard bounce or complaint stops every tenant sending to that mailbox and
// protects the shared sending reputation.
//
// The check is live at delivery time, so an address suppressed by a bounce or
// complaint webhook after the event was queued is still refused. A store error
// is returned as-is (retryable): a transient outage must never silently drop
// mail.
type suppressionGuard struct {
	inner        app.Sink
	address      string
	suppressions app.SuppressionStore
}

func (g *suppressionGuard) ID() domain.SinkID { return g.inner.ID() }

func (g *suppressionGuard) Deliver(ctx context.Context, e domain.Event) error {
	suppressed, err := g.suppressions.Suppressed(ctx, g.address)
	if err != nil {
		return err
	}
	if suppressed {
		// No address in the message: it is never logged or persisted.
		return app.Suppressed(errors.New("recipient is on the suppression list"))
	}
	return g.inner.Deliver(ctx, e)
}

var _ app.Sink = (*suppressionGuard)(nil)
