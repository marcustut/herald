# Deliverability: sender policy, bounces and suppressions

Herald sends over one shared SMTP relay — Stalwart on the host, which relays
through SES — so the sending reputation is shared by every tenant. This document
is the owner's checklist for making that reputation trustworthy: the DNS records
to publish, the sender-policy model, and how provider bounce/complaint
notifications feed the platform-wide suppression list.

**Herald changes no DNS and no mail-server configuration.** Everything in
[Publish these records](#publish-these-records) is done by the owner at the DNS
provider and in the SES/SNS and Stalwart consoles. Herald only consumes the
bounce and complaint notifications this produces.

## Sender-policy model

One deployment sends from **one** `From` domain, configured as
`smtp.from` (`herald@mail.example.com`). Every tenant shares that domain; the
`From` display name is the only per-message variation. This keeps the policy
simple and the reputation pooled — which is exactly why the suppression list is
global rather than per tenant.

The pieces that must line up for DMARC to pass:

| Header / identity | Value | Set by |
|---|---|---|
| `From:` domain | `mail.example.com` | `smtp.from` |
| Envelope `MAIL FROM` / `Return-Path` | `bounce.mail.example.com` | the relay's "custom MAIL FROM domain" (SES) |
| SPF | authorises SES's sending IPs (and the host's relay if it sends directly) | the SPF TXT record |
| DKIM | signed by the sending domain's key | the DKIM record(s) |
| DMARC | `p=…` aligned to `mail.example.com` | the DMARC TXT record |

DMARC passes on **alignment**: the DKIM signature domain (or the SPF domain)
must match the `From` domain. DKIM survives forwarding and is the durable
signal, so publish DKIM and treat SPF as the secondary check.

Use a subdomain (`mail.example.com`), not the organisational root
(`example.com`), so transactional mail's reputation does not drag down the
domain a human emails from. If the root must send, widen DMARC slowly:
`p=none` → `p=quarantine` → `p=reject`.

## Publish these records

Replace `mail.example.com` and `example.com` with the real sending domain and
organisation domain. `dig` each record after publishing; DNS can take up to the
record's TTL to propagate.

### 1. SPF (TXT)

Authorises the relays that may send as the domain. Publish **one** SPF record
per domain: `v=spf1`.

```dns
mail.example.com.  TXT  "v=spf1 include:amazonses.com -all"
```

- `include:amazonses.com` covers SES, which Stalwart relays through.
- `-all` (hard fail) is correct when the list is complete. Use `~all` while you
  are still discovering senders, then tighten.
- If the Stalwart host sends directly rather than through SES, add that host's
  address (`ip4:203.0.113.10`) before `-all`.
- Never publish two `v=spf1` records on one name; merge them.

### 2. DKIM (CNAME, or TXT)

SES Easy DKIM gives three `CNAME` records per signing domain; publish all three
verbatim. Herald does not generate keys.

```dns
<token1>._domainkey.mail.example.com.  CNAME  <token1>.dkim.amazonses.com.
<token2>._domainkey.mail.example.com.  CNAME  <token2>.dkim.amazonses.com.
<token3>._domainkey.mail.example.com.  CNAME  <token3>.dkim.amazonses.com.
```

Copy them from **SES → Verified identities → (domain) → DKIM**, then wait for
SES to report **Verified**. A manual DKIM setup publishes a single
`TXT` public key at `<selector>._domainkey.mail.example.com` instead.

### 3. Custom MAIL FROM (MX and TXT)

SES returns the envelope sender on the custom MAIL FROM domain, which is what
SPF checks and where bounces are returned. Configure it in **SES → Verified
identities → (domain) → MAIL FROM domain**, then publish the records SES shows:

```dns
bounce.mail.example.com.        MX  10 feedback-smtp.<region>.amazonses.com.
bounce.mail.example.com.        TXT "v=spf1 include:amazonses.com -all"
```

### 4. DMARC (TXT)

Tells receivers what to do when SPF and DKIM both fail and where to send
aggregate reports.

```dns
_dmarc.mail.example.com.  TXT  "v=DMARC1; p=none; rua=mailto:dmarc@example.com; adkim=s; aspf=s"
```

- Start at `p=none` with a reporting address, watch the aggregate reports, then
  move to `p=quarantine` and eventually `p=reject`.
- `adkim=s; aspf=s` (strict alignment) is safest once DKIM and the custom MAIL
  FROM domain are verified.
- DMARC does not fix deliverability by itself; it stops the domain being spoofed
  while you fix the alignment under it.

### Optional: BIMI and MTA-STS

Out of scope for this issue. BIMI needs a paid Verified Mark Certificate;
MTA-STS/TLS-RPT harden inbound transport, not outbound reputation.

## Wire bounces and complaints

A hard bounce or a complaint means "stop sending to this mailbox". SES reports
both asynchronously, after the SMTP session has already succeeded, so the
delivery path cannot see them. Herald exposes an authenticated ingestion
endpoint for them.

### Owner steps

1. **Enable the route.** In `gateway.toml`:

   ```toml
   [cloud]
   enabled = true
   base_url = "https://cloud.example.com"
   bounce_webhook = true
   ```

2. **Set the shared secret** (never the config file):

   ```sh
   HERALD_BOUNCE_WEBHOOK_SECRET="$(openssl rand -hex 32)"
   ```

   Configure it on the service that runs the HTTP API (`herald serve`). Herald
   refuses to start when `bounce_webhook` is on and the secret is empty.

3. **Point the provider at**
   `https://cloud.example.com/v1/email/events`. It must be reachable from
   the provider, so it lives on the public origin — not on a loopback interface.
   The path 404s when `bounce_webhook` is off or the secret is unset, exactly
   like any other absent cloud route.

4. **Authenticate every request** with
   `Authorization: Bearer $HERALD_BOUNCE_WEBHOOK_SECRET`. The secret is
   compared in constant time; there is no other credential.

**SES via SNS.** Create an SNS topic, subscribe the HTTPS endpoint, and add a
bounce/complaint event destination to the SES configuration set that Stalwart
relays through. SNS first posts a `SubscriptionConfirmation`; Herald
acknowledges it (`202`) but **does not fetch the `SubscribeURL`** — that would
be an unauthenticated outbound request. Confirm the subscription yourself in
the SNS console, then publish the topic.

**Any other relay.** POST the generic shape directly; no SDK or new dependency
is involved.

### Accepted payloads

Generic shape:

```json
{"id": "evt-123", "type": "bounce", "recipient": "user@example.com", "permanent": true}
```

- `type`: `bounce`, `complaint`, or `delivery` (case-insensitive; `delivered`,
  `bounced`, `complained`/`spam` are accepted aliases).
- `permanent`: a hard bounce suppresses; a transient bounce (`false`, the
  default when omitted) is recorded but does **not** suppress.
- `id`: the provider's event id. Ingestion is idempotent by it.

SES-via-SNS shape (the notification is a JSON string inside `Message`):

```json
{
  "Type": "Notification",
  "MessageId": "sns-1",
  "Message": "{\"notificationType\":\"Bounce\",\"bounce\":{\"bounceType\":\"Permanent\",\"bouncedRecipients\":[{\"emailAddress\":\"user@example.com\"}]}}"
}
```

`notificationType` may be `Bounce`, `Complaint` or `Delivery`; only
`bounceType: Permanent` and every `Complaint` suppress.

### Replay safety

The endpoint is idempotent by provider event id. Each event is inserted into
`email_provider_events`; the suppression is applied in the **same transaction**
and only when the insert is new, so:

- a replayed webhook (SNS retries until it gets a `2xx`) is a no-op;
- a crash between recording and suppressing cannot half-apply;
- a provider that omits an id still dedupes by `(kind, address)`;
- one notification naming several recipients gets one key per address, so the
  first address cannot swallow the rest.

The response reports `{"accepted": n}`, where `n` is how many events were newly
applied (`0` on a replay). Subscription confirmations and notifications with no
recipients are acknowledged with `accepted: 0`.

### What suppression does

When an address is suppressed, the **hosted** delivery path refuses it at
delivery time (checked live, so a suppression added after an event was queued
still takes effect). The refusal is permanent: the delivery is dead-lettered
immediately with no retry, and counted as `outcome="suppressed"`.

Suppression is **not** applied to the config-built (self-host) sink, which has
no database and no shared reputation to protect.

To remove a suppression after a mailbox is fixed:

```sql
DELETE FROM suppressions WHERE address = 'user@example.com';
```

Then redrive the affected dead letters (`herald delivery requeue --sink <id>`).
This is a deliberate, audited break-glass: a wrong removal re-burns reputation.

## Verify it works

```sh
# Route present only when configured (expect 401 without the secret, not 404).
curl -si -X POST https://cloud.example.com/v1/email/events
# Authenticated transient bounce: accepted, no suppression.
curl -si -X POST https://cloud.example.com/v1/email/events \
  -H "Authorization: Bearer $HERALD_BOUNCE_WEBHOOK_SECRET" \
  -H 'Content-Type: application/json' \
  -d '{"id":"test-1","type":"bounce","recipient":"test@example.com","permanent":false}'
# Permanent bounce or complaint: accepted, address suppressed.
curl -si -X POST https://cloud.example.com/v1/email/events \
  -H "Authorization: Bearer $HERALD_BOUNCE_WEBHOOK_SECRET" \
  -H 'Content-Type: application/json' \
  -d '{"id":"test-2","type":"complaint","recipient":"test@example.com"}'
# Replay the same id: accepted is 0.
```

Watch `herald_email_events_total`, `herald_email_suppressions_total` and
`herald_delivery_outcomes_total{outcome="suppressed"}` on `/metrics` — see
[observability.md](observability.md).
