# Releasing Herald

This is the checklist and process for cutting a Herald release. Herald is
pre-1.0: treat every tag as potentially breaking and say so in the notes.

A release is a signed, annotated Git tag on `main` whose commit is green in CI,
plus published build artifacts that have been verified against that commit.

## Version scheme

Herald uses [Semantic Versioning](https://semver.org/). Before 1.0, minor
versions may break compatibility; record any breaking change in the release
notes. The running version is reported by `herald version`; bump it (the
`version` constant in `internal/cli/cli.go`) in the release commit so the binary
and the tag agree.

## Pre-release checklist

Run everything on the exact commit you intend to tag (`git status` clean, on
`main`, up to date with `origin/main`).

### 1. Tests, vet and lint

```sh
gofmt -l .            # must print nothing
go vet ./...
go test -race ./...
golangci-lint run ./...
go build ./...
```

All of these also run in CI (`.github/workflows/ci.yml`). Confirm the CI run for
the tag commit is green before publishing.

### 2. PostgreSQL integration tests

The default `go test ./...` skips database tests when no DSN is set. Run them
against a real PostgreSQL 18, including the migration and backup/restore checks
in [docs/migrations.md](docs/migrations.md):

```sh
HERALD_TEST_DATABASE_URL='postgres://herald:change-me@localhost:5432/herald_test?sslmode=disable' \
  go test ./internal/adapters/postgres/
```

### 3. Migration review

If the release touches `internal/adapters/postgres/migrations.go`:

- every change is a **new appended step**; no applied step's SQL changed
  (the checksum guard fails startup otherwise);
- the step is idempotent/forward-only and comes with store-code changes and
  integration tests in the same commit;
- the backup/restore smoke test in [docs/migrations.md](docs/migrations.md)
  passes;
- the release notes call out that migrations run automatically on startup and
  are not downgradable.

### 4. Dependency and license review

```sh
go mod tidy && git diff --exit-code go.mod go.sum   # no drift
go mod verify
go list -m -u all                                   # note available updates
```

Review any new direct dependency for license and maintenance (Herald itself is
MIT; the two direct deps are `BurntSushi/toml` and `jackc/pgx`). Run a
vulnerability scan over the module graph:

```sh
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

### 5. Secret scan

Scan the full history, not just the working tree, so a secret cannot ship in a
commit that was later "removed":

```sh
# any one of these is fine; use whichever is available locally
gitleaks detect --source . --redact
# or
docker run --rm -v "$PWD:/repo" zricethezav/gitleaks:latest detect --source /repo --redact
```

Also confirm nothing sensitive is tracked:

```sh
git ls-files | grep -E '(^|/)(\.env|\.env\..*|gateway\.toml)$'   # must be empty
```

`.env`, `gateway.toml` and `.env.*` are gitignored and required to be untracked.
Example files (`*.example.*`) must contain neutral domains and placeholder
values only — never a real token, mailbox, or webhook URL.

### 6. Build and verify artifacts

Build the binary and the container image from the tagged commit, then smoke-test
each artifact before publishing.

```sh
# binary
go build -trimpath -o herald ./cmd/herald
./herald version                       # matches the tag

# container image (built from the Dockerfile)
docker build -t herald:<version> .
docker run --rm herald:<version> version

# NixOS module / flake (replace the fakeHash in flake.nix first)
nix build .#herald
./result/bin/herald version
```

Publish checksums for any binary you attach to the release and verify them after
upload:

```sh
sha256sum herald > herald_<version>_linux_amd64.sha256
sha256sum -c herald_<version>_linux_amd64.sha256
```

Run the end-to-end check from [docs/self-hosting-docker.md](docs/self-hosting-docker.md)
(`docker compose up`, create a token, submit an event, confirm delivery) against
the image you are about to publish — not a locally cached older one.

## Release process

1. Complete the checklist above on the release commit.
2. Commit the version bump (and any release-note prep) with a focused message
   such as `release: v0.2.0`.
3. Create an **annotated, signed** tag:
   ```sh
   git tag -s v0.2.0 -m "herald v0.2.0"
   git push origin v0.2.0
   ```
4. Push the tag; confirm CI runs green on it.
5. Draft a GitHub release for the tag. Summarize highlights, breaking changes,
   required migrations, and the minimum Go version. Attach the binary and its
   checksum file.
6. Announce only after the artifacts are uploaded and verified.

## Post-release

- Verify a fresh install from the published artifacts actually works
  (`docker compose up` path or `nix build`, then submit one event).
- Confirm `/v1/reports` still works as the deprecated alias of `/v1/events`; it
  must keep working until a documented removal.
- Watch for regressions and be ready to cut a patch release. Herald has no
  automatic downgrade: a bad migration is fixed forward, or by restoring a
  backup and rolling the binary back together
  (see [docs/migrations.md](docs/migrations.md)).
