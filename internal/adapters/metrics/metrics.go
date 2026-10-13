// Package metrics is a tiny, dependency-free metrics registry: counters, gauges
// and latency histograms rendered in the Prometheus text exposition format.
//
// It deliberately exposes only fixed metric names and bounded label sets. A
// label value is either a registered route pattern, a standard HTTP method, a
// configured sink id, one of the app.DeliveryOutcome values, or an HTTP status
// code — never a bearer token, a project id, or event content.
package metrics

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// httpBuckets are the latency histogram buckets, in seconds. They match the
// Prometheus default histogram buckets.
var httpBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// activationBuckets are the first-notification setup-time buckets, in seconds:
// one minute up to a week. A beta account can take minutes or days to go from
// sign-in to its first delivered event, so the HTTP latency buckets do not fit.
var activationBuckets = []float64{60, 300, 900, 1800, 3600, 3 * 3600, 6 * 3600, 12 * 3600, 86400, 3 * 86400, 7 * 86400}

type labels map[string]string

type scalar struct {
	name   string
	labels labels
	value  float64
}

type histogram struct {
	name    string
	labels  labels
	buckets []float64
	counts  []uint64 // len(buckets)+1; the last slot is +Inf
	sum     float64
	count   uint64
}

// Registry holds process-local metrics. It implements app.Observer and
// http.Handler (serving the Prometheus text format) so the same value powers
// both instrumentation and the /metrics endpoint.
type Registry struct {
	mu       sync.Mutex
	counters map[string]*scalar
	gauges   map[string]*scalar
	hists    map[string]*histogram
}

// Default is the process-wide registry. The package installs it as
// app.DefaultObserver so the HTTP server and dispatcher emit metrics without a
// composition-root change.
var Default = NewRegistry()

func init() { app.DefaultObserver = Default }

func NewRegistry() *Registry {
	return &Registry{
		counters: map[string]*scalar{},
		gauges:   map[string]*scalar{},
		hists:    map[string]*histogram{},
	}
}

// ObserveHTTP satisfies app.Observer.
func (r *Registry) ObserveHTTP(route, method string, status int, took time.Duration) {
	route, method = httpRoute(route, method), httpMethod(method)
	r.inc("herald_http_requests_total",
		labels{"route": route, "method": method, "status": strconv.Itoa(status)})
	r.observe("herald_http_request_duration_seconds", labels{"route": route, "method": method}, took)
}

// ObserveDelivery satisfies app.Observer.
func (r *Registry) ObserveDelivery(sink domain.SinkID, outcome app.DeliveryOutcome, took time.Duration) {
	s := string(sink)
	if s == "" {
		s = "unknown"
	}
	r.inc("herald_delivery_attempts_total", labels{"sink": s})
	l := labels{"sink": s, "outcome": string(outcome)}
	r.inc("herald_delivery_outcomes_total", l)
	r.observe("herald_delivery_duration_seconds", l, took)
}

// ObserveDispatchPoll satisfies app.Observer.
func (r *Registry) ObserveDispatchPoll(now time.Time) {
	r.set("herald_dispatcher_last_poll_timestamp_seconds", nil, float64(now.Unix()))
}

// ObserveSubmitRejection satisfies app.Observer.
func (r *Registry) ObserveSubmitRejection(scope string) {
	r.inc("herald_submit_rejections_total", labels{"scope": rejectScope(scope)})
}

// ObserveQueueDepth satisfies app.Observer.
func (r *Registry) ObserveQueueDepth(depth int) {
	r.set("herald_delivery_backlog", nil, float64(depth))
}

// ObserveEmailEvent satisfies app.Observer.
func (r *Registry) ObserveEmailEvent(kind string) {
	r.inc("herald_email_events_total", labels{"kind": emailEventKind(kind)})
}

// ObserveSuppression satisfies app.Observer.
func (r *Registry) ObserveSuppression(reason string) {
	r.inc("herald_email_suppressions_total", labels{"reason": suppressionReason(reason)})
}

// ObserveFunnel satisfies app.Observer.
func (r *Registry) ObserveFunnel(stage string) {
	r.inc("herald_funnel_events_total", labels{"stage": funnelStage(stage)})
}

// ObserveActivation satisfies app.Observer.
func (r *Registry) ObserveActivation(took time.Duration) {
	r.observe("herald_activation_seconds", nil, took)
}

// ServeHTTP renders the registry in the Prometheus text exposition format.
func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, r.expose())
}

func (r *Registry) inc(name string, l labels) {
	key := seriesKey(name, l)
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.counters[key]
	if !ok {
		c = &scalar{name: name, labels: clone(l)}
		r.counters[key] = c
	}
	c.value++
}

func (r *Registry) set(name string, l labels, value float64) {
	key := seriesKey(name, l)
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.gauges[key]
	if !ok {
		g = &scalar{name: name, labels: clone(l)}
		r.gauges[key] = g
	}
	g.value = value
}

func (r *Registry) observe(name string, l labels, took time.Duration) {
	secs := took.Seconds()
	key := seriesKey(name, l)
	buckets := bucketsFor(name)
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.hists[key]
	if !ok {
		h = &histogram{name: name, labels: clone(l), buckets: buckets, counts: make([]uint64, len(buckets)+1)}
		r.hists[key] = h
	}
	h.counts[sort.SearchFloat64s(buckets, secs)]++
	h.sum += secs
	h.count++
}

// bucketsFor selects the histogram buckets for a metric. Only the activation
// histogram uses non-default buckets.
func bucketsFor(name string) []float64 {
	if name == "herald_activation_seconds" {
		return activationBuckets
	}
	return httpBuckets
}

func (r *Registry) expose() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Group scalar series by metric name so HELP/TYPE is written once.
	scalars := map[string][]*scalar{}
	for _, c := range r.counters {
		scalars[c.name] = append(scalars[c.name], c)
	}
	for _, g := range r.gauges {
		scalars[g.name] = append(scalars[g.name], g)
	}
	names := make([]string, 0, len(scalars))
	for name := range scalars {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		writeMeta(&b, name)
		series := scalars[name]
		sort.Slice(series, func(i, j int) bool {
			return seriesKey(series[i].name, series[i].labels) < seriesKey(series[j].name, series[j].labels)
		})
		for _, s := range series {
			fmt.Fprintf(&b, "%s%s %s\n", s.name, formatLabels(s.labels), formatFloat(s.value))
		}
	}

	names = names[:0]
	for _, h := range r.hists {
		names = append(names, h.name)
	}
	sort.Strings(names)
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		writeMeta(&b, name)
		series := make([]*histogram, 0, 1)
		for _, h := range r.hists {
			if h.name == name {
				series = append(series, h)
			}
		}
		sort.Slice(series, func(i, j int) bool {
			return seriesKey(series[i].name, series[i].labels) < seriesKey(series[j].name, series[j].labels)
		})
		for _, h := range series {
			writeHistogram(&b, h)
		}
	}
	return b.String()
}

func writeHistogram(b *strings.Builder, h *histogram) {
	var cumulative uint64
	for i, ub := range h.buckets {
		cumulative += h.counts[i]
		fmt.Fprintf(b, "%s_bucket%s %d\n", h.name,
			formatLabels(withLabel(h.labels, "le", formatFloat(ub))), cumulative)
	}
	cumulative += h.counts[len(h.buckets)]
	fmt.Fprintf(b, "%s_bucket%s %d\n", h.name,
		formatLabels(withLabel(h.labels, "le", "+Inf")), cumulative)
	fmt.Fprintf(b, "%s_sum%s %s\n", h.name, formatLabels(h.labels), formatFloat(h.sum))
	fmt.Fprintf(b, "%s_count%s %d\n", h.name, formatLabels(h.labels), h.count)
}

var meta = map[string][2]string{
	"herald_http_requests_total":                    {"counter", "HTTP requests accepted, by route, method and status code."},
	"herald_http_request_duration_seconds":          {"histogram", "HTTP request latency in seconds."},
	"herald_delivery_attempts_total":                {"counter", "Delivery attempts, by sink."},
	"herald_delivery_outcomes_total":                {"counter", "Delivery attempts, by sink and outcome (delivered, retry, dead, bounce)."},
	"herald_delivery_duration_seconds":              {"histogram", "Delivery attempt latency in seconds, by sink and outcome."},
	"herald_dispatcher_last_poll_timestamp_seconds": {"gauge", "Unix time of the dispatcher's last successful queue poll."},
	"herald_submit_rejections_total":                {"counter", "Submits rejected by a limit, by scope (requests, daily, monthly, queue)."},
	"herald_delivery_backlog":                       {"gauge", "Pending and inflight deliveries across all tenants, sampled by the dispatcher."},
	"herald_email_events_total":                     {"counter", "Ingested provider email notifications, by kind (delivery, bounce, complaint)."},
	"herald_email_suppressions_total":               {"counter", "Addresses added to the suppression list, by reason (bounce, complaint)."},
	"herald_funnel_events_total":                    {"counter", "Beta funnel stage transitions, by stage (account_created, project_created, first_delivered, repeat_send)."},
	"herald_activation_seconds":                     {"histogram", "Time from account creation to its first delivered event, in seconds."},
}

func writeMeta(b *strings.Builder, name string) {
	m, ok := meta[name]
	if !ok {
		fmt.Fprintf(b, "# TYPE %s untyped\n", name)
		return
	}
	fmt.Fprintf(b, "# HELP %s %s\n", name, m[1])
	fmt.Fprintf(b, "# TYPE %s %s\n", name, m[0])
}

// httpMethod bounds the method label to the standard verbs; anything else is
// "other" so a client cannot inflate cardinality with a custom method token.
func httpMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodHead, http.MethodOptions:
		return m
	default:
		return "other"
	}
}

// rejectScope bounds the rejection scope to the known app.Scope* values; an
// empty or unknown scope becomes "other" so a caller cannot inflate cardinality.
func rejectScope(s string) string {
	switch s {
	case app.ScopeRequests, app.ScopeDaily, app.ScopeMonthly, app.ScopeQueue:
		return s
	default:
		return "other"
	}
}

// emailEventKind bounds the provider-event kind to the known app.EmailEvent*
// values; an unknown kind becomes "other".
func emailEventKind(s string) string {
	switch app.EmailEventKind(s) {
	case app.EmailEventDelivery, app.EmailEventBounce, app.EmailEventComplaint:
		return s
	default:
		return "other"
	}
}

// suppressionReason bounds the suppression reason to the known values; an
// unknown reason becomes "other".
func suppressionReason(s string) string {
	switch s {
	case "bounce", "complaint":
		return s
	default:
		return "other"
	}
}

// funnelStage bounds the funnel stage to the known app.FunnelStage values; an
// unknown stage becomes "other" so a caller cannot inflate cardinality.
func funnelStage(s string) string {
	switch app.FunnelStage(s) {
	case app.FunnelAccountCreated, app.FunnelProjectCreated, app.FunnelFirstDelivered, app.FunnelRepeatSend:
		return s
	default:
		return "other"
	}
}

// httpRoute strips the method prefix from a ServeMux pattern ("POST /v1/events"
// -> "/v1/events"). Unmatched requests have no pattern and become "other".
func httpRoute(pattern, method string) string {
	if pattern == "" {
		return "other"
	}
	return strings.TrimPrefix(pattern, method+" ")
}

func seriesKey(name string, l labels) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(name)
	for _, k := range keys {
		b.WriteByte(0)
		b.WriteString(k)
		b.WriteByte(1)
		b.WriteString(l[k])
	}
	return b.String()
}

func withLabel(l labels, k, v string) labels {
	out := clone(l)
	if out == nil {
		out = labels{}
	}
	out[k] = v
	return out
}

func clone(l labels) labels {
	if l == nil {
		return nil
	}
	out := make(labels, len(l))
	for k, v := range l {
		out[k] = v
	}
	return out
}

func formatLabels(l labels) string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `%s="%s"`, k, escapeLabel(l[k]))
	}
	b.WriteByte('}')
	return b.String()
}

func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// formatFloat renders whole numbers without an exponent (timestamps, counters)
// and everything else compactly.
func formatFloat(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
