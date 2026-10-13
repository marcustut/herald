#!/bin/sh
# Install the herald CLI from a GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/marcustut/herald/main/install.sh | sh
#
# Environment:
#   HERALD_VERSION       release tag to install (default: latest)
#   HERALD_INSTALL_DIR   destination directory (default: ~/.local/bin)
set -eu

REPO="marcustut/herald"
BIN="herald"
INSTALL_DIR="${HERALD_INSTALL_DIR:-$HOME/.local/bin}"
VERSION="${HERALD_VERSION:-latest}"

err() { printf 'herald-install: %s\n' "$*" >&2; exit 1; }

case "$(uname -s)" in
	Linux) OS=linux ;;
	Darwin) OS=darwin ;;
	*) err "unsupported OS: $(uname -s) (supported: Linux, macOS)" ;;
esac
case "$(uname -m)" in
	x86_64 | amd64) ARCH=amd64 ;;
	arm64 | aarch64) ARCH=arm64 ;;
	*) err "unsupported architecture: $(uname -m) (supported: amd64, arm64)" ;;
esac

command -v curl >/dev/null 2>&1 || err "curl is required"

if [ "$VERSION" = "latest" ]; then
	VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" |
		sed -n 's/.*"tag_name" *: *"\([^"]*\)".*/\1/p' | head -n1)
fi
[ -n "$VERSION" ] || err "could not determine the latest release (has one been published?)"

ASSET="${BIN}_${OS}_${ARCH}"
BASE="https://github.com/${REPO}/releases/download/${VERSION}"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

printf 'herald-install: downloading %s %s\n' "$ASSET" "$VERSION"
curl -fsSL "${BASE}/${ASSET}" -o "${TMP}/${ASSET}" ||
	err "download failed: ${BASE}/${ASSET} (does ${VERSION} exist with ${ASSET}?)"
curl -fsSL "${BASE}/checksums.txt" -o "${TMP}/checksums.txt" ||
	err "could not download checksums.txt"

if command -v sha256sum >/dev/null 2>&1; then
	(cd "$TMP" && grep " ${ASSET}\$" checksums.txt | sha256sum -c --ignore-missing - >/dev/null) ||
		err "checksum verification failed"
elif command -v shasum >/dev/null 2>&1; then
	(cd "$TMP" && grep " ${ASSET}\$" checksums.txt | shasum -a 256 -c - >/dev/null) ||
		err "checksum verification failed"
else
	err "need sha256sum or shasum to verify the download"
fi

mkdir -p "$INSTALL_DIR" || err "could not create ${INSTALL_DIR}"
if command -v install >/dev/null 2>&1; then
	install -m 0755 "${TMP}/${ASSET}" "${INSTALL_DIR}/${BIN}"
else
	cp "${TMP}/${ASSET}" "${INSTALL_DIR}/${BIN}"
	chmod 0755 "${INSTALL_DIR}/${BIN}"
fi

printf 'herald-install: installed %s to %s\n' "$VERSION" "${INSTALL_DIR}/${BIN}"
case ":$PATH:" in
	*":${INSTALL_DIR}:"*) ;;
	*) printf 'herald-install: add %s to your PATH to use it.\n' "$INSTALL_DIR" ;;
esac
