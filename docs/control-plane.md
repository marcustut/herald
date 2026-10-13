# Control plane: sign-in, sessions and deletion

The control plane shares one binary, one host and one `/v1` API — the same
versioned surface as event submission. The account surface is part of the
product: whenever a database is configured the control-plane routes are
registered. A `--dev` run (no database) has no account surface, so those paths
answer `404`, exactly as if the feature did not exist.

See [ADR 0001](adr/0001-multi-tenant-ownership.md) for the ownership model
(account → user / tenant → project → destination) this sits on top of.

## Configuring it

```toml
[auth]
base_url = "https://cloud.example.com"
signup = true

[smtp]
addr = "smtp.example.com:587"
from = "no-reply@example.com"
```

`base_url` is the public base under which the API is served; the magic-link
callback path (`/v1/auth/callback`) is appended to it. It is an absolute URL and
may include a path prefix such as `/api`. The server refuses to start without a
valid `base_url` and a usable `[smtp]` `addr`/`from`, because the account surface
cannot mail a magic link without them. An `https` origin makes the session
cookie `Secure`. The surface requires a database; the in-memory `--dev` mode has
none and registers no account routes.

`signup` (default `true`) gates self-service registration. With `signup = true`
a request for any valid address mails a link and redeems into a new account.
With `signup = false` a request for an address that has **no account** is
silently accepted (still `202`) but mails nothing and creates no rows, while an
**existing** account still receives its link. This lets an operator run a
closed deployment — provision accounts out of band, then let them sign in —
without ever revealing whether an address is registered.

Signing in the first time provisions one **account**, one **user** (keyed by the
email) and one default **tenant**. The session carries that tenant, so hosted
requests are scoped to the user's own tenant.

## Routes

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `POST` | `/v1/auth/magic-link` | none | Request a single-use sign-in link for `{"email":"..."}`. Always `202` for a valid address. |
| `GET`  | `/v1/auth/callback?token=...` | link token | Redeem the link, set the session cookie, return the CSRF token. |
| `GET`  | `/v1/me` | session | Return the signed-in user. |
| `POST` | `/v1/auth/signout` | session + CSRF | Revoke the session and clear the cookie. |
| `DELETE` | `/v1/account` | session + CSRF | Delete the account and everything it owns. |
| `GET`  | `/dashboard` | session | Self-contained HTML page for projects and tokens. |
| `GET`  | `/v1/projects` | session | List the tenant's projects. |
| `POST` | `/v1/projects` | session + CSRF | Create a project from `{"id":"slug","name":"..."}`. |
| `DELETE` | `/v1/projects/{project}` | session + CSRF | Delete a project (and its destinations). |
| `GET`  | `/v1/tokens` | session | List token metadata (never the secret). |
| `POST` | `/v1/tokens` | session + CSRF | Create a multi-project token from `{"projects":["a","b"]}`. |
| `DELETE` | `/v1/tokens/{id}` | session + CSRF | Revoke token `id` (its one-way hash). |
| `POST` | `/v1/projects/{project}/token` | session + CSRF | Create a project token; the raw secret is returned once. |
| `POST` | `/v1/projects/{project}/token/rotate` | session + CSRF | Issue a new project token and revoke the old one(s). |

A successful callback returns:

```json
{
  "csrfToken": "…",
  "user": {"id": "usr_…", "email": "you@example.com", "accountId": "acct_…", "tenantId": "ten_…"}
}
```

and sets `herald_session`.

## Destination configuration and verification

A signed-in tenant manages each project's delivery destinations. Every route
takes the tenant **and** project from the session and the path; the request body
never names an owner, and a project that belongs to another tenant resolves to
`404` as if it did not exist.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET`  | `/v1/projects/{project}/destinations` | session | List the project's destinations. |
| `POST` | `/v1/projects/{project}/destinations` | session + CSRF | Create one (`{"kind":"email"\|"webhook","target":"..."}`). |
| `DELETE` | `/v1/projects/{project}/destinations/{id}` | session + CSRF | Remove a destination. |
| `POST` | `/v1/projects/{project}/destinations/{id}/verify` | session + CSRF | Mail a fresh verification link (rate limited per IP). |
| `GET`  | `/v1/destinations/verify?token=...` | link token | Confirm an email address. |

- **Email ownership must be proved.** Creating an email destination sends a
  single-use, hashed, short-lived link to the address; only when the mailbox
  owner redeems it is the destination `verified`. An unverified (or expired)
  destination is **never delivered to**: hosted routing resolves a project's
  sinks from the database and includes only enabled, verified rows.
- **Webhooks are verified by configuration.** A webhook URL has no mailbox to
  prove; the authenticated owner supplying it is the proof. The dial-time SSRF
  guard still applies.
- **Confirmation is token-only**, like the sign-in callback: the recipient may
  not hold a session. The token row carries the owning `(tenant, project)` and
  destination id, so redemption verifies exactly the destination it was minted
  for, never one named by the request.
- **Removal cascades.** Deleting a destination removes its outstanding
  verification tokens, so a stale link cannot resurrect it.

Hosted and self-host deployments share one routing model: the API wires
`postgres.Ownership.EnabledSinks` as the sink directory, so a project's events
fan out only to enabled and verified destinations. The dispatcher then resolves
each queued delivery against the **current** destination row
(`sink.DestinationSource`): a destination that is removed, disabled or
unverified after the event was queued is not delivered to, and its delivery
dead-letters. There is no `gateway.toml` routing: a project with no destinations
fans out to nothing. Email rows build the SMTP sink (including the 5xx bounce
classification); webhook rows POST through the same hardened, SSRF-guarded
client.

## Magic-link security model

- **Single-use.** Redemption is `UPDATE … WHERE used_at IS NULL … RETURNING`:
  the row is atomically claimed, so a replay (or two concurrent redemptions)
  yields at most one session. A replayed link answers `401`.
- **High entropy.** The token is a 256-bit value from `crypto/rand`, so it is
  not guessable and needs no additional key-derivation cost.
- **Hashed at rest.** Only the SHA-256 hash is stored; the raw token exists only
  in the emailed link. The store is looked up by hash and the result is
  re-checked in constant time.
- **Short expiry.** Links expire 15 minutes after they are minted. An expired or
  unknown token is collapsed into the same `401` as a wrong one.
- **Never logged.** The token is not an error value, not a metric label and not
  part of the logged route; request logging records the matched route pattern,
  never the query string.
- **No enumeration.** The request endpoint answers `202` for any syntactically
  valid address, registered or not, and provisions nothing until a link is
  redeemed. With `signup = false` an unknown address still answers `202` but is
  neither mailed nor stored, so the response cannot be used to probe the
  account list.

### Email verification

There is no separate verification step: possession of the magic link, delivered
to the inbox, *is* the proof of mailbox control, and the account/user/tenant are
created only when the link is redeemed. An address that never receives or
redeems a link leaves no rows behind. This is the documented email-verification
route for the hosted product.

## Session and CSRF model

- The session cookie is `HttpOnly`, `Secure` (on the https origin), `SameSite=Strict`
  and `Path=/`, with the session lifetime as `Max-Age`.
- The session value is a 256-bit secret; only its hash is stored. It expires on
  its own and is revoked on sign-out.
- **Rotation.** Sessions are never reused or derived from a link: every sign-in
  mints a brand-new session secret, so a leaked link cannot fix or predict a
  session id (session-fixation defence), and sign-out revokes the one in use.
- **CSRF.** Every state-changing cloud request (`signout`, account deletion)
  must carry `X-CSRF-Token` matching the token returned at sign-in. The
  comparison is constant time. The token is a synchronizer secret stored only as
  a hash alongside the session, so a cross-site form cannot act with the cookie.
- **Revocation.** Sign-out revokes that session. Account deletion revokes every
  session in the account (and cascades the rows away).

## Rate limits

`POST /v1/auth/magic-link` consumes one unit per client IP and one per
target email in a fixed window (10/minute by default), answering `429` with
`Retry-After` when exhausted. The per-IP key is the transport peer — forwarded
headers are not trusted, so a caller cannot spoof its way around the limit.

## Account deletion

`DELETE /v1/account` (session + CSRF) deletes the account. Foreign keys
cascade, so the user, tenants, projects, destinations and all sessions are
removed in the same transaction; the response is `204` and the cookie is
cleared. Signing in again with the same address provisions a fresh account.
Account deletion is permanent — there is no export or soft-delete yet (see
[ADR 0001](adr/0001-multi-tenant-ownership.md) open question 4).

## Projects and tokens

Once signed in, a user manages their own projects and API tokens over the
session cookie. Ownership is always taken from the session's tenant — never from
a request body — so one account can never read, mutate, or mint a token for
another's project even knowing its slug. A cross-tenant address answers `404`,
the same as a missing row.

Creating a token returns the raw secret exactly once (`herald_...` for a
project token, `heraldacct_...` for a multi-project one) with `Cache-Control:
no-store`; only its hash is persisted, so the metadata list
(`GET /v1/tokens`) reports an id, kind, projects, created, last-used and
revoked time but can never reveal the secret. **Rotation** issues a replacement
and revokes the previous project token(s) in one operation; **revocation** by id
is immediate, because the sender's credential lookup sees the revoked row on its
next request.

The HTML page at `/dashboard` is a thin stdlib template over these JSON
routes. It authenticates with the session cookie and sends the CSRF token
(`X-CSRF-Token`) on every mutation; it accepts the token from the sign-in hash
(`#csrf=…`) or from a paste field (`herald login` redeems the callback directly
and never needs the page).
Every successful mutation is audited through the structured logger with the
actor, tenant, project and action — never a raw secret.

## Bounces, complaints and suppression

Provider bounce and complaint notifications are ingested at
`POST /v1/email/events`, shared-secret authenticated and replay-safe by
provider event id. A hard bounce or a complaint adds the address to the
platform-wide suppression list; the hosted delivery path then refuses it
permanently. The route is only mounted under `[auth] bounce_webhook = true`
with `HERALD_BOUNCE_WEBHOOK_SECRET` set. See
[deliverability.md](deliverability.md) for the DNS records and the owner's
wiring steps.

## CLI login (`herald login`)

A hosted account can onboard a machine from the CLI without an operator and
without a browser. The flow reuses the #29 magic link and the #30 project token —
it adds no new credential type and no new server route:

```sh
herald login --gateway https://cloud.example.com --project axis --email you@example.com
```

1. The CLI `POST`s `{"email":"you@example.com"}` to
   `/v1/auth/magic-link` and prints an instruction to paste the emailed
   link (or its token). It does not open the link, because the link is
   single-use and the CLI is the one that redeems it.
2. The CLI `GET`s `/v1/auth/callback?token=…`, holding the session cookie
   in memory, and reads the CSRF token from the response.
3. It lists `/v1/projects`; if `--project axis` is missing it creates it
   (`POST /v1/projects`), so a brand-new account is provisioned with a
   project in the same step.
4. It mints a **single-project token** with
   `POST /v1/projects/axis/token` (least privilege), drops the session, and
   writes the token to the `0600` credential file. The secret is never printed
   and never stored by the client except in that file.

The first cloud email still needs a verified destination: add an email
destination for the project and redeem its verification link. The token pins the
recipient server-side, so the destination is the only thing between a fresh
account and its first email.

On a self-hosted gateway, pass a token the operator issued (for example with
`herald token create --tenant <id> --project axis`) or copy one from a secret
store:

```sh
herald login --gateway http://localhost:8787 --project axis --token "$HERALD_TOKEN"
```

Run `herald login … --test` to submit one event and confirm delivery. See
[docs/cli.md](cli.md#herald-login) for the flags, the credential-file precedence
and the noninteractive CI path.

## Self-host compatibility

A self-hosted deployment configures `[auth] base_url` and a usable `[smtp]`
just like the hosted one — accounts are part of the product, not a hosted-only
switch. The migration that adds the auth tables is purely additive, so an
existing Event database upgrades with no data loss. To run closed, set
`signup = false` and provision accounts out of band: an unknown address is
accepted but never provisioned or mailed, and existing accounts still sign in.

## Deferred

Not part of this milestone and deliberately not built here: the remaining quota
work (#35). The project/token self-service surface above is #30, the dashboard is
#33 and `herald login` is #34. Token `last_used_at` is part of the metadata
contract but is not yet written on authentication, so it currently always reads
empty.
