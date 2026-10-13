# M0 / M1 Delivery Report

Coordinator delivery report for **M0 — Operational Foundation**
([milestone 1](https://github.com/marcustut/herald/milestone/1)) and **M1 —
Open-Source Developer Release**
([milestone 2](https://github.com/marcustut/herald/milestone/2)).

Baseline: `16178d7` ("Adopt the event model"). All work was delivered through
reviewed PRs by herdr-supervised OpenCode implementation agents (Flash
Nyrobrain), then independently reviewed, tested, and merged by the coordinator.

## Executive Summary

- **M0: complete** — 9/9 issues closed, PRs merged, tests + DB integration green.
- **M1: implementation complete** (10/10 issues closed) — **release-blocked
  pending one owner action:** authorizing the repository to change from private
  to public.
- 19 implementation issues delivered across 20 PRs; `main` at `3e8ee75` and CI
  green on every merge.
- No secrets committed; no production infrastructure changes were made beyond
  the already-existing deployment.

## Implementation Summary

| Issue | Title | PR | Merge | Independent verification |
|---|---|---|---|---|
| #64 | From display name | #66 | `c8be24c` | diff review; `-race`/lint; RFC-5322 quoting/encoding; CR/LF injection stripped |
| #13 | Occurrence IDs + project-scoped idempotency | #67 | `e430839` | `deriveKey` removed; DB tests (identical content ⇒ distinct events; retry ⇒ same id); migration |
| #19 | MIT license | #65, #69 | `30eadd4`, `3879f1a` | LICENSE/README/flake consistent; no Unlicense refs |
| #21 | Docker + Compose | #68 | `e3b295e` | **built image + `docker compose up`** → healthz 200, token create, event → 202 |
| #18 | Versioned migrations | #70 | `83ce7a2` | DB tests: fresh, legacy, event-schema, concurrent, checksum-mismatch, idempotent |
| #2 | Rate limiting + quotas | #71 | `43dc468` | 429 + Retry-After + X-RateLimit-*; concurrency, partition, reset tests |
| #24 | Lifecycle hook (Claude Code) | #72 | `2670a52` | opt-in default; debounce; decision-logic unit tests |
| #1 | herdr plugin | #73 | `3b37f36` | **live `pane.agent_status_changed` verified** vs herdr 0.9.3; blocked/done only; occurrence ids |
| #26 | Governance/security/release docs | #74 | `ee176d2` | new files; neutral example domains |
| #12 | NixOS deploy runbook + verification | #75 | `d0eebb5` | host-neutral procedure; rollback; verification checklist |
| #14 | Delivery diagnostics + dead-letter | #76 | `89c17a5` | `herald delivery list/show/retry`; idempotent requeue; DB tests |
| #15 | Observability + health/readiness | #77 | `36c1fa4` | metrics without secret/body labels; liveness vs readiness split |
| #22 | `herald init` + quickstart | #78 | `2fcd730` | init write/no-overwrite/force/detection tests |
| #23 | v1 API/CLI/MCP/skill docs | #79 | `5017229` | `openapi.yaml` matches schema; canonical `/v1/events` + deprecated `/v1/reports` |
| #16 | SMTP/webhook/content hardening | #80 | `6e5667c` | SSRF allowlist + dial-time private-IP block; adversarial tests |
| #17 | Integration/failure-recovery tests | #81 | `c5122d0` | black-box submit→queue→dispatcher→sink; lease reclaim; DB in CI |
| #20 | Release binaries | #82 | `63aff74` | release workflow; version via ldflags; checksums |
| #27 | Landing page + launch material | #83 | `6e574d9` | static site; no trackers; neutral domains |
| #25 | Consent / anti-noise policy | #84 | `3e8ee75` | shared opt-in policy across skill/MCP/hooks; tests |

## Architecture Review

- Layering preserved: `domain` (pure) → `app` (ports/use cases) →
  `adapters/*` → `cli`. No new dependencies beyond `BurntSushi/toml` and
  `pgx` (already present); everything else standard library.
- The Event model is intact and not duplicated (no `domain.Report`).
- New abstractions are ports with real consumers: `app.Limits`/`RateLimiter`
  (#2), an observer/metrics port (#15), delivery inspection methods on the
  existing `DeliveryStore` (#14). No speculative interfaces or generics.
- Migrations became versioned (#18) on top of — not instead of — the guarded
  rename, so the actual schema history is preserved.
- Concurrency: queue leasing, advisory-lock migration, and rate-limiter state
  are concurrency-safe and race-tested.

## Verification Evidence

Executed on `main` (`3e8ee75`), coordinator-run (not agent-reported):

- `gofmt -l .` — clean
- `go vet ./...` — clean
- `golangci-lint run ./...` — 0 issues
- `go build ./...` — OK
- `go test -race ./...` with `HERALD_TEST_DATABASE_URL` (PostgreSQL 18) — pass,
  including the migration and delivery integration suites
- Docker: image built; `docker compose up -d` verified end to end (health,
  token, event 202)
- CI: `test` + GitGuardian green on every merged PR and on `main`

## Security Review

- All event content is treated as untrusted; redaction remains at ingest.
- Authentication is token-based with tenant/project scoping; agent/host are
  client-asserted, never trusted from the body.
- Webhook sinks are admin-configured only; SSRF defences (redirects disabled,
  private/link-local/metadata IP blocking at dial time) added in #16.
- Email header injection guarded (#64); SMTP fails safe for remote transports.
- No secrets, tokens, or bodies in logs/metrics labels (#15).
- No credentials were committed; the repo history was not rewritten.

## M0 Readiness

- Runtime deployment: Herald is live at `https://herald.marcustut.me/api`
  (nginx/ACME, Stalwart SMTP, local PostgreSQL, separate dispatcher service).
- **Caveat:** the production box runs the pre-Event build (deployed earlier in
  this project). The merged changes are backward compatible but **not yet
  redeployed** — a redeploy + runtime re-verification is recommended (see
  Remaining Decisions).
- Event ingestion, delivery reliability, queue behaviour, recovery, and the
  herdr integration are covered by the integration tests and the live event
  probe; observability and dead-letter tooling are in place.

## M1 Readiness

- MIT license: implemented. Docker Compose self-host: verified. CLI/MCP/skill
  contracts: documented (#23), consent policy aligned (#25). Release binaries
  and pipeline: added (#20). Landing page + launch material: added (#27).
- **Open-source publication status: PRIVATE.** Changing repository visibility is
  the only remaining M1 release gate and requires explicit owner authorization.

## Remaining Decisions (owner)

1. **Authorize public repository visibility** (M1 release gate). All other M1
   requirements are complete; the repository remains private until approved.
2. **Authorize redeploying production** with the merged changes, and the
   preferred change window. The migration is additive; a redeploy applies the
   Event-model binary, versioned migrations, rate limiting, observability, and
   security hardening.

## Future Recommendations (do not implement now)

- M2: tenant/account model; managed delivery; deliverability (bounces).
- Consider a lightweight `LISTEN/NOTIFY` trigger to cut dispatcher latency.
- Revisit digest mode (issue tracker) once real volume is observed.

## Note on process

Some PRs used GitHub closing keywords, so a few issues auto-closed on merge.
The coordinator subsequently verified and (re)closed every issue with evidence.
Going forward, agents are instructed not to use closing keywords; only the
coordinator closes issues after acceptance.
