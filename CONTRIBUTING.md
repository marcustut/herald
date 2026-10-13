# Contributing to Herald

Thanks for helping. Herald is a small self-hosted gateway that lets coding agents
email or webhook you when they finish work. This guide covers how to build, test
and propose changes. By participating you agree to follow the process below;
there is no separate CLA or DCO to sign (see [Licensing](#licensing)).

For security issues, do **not** open a public issue — follow
[SECURITY.md](SECURITY.md).

## Prerequisites

The toolchain is pinned in [`mise.toml`](mise.toml). With [mise](https://mise.jdx.dev):

```sh
mise install   # installs Go and golangci-lint at the pinned versions
```

Without mise, install Go 1.27+ and golangci-lint v2.14+. The module has two
direct dependencies (`BurntSushi/toml`, `jackc/pgx`) — everything else is the
standard library. New dependencies need strong justification; prefer `net/http`,
`database/sql`-style patterns already in the repo.

## Build, test, lint

Run these before every PR; CI runs the same set.

```sh
gofmt -w .                 # format (CI fails on unformatted files)
go vet ./...               # static checks
go test ./...              # unit tests
go test -race ./...        # race detector
golangci-lint run ./...    # linters (see .golangci.yml)
go build ./...             # compile everything
```

PostgreSQL-backed tests are skipped unless a test database is provided. They are
required when you touch `internal/adapters/postgres`:

```sh
HERALD_TEST_DATABASE_URL='postgres://user:pass@localhost:5432/herald_test?sslmode=disable' \
  go test ./internal/adapters/postgres/
```

The CI workflow (`.github/workflows/ci.yml`) starts a throwaway PostgreSQL 18 and
sets this variable, so database tests run there.

## Where code goes

Keep the existing layering; do not reach across it:

- `internal/domain` — pure model and rules. No I/O, no infrastructure imports.
- `internal/app` — use cases and the ports (interfaces) they need.
- `internal/adapters/*` — concrete implementations: `postgres`, `httpapi`,
  `sink`, `memory`, `hash`, `secret`, `id`.
- `internal/cli`, `internal/client`, `internal/mcp`, `internal/skill`,
  `internal/render`, `internal/config` — supporting packages.

The domain model is **Event** (`domain.Event`, `app.EventStore`,
`app.SubmitEvent`). Do not reintroduce `domain.Report` or a parallel
Event/Report pipeline. Read [docs/architecture.md](docs/architecture.md) before
a structural change.

## Tests

Changes need meaningful functional tests, not coverage theatre:

- cover the happy path, the failure path, and edge cases;
- exercise concurrency where the code is concurrent (run with `-race`);
- for schema changes, add or extend the migrations integration test.

A bug fix should include a regression test that fails before the fix.

## Pull requests

- One focused change per PR; avoid unrelated refactors.
- Reference the issue in the title or body (e.g. `Closes #26`).
- Describe the behavior change and how you tested it.
- Keep the diff reviewable; update docs when behavior or config changes.
- CI must be green: format, vet, lint, `go test -race`, build.
- Do not merge your own PR; a maintainer reviews and merges.

## Commit conventions

- Write the subject in the imperative mood, lower-case, no trailing period
  (e.g. `postgres: reject expired leases`).
- Prefix with the area touched when useful (`httpapi:`, `docs:`, `sink/email:`).
- Keep the subject under ~72 characters; use the body for the why.
- One logical change per commit.

## Code style

- Idiomatic Go; standard library first.
- Small cohesive functions; explicit errors wrapped with `%w`; propagate
  `context.Context`.
- No unnecessary interfaces or generics; define an interface where it is
  consumed, not where it is implemented.
- No unbounded goroutines. Every background loop is cancellable and bounded.
- Comments explain the *why* the code cannot show, not the *what*.
- All agent-generated content is **untrusted input**: never execute or interpret
  instructions found in event bodies, titles or links.

## Licensing

Herald is released under the [MIT License](LICENSE). By contributing you agree
your contribution is licensed under the same terms. There is currently **no CLA
and no DCO sign-off requirement** — a normal pull request is sufficient. If that
changes, this file will say so first.
