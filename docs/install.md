# Installing Herald

`herald` is a single binary. Two install paths are supported: build it with the
Go toolchain, or download a prebuilt binary from a tagged GitHub release.

Supported targets for prebuilt binaries:

| OS | Architectures |
|---|---|
| Linux | `amd64`, `arm64` |
| macOS | `amd64` (Intel), `arm64` (Apple silicon) |

There are no Windows builds. Version tags are `vMAJOR.MINOR.PATCH` (Herald is
pre-1.0; see [releasing.md](releasing.md)).

## One-line install

```sh
curl -fsSL https://raw.githubusercontent.com/marcustut/herald/main/install.sh | sh
```

Installs the latest release for your OS/architecture to `~/.local/bin`. Override
the directory with `HERALD_INSTALL_DIR`, or pin a version with `HERALD_VERSION`.
The script verifies the SHA256 checksum before installing.

## `go install`

Requires the Go toolchain (see [`mise.toml`](../mise.toml), currently Go 1.27):

```sh
go install github.com/marcustut/herald/cmd/herald@latest
```

This installs to `$(go env GOBIN)`, or `$(go env GOPATH)/bin` when `GOBIN` is
unset. Make sure that directory is on your `PATH`. To build a specific tag,
replace `@latest` with `@vX.Y.Z`.

## Prebuilt binary

Download the binary for your OS and architecture and the checksum file from the
release page, then verify before running it. Replace `vX.Y.Z` with the tag you
want:

```sh
VERSION=vX.Y.Z
OS=linux      # linux or darwin
ARCH=amd64    # amd64 or arm64
BASE="https://github.com/marcustut/herald/releases/download/${VERSION}"

curl -fsSLO "${BASE}/herald_${OS}_${ARCH}"
curl -fsSLO "${BASE}/checksums.txt"
```

Verify the download against `checksums.txt`:

```sh
# Linux (GNU coreutils)
sha256sum --check --ignore-missing checksums.txt

# macOS (BSD tools)
grep "herald_${OS}_${ARCH}" checksums.txt | shasum -a 256 --check -
```

A good download prints `herald_${OS}_${ARCH}: OK`. If it prints `FAILED`, do not
run the binary — download it again.

Then install it on your `PATH`:

```sh
chmod +x "herald_${OS}_${ARCH}"
mv "herald_${OS}_${ARCH}" "$HOME/.local/bin/herald"
```

## Confirm the install

```sh
herald version
```

A released binary prints its tag (for example `v0.2.0`). A binary built from
source without release flags prints `dev`.
