# Submit rate limiting and quotas

Herald bounds how many events a project can submit, so a runaway agent, a
retry storm, or a compromised token cannot fill the delivery queue or flood the
operator's inbox. Limits are checked at `POST /v1/events` (and its deprecated
`/v1/reports` alias) after the caller is authenticated and before the event is
stored.

## Configuration

Limits live in `gateway.toml`. Every field is optional and inherits the built-in
default below.

```toml
# Global defaults, applied to every project.
[limits]
requests = 60      # requests allowed per window
window   = "1m"    # fixed window, Go duration syntax ("30s", "5m", "1h")
daily    = 2000    # calendar-day quota (00:00 UTC)
monthly  = 20000   # calendar-month quota (1st, 00:00 UTC)
queued   = 0       # max outstanding deliveries; 0 disables (default)

[projects.axis]
sinks = ["email"]

# Per-project override; any field left out keeps the global value.
[projects.axis.limits]
requests = 120
window   = "1m"
```

Built-in defaults (used when `[limits]` is omitted) are:

| Limit | Default | Meaning |
|---|---|---|
| `requests` / `window` | 60 / 1m | at most 60 submits per rolling minute, per project |
| `daily` | 2000 | at most 2000 submits per UTC calendar day |
| `monthly` | 20000 | at most 20000 submits per UTC calendar month |
| `queued` | 0 (off) | at most this many outstanding (pending or inflight) deliveries |

`queued` is off by default, so a self-hosted deployment that does not set it
behaves exactly as before.

A zero or omitted field means "inherit". There is no "unlimited" switch — set a
very high number (e.g. `requests = 1000000`) for a project you never want to
throttle. Negative limits and non-positive or unparseable windows fail config
validation at startup.

## Responses

Every response to an allowed submit carries the tightest remaining budget:

| Header | Meaning |
|---|---|
| `X-RateLimit-Limit` | the limit that is closest to exhaustion |
| `X-RateLimit-Remaining` | submits left in that limit's window |
| `X-RateLimit-Reset` | Unix seconds when that window resets |

A submit rejected by a rate or quota limit returns `429 Too Many Requests` with
the same headers, a `Retry-After` header in whole seconds, and the standard
error body:

```json
{"error":{"code":"rate_limited","message":"submit limit exceeded"}}
```

A submit rejected by the `queued` backlog cap returns `503 Service Unavailable`
with `{"error":{"code":"queue_full",…}}` instead (see [Queue
backpressure](#queue-backpressure)).

Rate limiting runs **after** authentication: an unknown, revoked, or
out-of-scope token always gets `401`, never `429`, so the response code cannot
be used to probe whether a token exists.

## How it works

Counters are partitioned by `(tenant, project)`, so two projects — or the same
project under two tenants — never share a budget. Three fixed windows are
tracked per partition:

- a per-project request window (`requests` / `window`),
- a UTC calendar day, and
- a UTC calendar month.

The first limit that is exhausted rejects the request and drives `Retry-After`.
Windows reset lazily when a request arrives after the boundary, and the in-memory
map of counters is capped; see below. The algorithm is a fixed window, not a
token bucket or sliding log: it is simple, has no background goroutines, and its
worst case (up to 2× the limit across a boundary) is fine for this workload.

## Queue backpressure

`queued` is a *backpressure* limit, not a rate: it caps how many deliveries a
project may have outstanding at once, counting both `pending` rows (waiting or
backing off) and `inflight` rows (claimed but not finished) in the durable
queue. When a project is at the cap, `POST /v1/events` is rejected before the
event is stored:

```http
HTTP/1.1 503 Service Unavailable
Retry-After: 30
X-RateLimit-Limit: 500

{"error":{"code":"queue_full","message":"the project delivery queue is congested; retry later"}}
```

This protects the shared dispatcher from one tenant's runaway backlog — the M2
"noisy neighbour" case — while a well-behaved client backs off and lets the
queue drain. It is enforced per `(tenant, project)`, so tenant A's congested
queue never throttles tenant B. Because the queue is shared across the API and
dispatcher processes, the depth is read from the database rather than a
process-local counter (which would be wrong across replicas). It is a soft cap:
a small overshoot under concurrent submits is expected.

## Local vs shared state

The shipped implementation is **in-memory and single-instance**
(`internal/adapters/memory.RateLimiter`). That matches how Herald runs today:
one `herald serve` process per deployment. Counters are process-local and reset
on restart, and the map of `(tenant, project)` counters is bounded — entries are
reset as their windows roll over and the oldest is evicted at a fixed cap.

The trade-off is explicit: **it is not correct for more than one API instance.**
If you run several `herald serve` replicas behind a load balancer, each keeps its
own counters, so the effective limit is `N × requests` and the numbers in the
`X-RateLimit-*` headers are per-instance. The enforcement point is an
application port (`app.RateLimiter`), so a shared-state implementation (for
example, counters in PostgreSQL or Redis with a scripted decrement) can be
substituted at wiring time without touching the HTTP layer or the domain. Until
then, run the API as a single instance, or size limits for the replica count.

## Tuning

- Limits are per project, so raise `[projects.<name>.limits]` for a busy CI
  project and keep the global default tight.
- The request window guards against bursts; the daily/monthly quotas guard
  against slow leaks. Set `daily` well above `requests × expected busy minutes`
  so normal traffic never hits the quota.
- `Retry-After` tells a well-behaved client exactly when to retry; the CLI and
  MCP surfaces surface the `429` and the header. Agents should back off, not
  hammer.
- Because limits are keyed by `(tenant, project)` and tenants come from
  credentials, you cannot raise your own limit by changing the request body.
