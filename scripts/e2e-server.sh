#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

DB_PATH="$TMPDIR/sessions.db"
DUCKDB_PATH="$TMPDIR/sessions.duckdb"
EMPTY_DIR="$TMPDIR/empty"
BACKEND="${AGENTSVIEW_E2E_BACKEND:-sqlite}"
mkdir -p "$EMPTY_DIR"

# Use pre-built binaries if available (CI sets these),
# otherwise build from source (local dev).
FIXTURE="${E2E_PREBUILT_FIXTURE:-}"
SERVER="${E2E_PREBUILT_SERVER:-}"
PRICING_SNAPSHOT_RESTORED=0

restore_pricing_snapshot() {
    if [ "$PRICING_SNAPSHOT_RESTORED" -eq 1 ]; then
        return
    fi

    (cd "$ROOT" && go run ./internal/pricing/cmd/litellm-snapshot -restore)
    PRICING_SNAPSHOT_RESTORED=1
}

if [ -n "$FIXTURE" ] && [ -f "$FIXTURE" ] && [ -x "$FIXTURE" ]; then
    echo "Using pre-built fixture: $FIXTURE"
else
    echo "Building test fixture..."
    restore_pricing_snapshot
    FIXTURE="$TMPDIR/testfixture"
    CGO_ENABLED=1 go build -tags "fts5,kit_posthog_disabled" \
      -o "$FIXTURE" "$ROOT/cmd/testfixture"
fi
fixture_args=(-out "$DB_PATH")
if [ "$BACKEND" = "duckdb" ]; then
  fixture_args+=(-duckdb-out "$DUCKDB_PATH")
fi
"$FIXTURE" "${fixture_args[@]}"

if [ -n "$SERVER" ] && [ -f "$SERVER" ] && [ -x "$SERVER" ]; then
    echo "Using pre-built server: $SERVER"
else
    echo "Building server..."
    restore_pricing_snapshot
    SERVER="$TMPDIR/agentsview"
    cd "$ROOT/frontend" && npm run build
    rm -rf "$ROOT/internal/web/dist"
    cp -r "$ROOT/frontend/dist" "$ROOT/internal/web/dist"
    printf '%s\n' \
      'keep embed dir for generated frontend assets' \
      > "$ROOT/internal/web/dist/.keep"
    CGO_ENABLED=1 go build -tags "fts5,kit_posthog_disabled" \
      -o "$SERVER" "$ROOT/cmd/agentsview"
fi

# Run server with test DB, no sync dirs, fixed port. Every agent dir override
# must point at EMPTY_DIR so the server never discovers real sessions on the
# host. The list is derived from the parser registry (the EnvVar fields in
# internal/parser/types.go), so registering a new agent cannot silently
# reintroduce a discovery leak by leaving its dir env var pointed at the host.
agent_env=("AGENTSVIEW_DATA_DIR=$TMPDIR")
while IFS= read -r agent_var; do
  agent_env+=("$agent_var=$EMPTY_DIR")
done < <(grep -oE 'EnvVar:[[:space:]]*"[A-Z0-9_]+"' "$ROOT/internal/parser/types.go" \
  | grep -oE '"[A-Z0-9_]+"' | tr -d '"' | sort -u)

# Sanity floor: a stale grep (e.g. types.go reformatted) would yield an empty
# list and silently re-leak host sessions. The registry has dozens of agents;
# far fewer than this means the parse broke, so fail loudly instead.
min_agent_dirs=10
if [ "${#agent_env[@]}" -le "$min_agent_dirs" ]; then
  echo "e2e isolation: derived only $(( ${#agent_env[@]} - 1 )) agent dir" \
    "overrides from internal/parser/types.go; registry parsing is stale" >&2
  exit 1
fi

case "$BACKEND" in
  sqlite)
    echo "Starting sqlite e2e server on :8090..."
    exec env "${agent_env[@]}" "$SERVER" serve \
      --port 8090 \
      --no-browser
    ;;
  duckdb)
    echo "Starting duckdb e2e server on :8090..."
    exec env "${agent_env[@]}" \
      AGENTSVIEW_DUCKDB_PATH="$DUCKDB_PATH" \
      "$SERVER" duckdb serve \
      --port 8090 \
      --no-browser
    ;;
  *)
    echo "unsupported AGENTSVIEW_E2E_BACKEND=$BACKEND" >&2
    exit 1
    ;;
esac
