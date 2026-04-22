#!/bin/sh
#
# Build helper invoked by air on every Go file change. Keeps the
# build command in one place so air config stays minimal.

set -eu

mkdir -p tmp

# Ensure the embed dir exists even if frontend hasn't been built,
# so go:embed doesn't complain during backend-only dev.
mkdir -p internal/web/dist
[ -n "$(ls internal/web/dist/ 2>/dev/null)" ] \
  || printf '%s\n' \
    '<!doctype html>' \
    '<html lang="en">' \
    '  <head>' \
    '    <meta charset="UTF-8" />' \
    '    <meta name="viewport" content="width=device-width, initial-scale=1.0" />' \
    '    <link rel="icon" type="image/svg+xml" href="/favicon.svg" />' \
    '    <title>AgentsView</title>' \
    '    <script type="module" src="/assets/index.js"></script>' \
    '    <link rel="stylesheet" href="/assets/index.css" />' \
    '  </head>' \
    '  <body>' \
    '    <div id="app"></div>' \
    '  </body>' \
    '</html>' \
    > internal/web/dist/index.html

VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

LDFLAGS="-X main.version=${VERSION} \
         -X main.commit=${COMMIT} \
         -X main.buildDate=${BUILD_DATE}"

CGO_ENABLED=1 go build -tags fts5 -ldflags="${LDFLAGS}" \
  -o ./tmp/agentsview ./cmd/agentsview
