#!/bin/bash
# Fails if the Go version drifts between the three places it is declared:
#
#   go.mod                       toolchain go1.26.5        (full version)
#   Dockerfile                   FROM golang:1.26.5-alpine (full version)
#   .devcontainer/devcontainer.json  devcontainers/go:2-1.26-bookworm (minor only)
#
# Renovate bumps these as three separate dependencies. The grouping rule in
# renovate.json puts them in one PR, but nothing stops them landing apart or
# being edited by hand, so this is the actual guarantee.
set -euo pipefail

cd "$(dirname "$0")/.."

fail() { echo "::error::$*" >&2; FAILED=1; }
FAILED=0

# sed -nE rather than GNU grep -P, so the script also runs on macOS/BSD.
GOMOD=$(sed -nE 's/^toolchain go([0-9]+\.[0-9]+\.[0-9]+).*$/\1/p' go.mod | head -1)
DOCKER=$(sed -nE 's/^FROM golang:([0-9]+\.[0-9]+\.[0-9]+).*$/\1/p' Dockerfile | head -1)
DEVC=$(sed -nE 's|.*devcontainers/go:[0-9]+-([0-9]+\.[0-9]+).*|\1|p' .devcontainer/devcontainer.json | head -1)

[ -n "$GOMOD" ]  || fail "could not read 'toolchain goX.Y.Z' from go.mod"
[ -n "$DOCKER" ] || fail "could not read 'FROM golang:X.Y.Z' from Dockerfile"
[ -n "$DEVC" ]   || fail "could not read 'devcontainers/go:N-X.Y' from .devcontainer/devcontainer.json"
[ "$FAILED" -eq 0 ] || exit 1

echo "go.mod toolchain:  $GOMOD"
echo "Dockerfile:        $DOCKER"
echo "devcontainer:      $DEVC (minor only)"

if [ "$GOMOD" != "$DOCKER" ]; then
    fail "go.mod toolchain ($GOMOD) != Dockerfile golang tag ($DOCKER). CI and the release image would build on different Go versions."
fi

if [ "${GOMOD%.*}" != "$DEVC" ]; then
    fail "devcontainer Go minor ($DEVC) != go.mod toolchain minor (${GOMOD%.*}). The dev container would download a different toolchain on every build."
fi

[ "$FAILED" -eq 0 ] || exit 1
echo "Go versions are in sync."
