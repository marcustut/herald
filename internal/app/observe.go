package app

import (
	"time"

	"github.com/marcustut/herald/internal/domain"
)

// FunnelStage is one step in the beta activation funnel. The set is fixed so a
// stage can be a bounded metric label; no account id, address or event content
// is ever attached to it.
type FunnelStage string

const (
	// FunnelAccountCreated marks a first sign-in that provisioned an account.
	FunnelAccountCreated FunnelStage = "account_created"
	// FunnelProjectCreated marks a project created through the cloud surface.
	FunnelProjectCreated FunnelStage = "project_created"
	// FunnelFirstDelivered marks an account's first event reaching a sink.
	FunnelFirstDelivered FunnelStage = "first_delivered"
	// FunnelRepeatSend marks a later event reaching a sink, i.e. retention.
	FunnelRepeatSend FunnelStage = "repeat_send"
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
	// OutcomeSuppressed is a permanent refusal of a recipient on the
	// suppression list, dead-lettered immediately instead of retried.
	OutcomeSuppressed DeliveryOutcome = "suppressed"
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
	// ObserveEmailEvent records one ingested provider notification, by bounded
	// kind (delivery, bounce, complaint). Acceptance and cost are derived from
	// the delivery and bounce counters; the provider is the billing source of
	// truth.
	ObserveEmailEvent(kind string)
	// ObserveSuppression records that an address was added to the suppression
	// list, by bounded reason (bounce, complaint).
	ObserveSuppression(reason string)
	// ObserveFunnel records one beta funnel stage transition, by bounded stage
	// (account_created, project_created, first_delivered, repeat_send). It
	// carries no identifier, address or content, so the funnel is countable
	// without collecting personal data.
	ObserveFunnel(stage string)
	// ObserveActivation records account-created -> first-delivered for one
	// account, so the beta can watch first-notification setup time.
	ObserveActivation(took time.Duration)
}

// NoopObserver discards every measurement. It is the zero value used when no
// observer is wired.
type NoopObserver struct{}

func (NoopObserver) ObserveHTTP(string, string, int, time.Duration)                {}
func (NoopObserver) ObserveDelivery(domain.SinkID, DeliveryOutcome, time.Duration) {}
func (NoopObserver) ObserveDispatchPoll(time.Time)                                 {}
func (NoopObserver) ObserveSubmitRejection(string)                                 {}
func (NoopObserver) ObserveQueueDepth(int)                                         {}
func (NoopObserver) ObserveEmailEvent(string)                                      {}
func (NoopObserver) ObserveSuppression(string)                                     {}
func (NoopObserver) ObserveFunnel(string)                                          {}
func (NoopObserver) ObserveActivation(time.Duration)                               {}

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
