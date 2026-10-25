#!/usr/bin/env bash
set -euo pipefail
export GOTOOLCHAIN=local
export GOMODCACHE=/bundle/gomod
export GOCACHE=/tmp/go-cache
export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL=/tmp/capture-gitconfig
export CGO_ENABLED=1
export CGO_CFLAGS='-O2 -g -I/src/.sqlite-include'
cd /src
mkdir -p /tmp/build-home

case "${1:?prepare or build required}" in
  prepare)
    git config --file "$GIT_CONFIG_GLOBAL" url.https://github.com/.insteadOf ssh://git@github.com/
    go mod download
    make pricing-snapshot
    cp internal/pricing/snapshot/litellm_snapshot.json.gz /bundle/pricing.json.gz
    (cd frontend && npm ci)
    mv frontend/node_modules /bundle/frontend_node_modules
    uv sync --project docs --frozen --no-dev --python /usr/local/bin/python
    mv docs/.venv /bundle/docs_venv
    ;;
  build)
    export GOPROXY=off GOSUMDB=off GOFLAGS='-mod=readonly -buildvcs=false'
    mkdir -p .sqlite-include internal/pricing/snapshot
    cp "$(go list -m -f '{{.Dir}}' github.com/mattn/go-sqlite3)/sqlite3-binding.h" .sqlite-include/sqlite3.h
    cp /bundle/pricing.json.gz internal/pricing/snapshot/litellm_snapshot.json.gz
    cp -a /bundle/frontend_node_modules frontend/node_modules
    (cd frontend && VITE_PROJECT_MAPPING_WORKSPACE=true npm --offline run build)
    rm -rf internal/web/dist
    cp -a frontend/dist internal/web/dist
    printf '%s\n' 'keep embed dir for generated frontend assets' > internal/web/dist/.keep
    go build -trimpath -tags 'fts5,kit_posthog_disabled' \
      -ldflags "-s -w -X main.version=$AV_VERSION -X main.commit=$AV_COMMIT -X main.buildDate=$AV_BUILD_DATE" \
      -o /out/agentsview ./cmd/agentsview
    cp -a internal/web/dist /out/frontend
    ;;
  *) exit 2 ;;
esac
