# HTTP API (v1)

The canonical Herald contract for agents, hooks, CI and the CLI/MCP surfaces.
One endpoint accepts work: **`POST /v1/events`**. A machine-readable copy of this
document lives in [`openapi.yaml`](../openapi.yaml); keep the two in sync.

The examples use the placeholder host `https://herald.example.com`. Replace it
with your deployment's origin; the path and payload are identical everywhere.

- [Authentication](#authentication)
- [Submit an event](#submit-an-event)
- [Request fields](#request-fields)
- [Validation rules](#validation-rules)
- [Responses](#responses)
- [Error envelope](#error-envelope)
- [Processing order](#processing-order)
- [Idempotency and occurrences](#idempotency-and-occurrences)
- [Rate limiting](#rate-limiting)
- [The deprecated `/v1/reports` alias](#the-deprecated-v1reports-alias)
- [Health and metrics](#health-and-metrics)
- [Delivery guarantees](#delivery-guarantees)
- [Security boundaries](#security-boundaries)

## Authentication

Every submit carries a bearer token:

```
Authorization: Bearer herald_<project>_<secret>
```

Two token shapes exist, both managed by `herald token create` (see
[docs/cli.md](cli.md)):

| Shape | Scope | `project` in the body |
|---|---|---|
| `herald_<project>_<secret>` | fixes one project | optional; if present it must match the token’s project |
| `heraldacct_<secret>` | one or more projects | **required**; authorized against the token’s scope |

The project is resolved server-side from the credential. A body `project` is only
a hint the token must be authorized for — it never widens access, and the tenant
always comes from the credential. A missing, malformed, unknown, revoked or
out-of-scope token gets `401 unauthenticated`; the response never distinguishes
which of those it was, so a caller cannot probe for valid tokens.

## Submit an event

```http
POST /v1/events HTTP/1.1
Host: herald.example.com
Authorization: Bearer herald_axis_<secret>
Content-Type: application/json

{
  "project": "axis",
  "status": "success",
  "title": "deploy finished",
  "body": "main is green; 3 commits shipped",
  "links": [
    {"label": "PR", "url": "https://github.com/example/repo/pull/42"}
  ],
  "context": {"host": "ci-runner-7"},
  "agent": "herdr-build",
  "occurrenceId": "deploy-2026-10-13"
}
```

A successful accept returns `202 Accepted`:

```json
{"id": "evt_01JABC…", "deliveries": 1}
```

`202` means the event is durably stored and one delivery per configured sink has
been queued — **not** that a notification has been sent yet. Delivery is
asynchronous (see [Delivery guarantees](#delivery-guarantees)).

`deliveries` is how many delivery rows were queued. `0` means the project has no
enabled, verified destination, so nothing will be sent; the field is omitted on
an idempotent retry, where the original event's count is not re-derived.

## Request fields

| Field | Type | Required | Notes |
|---|---|---|---|
| `project` | string | scoped tokens only | Project slug. Must match a project token; must be in scope for a scoped token. |
| `status` | string | yes | One of `success`, `failure`, `blocked`. Case-insensitive; stored canonical (lowercase). |
| `title` | string | yes | Short summary. Leading/trailing whitespace is trimmed. |
| `body` | string | no | Free text. |
| `links` | array of `{label,url}` | no | Labelled references. |
| `context` | object (string → string) | no | Bounded metadata; `host` is the conventional key. |
| `agent` | string | yes | Slug identifying the agent. Over HTTP the gateway does not default it; the CLI and MCP tool fill a default before sending. |
| `occurrenceId` | string | no | Stable id for one logical event; a safe retry key. |
| `idempotencyKey` | string | no | Legacy retry key; accepted for backward compatibility. |

Unknown JSON properties are ignored. Sending both `occurrenceId` and
`idempotencyKey` is allowed; `occurrenceId` wins.

## Validation rules

Limits are byte lengths (not rune counts) and mirror `internal/domain`.

| Field | Limit |
|---|---|
| `title` | required after trimming; ≤ 200 bytes |
| `body` | ≤ 65536 bytes |
| `links` | ≤ 20 entries |
| `links[].label` | required; ≤ 64 bytes |
| `links[].url` | required; ≤ 2048 bytes; absolute `http`/`https` only (no `javascript:`, `data:`, or relative URLs) |
| `context` | ≤ 8 entries |
| `context` key | ≤ 32 bytes; `[a-z0-9_-]` only; not `agent`, `project`, `tenant` or `status` |
| `context` value | ≤ 128 bytes; no control characters |
| `agent` | required; lowercase alphanumerics with interior dashes; ≤ 64 bytes |
| `occurrenceId` | ≤ 124 bytes; `[A-Za-z0-9-_.:]` |
| `idempotencyKey` | ≤ 128 bytes; `[A-Za-z0-9-_.:]` |
| request body | ≤ 256 KiB (262144 bytes) by default, configurable server-side |

A body that is well-formed JSON but fails any of these is `422 validation_failed`
with the offending `field` set.

## Responses

| Status | Code | When |
|---|---|---|
| `202` | — | Event accepted; body `{"id": "evt_…", "deliveries": n}` (`deliveries` omitted on an idempotent retry). |
| `400` | `invalid_json` | Body could not be read, or is not valid JSON. |
| `401` | `unauthenticated` | Missing bearer token, or invalid/revoked/out-of-scope token. |
| `413` | `payload_too_large` | Body exceeds the server’s maximum. |
| `415` | `unsupported_media_type` | `Content-Type` is missing or not `application/json`. |
| `422` | `validation_failed` | Well-formed JSON that violates a [validation rule](#validation-rules). |
| `429` | `rate_limited` | Submit budget exhausted; see [Rate limiting](#rate-limiting). |
| `503` | `queue_full` | The project’s delivery queue has reached its configured backlog cap; retry later. |
| `503` | `unavailable` | A dependency (database, credential store) failed. Safe to retry. |

Unregistered paths return `404`, and a known path with the wrong method returns
`405` (with `Allow`), both handled by the standard library router.

## Error envelope

Every non-2xx submit returns the same JSON shape:

```json
{"error": {"code": "validation_failed", "message": "title is required", "field": "title"}}
```

| Field | Type | Notes |
|---|---|---|
| `code` | string | Stable, machine-readable. Switch on this, not on `message`. |
| `message` | string | Human-readable detail; may change between releases. |
| `field` | string | Present only for `validation_failed`; names the offending input field. |

`field` is one of `title`, `body`, `status`, `links`, `agent`, `context`,
`idempotencyKey`, `occurrenceId`.

## Processing order

The checks run in a fixed order, so a request can only ever produce the first
failure it hits:

1. If the path is `/v1/reports`, set the deprecation headers (all responses).
2. `Content-Type` must be `application/json` → otherwise `415`.
3. Read the body up to the size cap → `413`, or `400 invalid_json` if unreadable.
4. A bearer token must be present → otherwise `401`.
5. Parse the JSON → otherwise `400 invalid_json`.
6. Authenticate the token and resolve the project → otherwise `401` (or `503`).
7. Check the rate limit → otherwise `429`; or the queue backlog cap →
   `503 queue_full` (or `503` for a dependency failure).
8. Validate and store the event → `422`, or `503` for a dependency failure,
   otherwise `202`.

Two consequences worth knowing:

- **A malformed body is reported before authentication.** A request with a
  present-but-invalid token and broken JSON returns `400 invalid_json`, not
  `401`. Conversely, a *missing* token is `401` even with a broken body.
- **Rate limiting runs after authentication**, so an invalid token is always
  `401` and can never be used to probe whether a valid token exists via `429`.

## Idempotency and occurrences

Every submission is a **new event** by default, even when its text matches an
earlier one. To make a retry of one logical occurrence safe, send a stable
`occurrenceId`:

- Repeating the same `occurrenceId` returns the **original** event id and does
  not fan out twice.
- Deduplication is scoped to `(tenant, project)`, so two projects can reuse the
  same id without suppressing each other.
- `occurrenceId` and the legacy `idempotencyKey` share the same
  `(tenant, project, key)` namespace; `occurrenceId` takes precedence when both
  are set.
- With neither, the server mints a unique key, so identical content never
  collapses two events.

Idempotency protects **event creation**, not delivery. A `202` retry is safe;
deliveries remain at-least-once and a sink may still see a duplicate after a
crash (see [Delivery guarantees](#delivery-guarantees)).

## Rate limiting

Limits are enforced per `(tenant, project)`, after authentication. When a limit
is active, **every** submit response (success or rejection) carries the tightest
remaining budget:

| Header | Meaning |
|---|---|
| `X-RateLimit-Limit` | The limit closest to exhaustion. |
| `X-RateLimit-Remaining` | Submits left in that limit’s window. |
| `X-RateLimit-Reset` | Unix seconds when that window resets. |

An exhausted limit returns `429 rate_limited` with `Retry-After` in whole seconds
(never below `1`):

```http
HTTP/1.1 429 Too Many Requests
Retry-After: 17
X-RateLimit-Limit: 60
X-RateLimit-Remaining: 0
X-RateLimit-Reset: 1760000060

{"error":{"code":"rate_limited","message":"submit limit exceeded"}}
```

Clients should honour `Retry-After`. The shipped limiter is in-memory and
single-instance; see [docs/rate-limiting.md](rate-limiting.md) for the trade-off
and tuning.

### Queue backpressure

A project may also set a `queued` cap: the maximum number of deliveries it may
have outstanding (pending or inflight) at once. When the durable backlog reaches
the cap, a submit is rejected **before any event is stored** with
`503 queue_full` and `Retry-After`:

```http
HTTP/1.1 503 Service Unavailable
Retry-After: 30
X-RateLimit-Limit: 500

{"error":{"code":"queue_full","message":"the project delivery queue is congested; retry later"}}
```

The cap is **per `(tenant, project)`**, so one tenant's congested queue never
throttles another. It is off unless configured.

## The deprecated `/v1/reports` alias

`POST /v1/reports` is a **deprecated alias** for `POST /v1/events`. It accepts
the identical request and returns the identical response, with two extra headers
on every response (including errors) so clients can migrate:

```http
Deprecation: true
Link: </v1/events>; rel="successor-version"
```

There is no separate Report resource, store or delivery pipeline — the alias
dispatches to the same handler. New integrations must use `/v1/events`.

## Health and metrics

| Endpoint | Method | Response |
|---|---|---|
| `/healthz` | GET | `200` `ok` — liveness; never touches the database. |
| `/readyz` | GET | `200` `ready`, or `503 unavailable` when not ready. |
| `/metrics` | GET | Prometheus text exposition. |

See [docs/observability.md](observability.md) for metric names and alerting.

## Delivery guarantees

Successful submits are delivered by a separate `herald dispatch` process:

- **At-least-once.** A delivery is attempted until it succeeds, is classified
  permanent, or exhausts its attempts (default 8) and becomes `dead`. A crashed
  worker’s lease is reclaimed and retried, so a sink can occasionally receive a
  duplicate. Sinks receive an `Idempotency-Key` header to help deduplicate.
- **Per-attempt backoff** between retries.
- **Sink destinations are fixed server-side.** The project’s sinks come from
  `gateway.toml`; the request body cannot choose or redirect a recipient.
- Operate the queue with `herald delivery …` and
  [docs/runbook-deliveries.md](runbook-deliveries.md).

## Security boundaries

- **Recipients are pinned server-side** from the token and the gateway config.
  An agent sets no destination headers and cannot redirect mail or webhooks.
- **Secrets are redacted at ingest.** Secret-looking substrings are stripped
  from `title`, `body`, link URLs and context values before the event is stored
  or delivered; the database holds only the redacted copy. This is a safety net,
  not a licence to send secrets.
- **Body content is untrusted.** Titles, bodies, links and context are
  attacker-controlled data. They are never interpreted as instructions by the
  gateway, and downstream agents must treat them the same way.
- **Tokens are hashed at rest** (SHA-256) and shown once at issue time. The raw
  token is never logged or stored.
