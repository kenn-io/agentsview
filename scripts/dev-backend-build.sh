#!/bin/sh
#
# Build helper invoked by air on every Go file change. Keeps the
# build command in one place so air config stays minimal.

set -eu

mkdir -p tmp

mkdir -p internal/web/dist
[ -f internal/web/dist/.keep ] \
  || printf '%s\n' \
    'keep embed dir for generated frontend assets' \
    > internal/web/dist/.keep

VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

LDFLAGS="-X main.version=${VERSION} \
         -X main.commit=${COMMIT} \
         -X main.buildDate=${BUILD_DATE}"

CGO_ENABLED=1 go build -tags fts5 -ldflags="${LDFLAGS}" \
  -o ./tmp/agentsview ./cmd/agentsview
