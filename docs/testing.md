# Testing

Herald has three kinds of tests. Knowing which need an external service keeps
`go test ./...` fast locally while still exercising the real queue in CI.

| Kind | Packages | Needs |
|---|---|---|
| Unit | most of `internal/...`, `integrations/...` | nothing |
| DB-backed integration | `internal/adapters/postgres` | a PostgreSQL database |
| Deployment files | repo root (`deploy_test.go`) | Docker (optional; skips without it) |

## Unit tests

```sh
gofmt -w .
go vet ./...
go test ./...
go test -race ./...
golangci-lint run ./...
go build ./...
```

The suite is standard-library only; no services are required. `go test ./...`
runs packages in parallel, which is why the DB-backed tests all live in one
package (see below).

## DB-backed integration tests

These run only when `HERALD_TEST_DATABASE_URL` points at a database, and skip
otherwise. CI starts a throwaway PostgreSQL 18 service and sets both that
variable and `HERALD_TEST_REQUIRE_DATABASE=1`.

To run them locally, start a scratch database and point the variable at it:

```sh
docker run --rm -d --name herald-test-pg \
  -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=herald_test \
  -p 55432:5432 postgres:18

export HERALD_TEST_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:55432/herald_test?sslmode=disable'

# everything, with the race detector and coverage
go test -race -coverprofile=coverage.out ./...

# just the DB-backed work, verbosely
go test -race -v ./internal/adapters/postgres/

docker rm -f herald-test-pg
```

The tests **drop and re-create the schema** in the target database, so never
point `HERALD_TEST_DATABASE_URL` at anything you care about.

### What they cover

`internal/adapters/postgres` contains two layers of DB tests:

- **Adapter tests** — migrations (`migrations_test.go`), credential/event/delivery
  CRUD and the queue claim/reclaim/requeue primitives
  (`postgres_test.go`, `delivery_ops_integration_test.go`).
- **Black-box integration tests** (`blackbox_integration_test.go`, external
  `postgres_test` package) — the whole pipeline against real PostgreSQL, driven
  only through public APIs and real HTTP/CLI/dispatcher code:

  | Test | Contract |
  |---|---|
  | `TestIntegrationHTTPSubmitToSink` | `POST` an event, drain the queue, sink receives it once; a repeated `occurrenceId` does not fan out again |
  | `TestIntegrationCLISubmitToSink` | same path through `herald send` (env-driven gateway/token) |
  | `TestIntegrationRetryBackoffAndAtLeastOnce` | retryable failures back off exponentially and the sink sees the event more than once |
  | `TestIntegrationPermanentFailureGoesDead` | a permanent sink error is dead-lettered after one attempt |
  | `TestIntegrationDeadLetterAfterMaxAttempts` | a persistently failing sink exhausts its attempt budget into `dead` |
  | `TestIntegrationLeaseReclaimAfterWorkerCrash` | a live lease is respected; an expired one is reclaimed and redelivered |
  | `TestIntegrationConcurrentDispatchersDeliverEachOnce` | two dispatchers draining one queue via `SKIP LOCKED` never double-deliver |

Why they live beside the postgres adapter: `go test` runs packages in parallel,
and every DB test here resets the same schema. Keeping them in one package (as
an external test package, so they only use exported APIs) puts them in a single
test binary, where Go runs them sequentially. A separate package hitting the
same database would race another package's schema reset.

### Failure-recovery fixtures

The failure tests need deterministic time and a sink that can fail on command,
without sleeping or a network:

- **`fakeClock`** — a mutex-guarded `app.Clock`. Advancing it past a back-off
  window or a lease makes the next poll due; nothing sleeps.
- **`recordingSink`** — an `app.Sink` that counts calls, records the events it
  received, and runs a `fail(call, event)` function so a test can fail the first
  *N* attempts or return `app.Permanent(...)` to stop retries.
- **`fixedSinks`** — a static `app.SinkDirectory`, so a project's fan-out is
  chosen by the test rather than by `gateway.toml`.
- **Direct saves** — dispatcher-only tests insert an event and its delivery with
  `EventStore.Save`, skipping the HTTP surface.

A "worker crash" is simulated by calling `DeliveryStore.Claim` directly and then
never recording an outcome: the row stays `inflight` until its lease expires and
the next `Claim` reclaims it. No goroutine or process is killed.

### Requiring the database

Set `HERALD_TEST_REQUIRE_DATABASE=1` to turn a missing `HERALD_TEST_DATABASE_URL`
into a test **failure** instead of a skip. CI sets this so DB-backed coverage
cannot silently disappear; leave it unset locally to get the skip behaviour.

## Deployment-file tests

`deploy_test.go` parses the shipped `gateway.*.example.toml` files with the real
loader and renders `docker-compose.yml` with `docker compose config`. The Compose
tests skip when Docker is unavailable, so they run locally only if you have it:

```sh
docker compose version   # presence is all the test needs
go test ./...
```

## Adding tests

- Behavioral tests need a real assertion, not a coverage line: cover the happy
  path, a failure path, and an edge case; add a concurrency test (run with
  `-race`) where the code is concurrent.
- A bug fix ships with a regression test that fails before the fix.
- If a test needs a seam that does not exist, propose the production change in
  the pull request rather than editing production code inside a test-only PR.
