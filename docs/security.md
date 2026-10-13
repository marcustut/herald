# Security hardening

This document records the delivery-time security policies for Herald: how
webhook destinations are constrained (SSRF), how SMTP fails safe, and where
sensitive content is — and is not — persisted or logged. It complements
[SECURITY.md](../SECURITY.md) (reporting policy and trust boundaries) and
[architecture.md](architecture.md).

The model is single-owner for v1: the gateway, its config file, and its
database are trusted; **event content and every HTTP client are not**.

## Sinks are pinned server-side

A sink destination is resolved from the credential and the admin's
`gateway.toml`, never from a request body or event content. An agent cannot
choose where a notification goes, include an extra recipient, or add headers.
This is enforced by construction: `SubmitEvent` fans out to the project's
configured sinks only, and the event's free-text `links` are delivered as data
— they are never dialled.

- Email recipient: from `[sinks.*] recipient`.
- Webhook URL: from `[sinks.*] url`, validated at config load.

## Webhook SSRF policy

Because a webhook is an outbound HTTP request made by the server, a
misconfigured or attacker-influenced URL is a server-side request forgery
(SSRF) risk. Herald defends in depth:

1. **Admin-configured only.** There is no API, CLI, MCP, or event field that
   sets a webhook URL. It can only come from `gateway.toml`, which is trusted
   configuration.
2. **Scheme and host validation at load.** A webhook URL must be an absolute
   `http` or `https` URL with a host. `file:`, `gopher:`, `data:`, `javascript:`
   and protocol-relative targets are rejected when the config is read, before a
   sink is built.
3. **Optional explicit host allowlist.** Set `security.webhook_allow_hosts` to
   restrict webhooks to named hosts:

   ```toml
   [security]
   webhook_allow_hosts = ["hooks.slack.com", "alerts.example.com"]
   ```

   When the list is non-empty, any webhook sink whose host is not listed fails
   config validation. Matching is case-insensitive and ignores a port. An empty
   list permits any host, still subject to the dial-time guard below.
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

## Deployment notes

- Existing configs keep working: an absent `[security]` table means no host
  allowlist and no private networks.
- A webhook sink pointed at a loopback/private address now fails delivery
  (permanently) unless `security.allow_private_networks = true`. This is a
  deliberate change: opt in only for a receiver you trust on your own network.
- An SMTP host that previously accepted a cleartext remote session now fails.
  Configure a submission listener that offers `STARTTLS` (the common case), or
  point Herald at a loopback relay.
