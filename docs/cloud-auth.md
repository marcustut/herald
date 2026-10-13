# Cloud accounts: sign-in, sessions and deletion

Herald Cloud is the hosted control plane. It shares one binary, one host and one
database with self-hosting; the account surface is compiled in but **gated** by
config. With `cloud.enabled` unset or false (the self-host default) none of the
routes below are registered and every path answers `404`, exactly as if the
feature did not exist.

See [ADR 0001](adr/0001-multi-tenant-ownership.md) for the ownership model
(account → user / tenant → project → destination) this sits on top of.

## Enabling it

```toml
[cloud]
enabled = true
base_url = "https://cloud.example.com"

[smtp]
addr = "smtp.example.com:587"
from = "no-reply@example.com"
```

`base_url` is the absolute public origin the magic-link callback points at. The
hosted server refuses to start if `cloud.enabled` is true without a valid
`base_url` and a usable `[smtp]` `addr`/`from`. An `https` origin makes the
session cookie `Secure`; the surface also requires a database (the in-memory
`--dev` mode is refused).

Signing in the first time provisions one **account**, one **user** (keyed by the
email) and one default **tenant**. The session carries that tenant, so hosted
requests are scoped to the user's own tenant.

## Routes

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `POST` | `/cloud/v1/auth/magic-link` | none | Request a single-use sign-in link for `{"email":"..."}`. Always `202` for a valid address. |
| `GET`  | `/cloud/v1/auth/callback?token=...` | link token | Redeem the link, set the session cookie, return the CSRF token. |
| `GET`  | `/cloud/v1/me` | session | Return the signed-in user. |
| `POST` | `/cloud/v1/auth/signout` | session + CSRF | Revoke the session and clear the cookie. |
| `DELETE` | `/cloud/v1/account` | session + CSRF | Delete the account and everything it owns. |
| `GET`  | `/cloud/projects` | session | Self-contained HTML page for projects and tokens. |
| `GET`  | `/cloud/v1/projects` | session | List the tenant's projects. |
| `POST` | `/cloud/v1/projects` | session + CSRF | Create a project from `{"id":"slug","name":"..."}`. |
| `DELETE` | `/cloud/v1/projects/{project}` | session + CSRF | Delete a project (and its destinations). |
| `GET`  | `/cloud/v1/tokens` | session | List token metadata (never the secret). |
| `POST` | `/cloud/v1/tokens` | session + CSRF | Create a multi-project token from `{"projects":["a","b"]}`. |
| `DELETE` | `/cloud/v1/tokens/{id}` | session + CSRF | Revoke token `id` (its one-way hash). |
| `POST` | `/cloud/v1/projects/{project}/token` | session + CSRF | Create a project token; the raw secret is returned once. |
| `POST` | `/cloud/v1/projects/{project}/token/rotate` | session + CSRF | Issue a new project token and revoke the old one(s). |

A successful callback returns:

```json
{
  "csrfToken": "…",
  "user": {"id": "usr_…", "email": "you@example.com", "accountId": "acct_…", "tenantId": "ten_…"}
}
```

and sets `herald_session`.

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
  redeemed.

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

`POST /cloud/v1/auth/magic-link` consumes one unit per client IP and one per
target email in a fixed window (10/minute by default), answering `429` with
`Retry-After` when exhausted. The per-IP key is the transport peer — forwarded
headers are not trusted, so a caller cannot spoof its way around the limit.

## Account deletion

`DELETE /cloud/v1/account` (session + CSRF) deletes the account. Foreign keys
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
(`GET /cloud/v1/tokens`) reports an id, kind, projects, created, last-used and
revoked time but can never reveal the secret. **Rotation** issues a replacement
and revokes the previous project token(s) in one operation; **revocation** by id
is immediate, because the sender's credential lookup sees the revoked row on its
next request.

The HTML page at `/cloud/projects` is a thin stdlib template over these JSON
routes. It authenticates with the session cookie and sends the CSRF token
(`X-CSRF-Token`) on every mutation; if the token is not already in the browser
(after `#34` lands, sign-in will hand it over) the page offers a paste field.
Every successful mutation is audited through the structured logger with the
actor, tenant, project and action — never a raw secret.

## Self-host compatibility

A deployment that never sets `[cloud]` is unchanged: no accounts surface, no
mail changes, no extra routes. The migration that adds the auth tables is purely
additive, so an existing Event database upgrades with no data loss.

## Deferred

Not part of this milestone and deliberately not built here: destination
configuration and verification (#31), bounce/suppression handling (#32), the
dashboard (#33), `herald login` (#34) and quotas (#35). The project/token
self-service surface above is #30. Token `last_used_at` is part of the metadata
contract but is not yet written on authentication, so it currently always reads
empty.
