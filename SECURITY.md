# Security Policy

Herald accepts events from coding agents, redacts secrets, and delivers them to
your inbox or a webhook. It is self-hosted, so most of the trust boundary is
yours to configure — but the code that runs inside that boundary should be
sound. This document explains how to report a problem and what is in scope.

## Supported versions

Herald is pre-1.0 and ships from `main`. Security fixes are made on `main` and
released as a tagged version; older tags are not maintained.

| Version | Supported |
|---|---|
| `main` / latest tag | Yes |
| Older tags | No |

If you self-host, run a recent build and follow [docs/releasing.md](docs/releasing.md)
for the release checklist.

## Reporting a vulnerability

**Do not open a public issue, discussion, or pull request for a security
problem.** Public disclosure before a fix puts every deployment at risk.

Use GitHub's private vulnerability reporting on the repository:
**Security → Report a vulnerability** (this opens a private advisory visible only
to the maintainers). If you cannot use that, contact the maintainer directly via
the repository owner's GitHub profile.

Please include:

- the affected version or commit,
- a description of the issue and its impact,
- reproduction steps or a proof of concept,
- any suggested fix or mitigation, if you have one.

### What to expect

- **Acknowledgement** within 3 business days.
- An initial assessment (severity and scope) within 10 business days.
- A fix and coordinated disclosure; we will credit reporters who want credit, and
  keep them updated on progress.

Please give us a reasonable window to ship a fix before disclosing publicly.

## Scope

In scope — issues in this repository:

- authentication and authorization of the HTTP API (`POST /v1/events`, the
  deprecated `/v1/reports` alias, and read endpoints),
- credential handling: token issuance, storage at rest, revocation, and scope,
- redaction of secrets from event content, and injection into sinks,
- the delivery queue: idempotency, lease/claim handling, and duplicate delivery,
- request parsing, size/rate limits, and denial-of-service resistance,
- SQL construction and database migrations,
- the CLI, MCP server, and agent skill surfaces.

Out of scope:

- Misconfiguration of your own deployment (exposed HTTP port, missing TLS
  termination, weak or leaked tokens, over-broad sink destinations).
- Vulnerabilities in third-party services you point Herald at (SMTP provider,
  PostgreSQL, webhook receiver) or in your reverse proxy.
- Missing security hardening in example configs that are explicitly documented as
  examples — but tell us anyway, we would rather ship secure defaults.

## Design notes for reviewers

- **Event content is untrusted.** Titles, bodies, links, context values and
  agent-supplied fields are attacker-controlled free text. Herald treats them as
  data: it never interprets or executes instructions found in them, and it
  redacts secret-looking strings before storage and delivery. Reporters should
  assume content may contain markup or injection payloads aimed at whoever reads
  the notification.
- **The recipient is pinned server-side.** Sinks (email address, webhook URL) are
  resolved from the token and configuration, never from the request body, so an
  agent cannot redirect where a notification goes.
- **Tokens are hashed at rest.** Only the SHA-256 of a token is stored; the raw
  value is shown once at issue time. There is no recovery path for a lost token —
  revoke and reissue.
- **Secrets never belong in `gateway.toml`.** SMTP credentials and database URLs
  come from the environment; config files hold non-secret settings only.
