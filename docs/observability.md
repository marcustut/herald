# Observability

Herald exposes process-local metrics at `GET /metrics` in the Prometheus text
exposition format, keeps liveness and readiness separate, and documents SQL
queries for the health of the durable PostgreSQL queue.

The metrics registry (`internal/adapters/metrics`) and the observer port
(`internal/app/observe.go`) are dependency-free: standard library only.

## Design choice: a scrape endpoint *and* SQL

A scrape endpoint is in scope, so Herald serves one. But the queue *is*
PostgreSQL, shared by every dispatcher replica, while the registry is
process-local. Reporting queue depth or lag from one process would be wrong when
several dispatchers run, and the `DeliveryStore` port deliberately exposes only
`Claim`/`Update`. So the split is:

- **`/metrics`** covers what a process knows first-hand: HTTP accepts by status,
  per-attempt delivery outcomes and latency, and dispatcher liveness.
- **SQL** (below) is the source of truth for queue depth, queue lag, stuck
  inflight rows, and dead-letter counts across all replicas.

`GET /metrics` is served by `httpapi.Server`. The metrics adapter installs its
registry as `app.DefaultObserver` at `init`, so the HTTP server and dispatcher
emit metrics with no extra wiring; `herald serve` exposes the endpoint
immediately.

## Endpoints

| endpoint | meaning | failure |
|---|---|---|
| `GET /healthz` | **Liveness.** The process is up and serving. It never depends on the database or dispatcher, so a downstream outage does not cause a restart loop. | non-200 only if the process is gone |
| `GET /readyz` | **Readiness.** PostgreSQL answers a ping **and** the dispatcher has completed a poll within its max age. | `503 unavailable` |
| `GET /metrics` | Prometheus text exposition. | — |

Readiness is built from `metrics.Health{Ping, Beat, MaxAge}`: `Ping` checks the
database, `Beat` is the dispatcher `*metrics.Heartbeat`. See [Wiring](#wiring).

## Metrics

All labels are bounded: registered route patterns, standard HTTP methods, HTTP
status codes, sink ids from the gateway config, and the fixed delivery-outcome
set. Bearer tokens, project ids, and event bodies are **never** used as labels.

| metric | type | labels | meaning |
|---|---|---|---|
| `herald_http_requests_total` | counter | `route`, `method`, `status` | HTTP requests accepted, by route, method, and response status. |
| `herald_http_request_duration_seconds` | histogram | `route`, `method` | HTTP request latency (default Prometheus buckets). |
| `herald_delivery_attempts_total` | counter | `sink` | Delivery attempts (one per `sink.Deliver` call). |
| `herald_delivery_outcomes_total` | counter | `sink`, `outcome` | Attempt result: `delivered`, `retry`, `dead`, `bounce` (a hard recipient rejection), or `suppressed` (refused because the address is on the suppression list). |
| `herald_delivery_duration_seconds` | histogram | `sink`, `outcome` | Delivery attempt latency. |
| `herald_dispatcher_last_poll_timestamp_seconds` | gauge | — | Unix time of the dispatcher's last successful queue poll. |
| `herald_submit_rejections_total` | counter | `scope` | Submits rejected by a limit, by scope (`requests`, `daily`, `monthly`, `queue`); a rising rate is a send-rate spike or abuse. |
| `herald_delivery_backlog` | gauge | — | Pending and inflight deliveries across all tenants, sampled by the dispatcher on each successful poll. |
| `herald_email_events_total` | counter | `kind` | Provider notifications ingested, by kind (`delivery`, `bounce`, `complaint`). Acceptance and cost are derived from this and `herald_delivery_outcomes_total` (see [Cost](#cost)). |
| `herald_email_suppressions_total` | counter | `reason` | Addresses added to the platform-wide suppression list, by reason (`bounce`, `complaint`). |
| `herald_funnel_events_total` | counter | `stage` | Beta funnel stage transitions: `account_created`, `project_created`, `first_delivered`, `repeat_send`. See [beta.md](beta.md). |
| `herald_activation_seconds` | histogram | — | Account-created → first-delivered setup time, bucketed from one minute to one week. See [beta.md](beta.md). |

`route` is the matched `http.ServeMux` pattern with the method stripped (for
example `/v1/events`); unmatched requests become `other`. Unknown HTTP methods
become `other`, and an unknown sink id becomes `unknown`, so a client cannot
inflate cardinality.

### Example output

```
# HELP herald_http_requests_total HTTP requests accepted, by route, method and status code.
# TYPE herald_http_requests_total counter
herald_http_requests_total{method="POST",route="/v1/events",status="202"} 42
herald_http_requests_total{method="POST",route="/v1/events",status="401"} 3
# HELP herald_delivery_outcomes_total Delivery attempts, by sink and outcome (delivered, retry, dead, bounce, suppressed).
# TYPE herald_delivery_outcomes_total counter
herald_delivery_outcomes_total{outcome="delivered",sink="email"} 40
herald_delivery_outcomes_total{outcome="dead",sink="webhook"} 2
herald_delivery_outcomes_total{outcome="suppressed",sink="email"} 1
# HELP herald_dispatcher_last_poll_timestamp_seconds Unix time of the dispatcher's last successful queue poll.
# TYPE herald_dispatcher_last_poll_timestamp_seconds gauge
herald_dispatcher_last_poll_timestamp_seconds 1760000000
```

## Useful PromQL

```promql
# Accepted submissions per second, by status.
sum by (status) (rate(herald_http_requests_total{route="/v1/events"}[5m]))

# Server error rate.
sum(rate(herald_http_requests_total{status=~"5.."}[5m]))

# p95 delivery latency by sink.
histogram_quantile(0.95,
  sum by (le, sink) (rate(herald_delivery_duration_seconds_bucket[5m])))

# Dead-letter rate per sink.
sum by (sink) (rate(herald_delivery_outcomes_total{outcome="dead"}[5m]))

# Retry pressure (backlog forming).
sum by (sink) (rate(herald_delivery_outcomes_total{outcome="retry"}[5m]))

# Is the dispatcher alive?
time() - herald_dispatcher_last_poll_timestamp_seconds

# Submit rejections by scope (throttling, or an abuse attempt).
sum by (scope) (rate(herald_submit_rejections_total[5m]))

# Queue congestion: pending + inflight sampled by the dispatcher.
herald_delivery_backlog

# Addresses suppressed by a bounce or complaint, per minute.
sum by (reason) (rate(herald_email_suppressions_total[5m]))

# Delivery attempts refused because the address is suppressed.
sum(rate(herald_delivery_outcomes_total{outcome="suppressed"}[5m]))

# Ingested provider notifications, by kind (a spike is a delivery problem).
sum by (kind) (rate(herald_email_events_total[5m]))
```

## Cost

The provider (SES) is the billing source of truth, so Herald does not invent a
price. Because there is one shared relay, the month's spend is the accepted-send
count times the provider's per-message rate:

```promql
# Accepted sends this month (hosted email sinks).
sum(increase(herald_delivery_outcomes_total{outcome="delivered",sink=~"dst_.*"}[30d]))
```

Multiply by the SES rate for the deployment's region and add any fixed fees. For
the `<$20/mo` budget, alert well before the ceiling:

```yaml
      - alert: HeraldEmailCostApproachingBudget
        # 20,000 accepted sends at $0.10/1,000 ≈ $2; adjust the rate to the region.
        expr: >
          sum(increase(herald_delivery_outcomes_total{outcome="delivered",sink=~"dst_.*"}[30d]))
          * 0.0001 > 15
        for: 1h
        labels: {severity: warning}
        annotations:
          summary: "Herald estimated email spend is approaching the monthly budget"
```

## Queue queries (source of truth)

Run against the Herald database. They cover the whole durable queue, across all
dispatcher replicas.

```sql
-- Depth by state, and how stale the oldest due pending row is (queue lag).
SELECT
  count(*) FILTER (WHERE state = 'pending')  AS pending,
  count(*) FILTER (WHERE state = 'inflight') AS inflight,
  count(*) FILTER (WHERE state = 'delivered') AS delivered,
  count(*) FILTER (WHERE state = 'dead')      AS dead,
  extract(epoch FROM (
    now() - min(next_attempt_at) FILTER (
      WHERE state = 'pending' AND next_attempt_at <= now()
    )
  )) AS lag_seconds
FROM deliveries;
```

```sql
-- Dead letters by sink, with attempt counts, for triage.
SELECT sink_id, attempts, count(*) AS rows,
       max(updated_at) AS last_failure
FROM deliveries
WHERE state = 'dead'
GROUP BY sink_id, attempts
ORDER BY rows DESC;
```

```sql
-- Stuck inflight rows: a worker claimed them and never finished, and the lease
-- has expired. Non-zero (and growing) means a crashed or wedged dispatcher.
SELECT count(*) AS stuck
FROM deliveries
WHERE state = 'inflight' AND lease_until < now();
```

```sql
-- Retry runway: when the next due delivery will fire.
SELECT sink_id, min(next_attempt_at) AS next_attempt
FROM deliveries
WHERE state = 'pending'
GROUP BY sink_id;
```

## Alert examples

Prometheus rules based on the scrape endpoint:

```yaml
groups:
  - name: herald
    rules:
      - alert: HeraldDeadLettersGrowing
        expr: increase(herald_delivery_outcomes_total{outcome="dead"}[15m]) > 0
        for: 5m
        labels: {severity: warning}
        annotations:
          summary: "Herald dead-lettered a delivery on {{ $labels.sink }}"

      - alert: HeraldDispatcherStalled
        expr: time() - herald_dispatcher_last_poll_timestamp_seconds > 60
        for: 2m
        labels: {severity: critical}
        annotations:
          summary: "Herald dispatcher has not polled the queue for over a minute"

      - alert: HeraldDeliveryRetryStorm
        expr: sum(rate(herald_delivery_outcomes_total{outcome="retry"}[5m])) > 5
        for: 10m
        labels: {severity: warning}
        annotations:
          summary: "Herald is retrying deliveries heavily; a sink may be down"

      - alert: HeraldServerErrors
        expr: sum(rate(herald_http_requests_total{status=~"5.."}[5m])) > 0
        for: 5m
        labels: {severity: warning}
        annotations:
          summary: "Herald returned 5xx responses"

      - alert: HeraldEmailBounces
        expr: increase(herald_delivery_outcomes_total{outcome="bounce"}[15m]) > 0
        for: 5m
        labels: {severity: warning}
        annotations:
          summary: "Herald dead-lettered a bounced email on {{ $labels.sink }}"

      - alert: HeraldEmailSuppressions
        expr: increase(herald_email_suppressions_total[15m]) > 0
        for: 5m
        labels: {severity: warning}
        annotations:
          summary: "Herald suppressed an address ({{ $labels.reason }}) after a provider event"

      - alert: HeraldSubmitRejectionsSpiking
        expr: sum by (scope) (rate(herald_submit_rejections_total[5m])) > 10
        for: 10m
        labels: {severity: warning}
        annotations:
          summary: "Herald is rejecting submits ({{ $labels.scope }}); a client is being throttled"

      - alert: HeraldQueueCongested
        expr: herald_delivery_backlog > 1000
        for: 10m
        labels: {severity: warning}
        annotations:
          summary: "Herald delivery backlog is large; the dispatcher may be falling behind"
```

Bounce/complaint coverage is split by what the delivery path can observe
synchronously. A **hard SMTP bounce** (a 5xx reply) is classified by the email
sink, dead-lettered at once, and counted as `outcome="bounce"`. **Asynchronous
bounces and complaints** (ESP feedback) arrive out of band: the authenticated,
replay-safe ingestion endpoint described in
[deliverability.md](deliverability.md) adds the address to the platform-wide
suppression list and counts `herald_email_events_total` /
`herald_email_suppressions_total`. A later delivery to a suppressed address is
refused at once and counted as `outcome="suppressed"` — a different signal from
a live provider bounce, so the two are not conflated.

Alerts that need the durable queue (SQL, e.g. via a metrics exporter or a cron
check). Use these thresholds as a starting point:

- **Queue lag** — the `lag_seconds` query above exceeding `300` means the
  dispatcher cannot keep up (or is not running).
- **Dead-letter growth** — `dead` increasing between checks, or a per-sink dead
  count above zero for a sink that should be healthy.
- **Stuck inflight** — the `stuck` query above staying non-zero after a lease
  period indicates a crashed dispatcher whose claims were never reclaimed.

## Logs

Both `serve` and `dispatch` use `log/slog` (text handler, key=value). The
dispatcher logs one line per failed attempt:

```
level=WARN msg="delivery attempt failed" sink=email event=evt_... attempts=3 error="..."
```

`event`/`sink` are identifiers, not content; the redacted event body and the
token are never logged. Query on a systemd host with:

```sh
journalctl -u herald -u herald-dispatcher -o cat | grep 'level=ERROR'
```

## Wiring

The metrics registry installs itself as `app.DefaultObserver`, so
`herald serve` serves `/metrics` and the dispatcher emits delivery counters with
no composition-root change. Readiness (`/readyz`) still needs the database ping
and the dispatcher heartbeat handed to it from `internal/cli`, which is a
separate change; the mechanism is `metrics.Health`:

```go
// internal/cli/serve.go
beat := &metrics.Heartbeat{}
health := metrics.Health{
    Ping:   stores.Ping,        // pgxpool.Pool.Ping (nil in --dev)
    Beat:   beat,               // shared with the dispatcher
    MaxAge: 3 * time.Second,    // comfortably above the poll interval
}
api := &httpapi.Server{
    // ...
    Observer: metrics.Default,
    Metrics:  metrics.Default,
    Ready:    health.Ready,
}
dispatcher := app.Dispatcher{
    // ...
    Observer:  metrics.Default,
    Heartbeat: beat,
}
```

Until that is wired, `/healthz` and `/readyz` still differ in intent: `/readyz`
returns `503` whenever a readiness probe is configured and fails, while
`/healthz` stays `200` for the life of the process.
