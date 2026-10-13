package app

import (
	"time"

	"github.com/marcustut/herald/internal/domain"
)

// DeliveryOutcome classifies how a single delivery attempt ended. It is the
// bounded set allowed as a metric label.
type DeliveryOutcome string

const (
	OutcomeDelivered DeliveryOutcome = "delivered"
	OutcomeRetry     DeliveryOutcome = "retry"
	OutcomeDead      DeliveryOutcome = "dead"
	// OutcomeBounce is a permanent recipient rejection (a hard SMTP bounce)
	// dead-lettered immediately instead of retried.
	OutcomeBounce DeliveryOutcome = "bounce"
)

// Observer receives operational measurements for metrics. It is a port so the
// HTTP server and dispatcher stay independent of any particular metrics
// backend.
//
// Implementations must be safe for concurrent use. Callers only ever pass
// bounded label values (registered route patterns, standard HTTP methods, sink
// ids from configuration, the DeliveryOutcome set): never a bearer token, a
// project id, or event content, so metric cardinality stays bounded and no
// secret reaches a label.
type Observer interface {
	// ObserveHTTP records one accepted HTTP request by route, method and
	// response status.
	ObserveHTTP(route, method string, status int, took time.Duration)
	// ObserveDelivery records one delivery attempt and its outcome for a sink.
	ObserveDelivery(sink domain.SinkID, outcome DeliveryOutcome, took time.Duration)
	// ObserveDispatchPoll records that the dispatcher completed a poll, so
	// alerting can detect a stalled or dead dispatcher.
	ObserveDispatchPoll(now time.Time)
	// ObserveSubmitRejection records one submit rejected by a limit, by scope
	// (a bounded app.Scope* value), so send-rate spikes and abuse are visible.
	ObserveSubmitRejection(scope string)
	// ObserveQueueDepth records the total outstanding (pending + inflight)
	// deliveries across all tenants, sampled by the dispatcher, for queue
	// congestion alerting.
	ObserveQueueDepth(depth int)
}

// NoopObserver discards every measurement. It is the zero value used when no
// observer is wired.
type NoopObserver struct{}

func (NoopObserver) ObserveHTTP(string, string, int, time.Duration)                {}
func (NoopObserver) ObserveDelivery(domain.SinkID, DeliveryOutcome, time.Duration) {}
func (NoopObserver) ObserveDispatchPoll(time.Time)                                 {}
func (NoopObserver) ObserveSubmitRejection(string)                                 {}
func (NoopObserver) ObserveQueueDepth(int)                                         {}

var _ Observer = NoopObserver{}

// DefaultObserver backs components that were not given an explicit Observer.
// It starts as a no-op; the metrics adapter installs its process-wide registry
// here from init so a stock binary emits metrics without a composition-root
// change. It is only assigned during package initialization.
var DefaultObserver Observer = NoopObserver{}

// Heartbeat is signalled by the dispatcher after each successful poll. Readiness
// uses it to distinguish a live process from one whose dispatcher has stalled.
type Heartbeat interface {
	Beat(now time.Time)
}
