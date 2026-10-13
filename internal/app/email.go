package app

import (
	"context"
	"errors"

	"github.com/marcustut/herald/internal/domain"
)

// ErrInvalidEmailEvent is returned for a provider event Herald cannot act on:
// a missing/unknown kind or an address it cannot canonicalize. The HTTP adapter
// maps it to a validation failure rather than a server error.
var ErrInvalidEmailEvent = errors.New("invalid email event")

// EmailEvents applies provider delivery/bounce/complaint notifications to the
// suppression list. Transport-level decoding lives in the HTTP adapter; this is
// the use case: canonicalize the address, ingest idempotently by event id, and
// observe the bounded metrics.
type EmailEvents struct {
	Suppressions SuppressionStore
	Clock        Clock
	Observer     Observer
}

func (u EmailEvents) observer() Observer {
	if u.Observer != nil {
		return u.Observer
	}
	return DefaultObserver
}

// Ingest records one provider event and suppresses its address when the event
// is a permanent failure or a complaint. A replay (the same event id) reports
// recorded=false and re-applies nothing.
func (u EmailEvents) Ingest(ctx context.Context, ev ProviderEvent) (bool, error) {
	if !validEmailEventKind(ev.Kind) {
		return false, ErrInvalidEmailEvent
	}
	addr, err := domain.NormalizeEmail(ev.Address)
	if err != nil {
		return false, ErrInvalidEmailEvent
	}
	ev.Address = addr
	if ev.ReceivedAt.IsZero() {
		ev.ReceivedAt = u.Clock.Now()
	}
	// Scope the provider's event id by the canonical address, so one
	// notification about several recipients yields one idempotency key each and
	// a provider that omits an id still dedupes by (kind, address).
	if ev.ID == "" {
		ev.ID = string(ev.Kind)
	}
	ev.ID += ":" + addr
	recorded, err := u.Suppressions.IngestProviderEvent(ctx, ev)
	if err != nil {
		return false, err
	}
	if !recorded {
		return false, nil
	}
	u.observer().ObserveEmailEvent(string(ev.Kind))
	if ev.Suppresses() {
		u.observer().ObserveSuppression(ev.Reason())
	}
	return true, nil
}

func validEmailEventKind(k EmailEventKind) bool {
	switch k {
	case EmailEventDelivery, EmailEventBounce, EmailEventComplaint:
		return true
	default:
		return false
	}
}
