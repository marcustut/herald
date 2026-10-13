# Security hardening

This document records the delivery-time security policies for Herald: how
webhook destinations are constrained (SSRF), how SMTP fails safe, and where
sensitive content is — and is not — persisted or logged. It complements
[SECURITY.md](../SECURITY.md) (reporting policy and trust boundaries) and
[architecture.md](architecture.md).

The model is single-owner for v1: the gateway, its config file, and its
database are trusted; **event content and every HTTP client are not**.

## Destinations are pinned server-side

A destination is resolved from the credential and the project's stored
destination rows, never from a request body or event content. An agent cannot
choose where a notification goes, include an extra recipient, or add headers.
This is enforced by construction: `SubmitEvent` fans out to the project's
enabled, verified destinations only, and the event's free-text `links` are
delivered as data — they are never dialled.

- Email recipient: from the stored `email` destination's target.
- Webhook URL: from the stored `webhook` destination's target, re-checked at
  delivery time.

Destinations are created by the authenticated project owner over the API or
`/dashboard`; there is no `gateway.toml` routing and no request field that names
one.

## Webhook SSRF policy

Because a webhook is an outbound HTTP request made by the server, a
misconfigured or maliciously-chosen URL is a server-side request forgery (SSRF)
risk. Herald defends in depth:

1. **Authenticated owner only.** A webhook URL can only be created by the
   account that owns the project, over the session-authenticated API (or the
   `/dashboard` page). Agents and event payloads have no path to it.
2. **Scheme and host validation at creation.** A webhook target must be an
   absolute `http` or `https` URL with a host. `file:`, `gopher:`, `data:`,
   `javascript:` and protocol-relative targets are rejected when the destination
   is created.
3. **Optional explicit host allowlist.** Set `security.webhook_allow_hosts` to
   restrict webhook destinations to named hosts:

   ```toml
   [security]
   webhook_allow_hosts = ["hooks.slack.com", "alerts.example.com"]
   ```

   When the list is non-empty, a webhook destination whose host is not listed is
   not delivered to (its delivery dead-letters). Matching is case-insensitive
   and ignores a port. An empty list permits any host, still subject to the
   dial-time guard below.
4. **Redirects are never followed.** The webhook client sets `CheckRedirect` to
   `http.ErrUseLastResponse`, so a `3xx` is surfaced to the dispatcher as a
   permanent failure and the redirect target is never contacted. This closes
   the "public URL redirects to `169.254.169.254`" bypass.
5. **Non-public addresses are blocked at dial time.** A custom `DialContext`
   resolves the host itself and refuses the connection when **every** resolved
   address is non-public — loopback, RFC 1918 / ULA private, link-local
   (including the cloud metadata service `169.254.169.254`), carrier-grade NAT
   (where some clouds host metadata, e.g. `100.100.100.200`), multicast, or
   unspecified. The resolved IP is dialled directly, so a DNS record cannot
   rebind between the check and the connection.
6. **No environment proxy.** The webhook transport ignores `HTTP_PROXY` /
   `HTTPS_PROXY`. A proxy would otherwise resolve and reach the target on the
   server's behalf, bypassing the dial-time guard.

The dial-time block is the load-bearing control: even a URL that passes
validation cannot reach a private or metadata address unless the operator
explicitly opts in with `security.allow_private_networks = true` (for a
receiver on a trusted internal network).

## SMTP policy

Email delivery fails safe for remote transports:

- **STARTTLS is required for non-loopback hosts.** If a remote server does not
  offer `STARTTLS`, the transport refuses the session. It does not silently
  continue in cleartext.
- **Certificates are verified.** `STARTTLS` uses TLS ≥ 1.2 and verifies the
  server certificate against `smtp.server_name` (or the dialled host). There is
  no `InsecureSkipVerify` path.
- **No silent plaintext auth.** Because the session is refused before `AUTH`
  when STARTTLS is unavailable, credentials are never sent in the clear to a
  remote host.
- **Loopback is the exception.** A submission listener on `127.0.0.1`,
  `::1`, or `localhost` (for example Stalwart on the box) may speak plaintext
  with PLAIN auth; such traffic does not leave the host.

`formatFrom` also strips CR/LF from the display name and quotes or encodes it,
and the `Subject` is RFC 2047 encoded, so event content cannot inject mail
headers.

## Outbound email safety: suppression and provider webhooks

Deliverability is part of the trust boundary: one shared relay means one
address that bounces hard can damage every tenant's sending. Herald therefore:

- **Suppresses globally.** A hard bounce or a complaint adds the address to a
  platform-wide suppression list. The hosted delivery path checks it live, so a
  suppression added after an event was queued still takes effect, and refuses a
  suppressed address permanently — dead-lettered with no retry. Unverified and
  removed destinations are refused the same way (see [Destinations are pinned
  server-side](#destinations-are-pinned-server-side)).
- **Authenticates ingestion.** The provider bounce/complaint endpoint
  (`POST /v1/email/events`) is off unless `[auth] bounce_webhook` is set,
  and requires the shared secret from `HERALD_BOUNCE_WEBHOOK_SECRET`, compared
  in constant time. It 404s like any absent cloud route when unconfigured, so a
  probe cannot tell it apart from a route that does not exist.
- **Is replay-safe.** Ingestion is idempotent by provider event id, in one
  transaction with the suppression, so a retried or duplicated webhook cannot
  re-apply its effect.
- **Never fetches provider URLs.** An SNS `SubscriptionConfirmation` is
  acknowledged without fetching its `SubscribeURL`, closing the
  unauthenticated-request shape. The owner confirms the subscription in the SNS
  console.
- **Never logs the mailbox or payload.** The provider body is not persisted;
  only the fields Herald acts on (kind, address, permanence, event id) reach the
  database. SMTP transport errors have the recipient address stripped before
  they reach a log line or a stored `last_error`, while the bounce
  classification is preserved. Metric labels stay bounded (`kind`, `reason`),
  never an address.

The suppression list is deliberately global, not tenant-scoped: it is a
deliverability safeguard for the shared sending identity, not tenant data. See
[deliverability.md](deliverability.md) for the owner's DNS and wiring steps.

## Content: redact at ingest, never log bodies

Event content is untrusted free text and may contain credentials. Herald:

- **Redacts before persistence.** `SubmitEvent.Handle` calls `Event.Redact`
  before saving, so the database holds the redacted copy. The same redacted
  event is what the dispatcher renders and delivers. `domain.Redact` strips
  PEM private keys, cloud/API token shapes (AWS, GitHub, Slack, `sk-…`), JWTs,
  URL-embedded credentials, `Bearer`/`Basic` values, and `key: value` secrets.
- **Does not log content.** Logs and metrics use identifiers only. The
  dispatcher logs `sink` and `event` ids and the error string (a status code or
  transport error), never the title, body, links, or context. HTTP metrics
  labels are bounded to route, method, status, sink id, and delivery outcome —
  never a token, project id, or event field.
- **Bounds untrusted fields.** Titles, bodies, link counts/lengths, and
  context entries/keys are length- and character-validated, and context keys
  that would fake provenance (`agent`, `project`, `tenant`, `status`) are
  rejected.

Link URLs are restricted to absolute `http`/`https` at ingest, so `javascript:`
and `data:` URLs cannot reach rendered output.

## Adversarial coverage

The following behaviours are covered by automated tests:

| Area | Test |
|---|---|
| Non-public IP classification (loopback, private, link-local, metadata, multicast) | `internal/adapters/sink` `TestBlockedIP` |
| Webhook refused to a loopback/private target, and allowed only when opted in | `TestWebhookRefusesPrivateTarget`, `TestWebhookAllowsPrivateWhenConfigured`, `TestBuildWiresPrivateNetworkPolicy` |
| Redirects not followed | `TestWebhookDoesNotFollowRedirect` |
| Malicious URLs (`file:`, `gopher:`, `javascript:`, metadata IP, IPv6 loopback) | `TestWebhookMaliciousURLs` |
| Shared webhook client under concurrency | `TestWebhookConcurrentDeliver` |
| Webhook scheme/host validation and allowlist at config load | `internal/config` `TestWebhookURLValidation`, `TestSecurityWebhookAllowlist` |
| STARTTLS required for non-loopback SMTP; nothing sent | `TestSMTPTransportRefusesPlaintextRemote` |
| CRLF header injection in the subject | `TestEmailSubjectHeaderInjection` |
| HTML escaping of body markup | `TestEmailBuildMessageEscapesHTML` |
| Secret redaction (tokens, keys, URL credentials) | `internal/domain` redaction tests |
| Unverified destination is never routed to | `internal/adapters/postgres` `TestDestinationRoutingRequiresVerification` |
| Verification token is single-use, expires, and cascades with its destination | `TestDestinationVerificationTokens` |
| Destination ownership comes from the session, and CSRF is enforced | `internal/adapters/httpapi` `TestCloudDestinationOwnershipComesFromSession`, `TestCloudDestinationRequiresSessionAndCSRF` |
| A suppressed address is refused permanently and never delivered to | `internal/adapters/sink` `TestDestinationSourceRefusesSuppressed` |
| A suppression-store outage is retryable, not a silent drop | `TestDestinationSourceSuppressionStoreErrorIsRetryable` |
| SMTP errors redact the recipient but keep the bounce classification | `TestEmailRedactsRecipientFromSMTPError` |
| Bounce webhook is absent unless configured, and rejects a missing/wrong secret | `internal/adapters/httpapi` `TestEmailEventRouteAbsentWithoutUseCaseOrSecret`, `TestEmailEventRequiresSecret` |
| Replayed provider event is a no-op; transient bounce does not suppress | `TestEmailEventReplayIsIdempotent`, `TestEmailEventTransientBounceDoesNotSuppress` |
| SNS subscription confirmation is acknowledged without fetching its URL | `TestEmailEventSubscriptionConfirmationIsAcknowledged` |
| Suppression ingestion is idempotent under concurrency | `internal/adapters/postgres` `TestSuppressionsIngestConcurrent` |

## Deployment notes

- Existing configs keep working: an absent `[security]` table means no host
  allowlist and no private networks.
- A webhook sink pointed at a loopback/private address now fails delivery
  (permanently) unless `security.allow_private_networks = true`. This is a
  deliberate change: opt in only for a receiver you trust on your own network.
- An SMTP host that previously accepted a cleartext remote session now fails.
  Configure a submission listener that offers `STARTTLS` (the common case), or
  point Herald at a loopback relay.
