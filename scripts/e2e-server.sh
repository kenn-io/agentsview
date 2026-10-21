#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
E2E_TMP="$(mktemp -d)"
server_pid=""
docbank_pid=""
cleanup() {
  for child in "$server_pid" "$docbank_pid"; do
    if [[ -n "$child" ]]; then
      kill "$child" 2>/dev/null || true
      wait "$child" 2>/dev/null || true
    fi
  done
  rm -rf "$E2E_TMP"
}
trap cleanup EXIT
trap 'exit 143' TERM
trap 'exit 130' INT

DB_PATH="$E2E_TMP/sessions.db"
DUCKDB_PATH="$E2E_TMP/sessions.duckdb"
EMPTY_DIR="$E2E_TMP/empty"
BACKEND="${AGENTSVIEW_E2E_BACKEND:-sqlite}"
raw_e2e_port="${AGENTSVIEW_E2E_PORT-}"
if [[ -z "$raw_e2e_port" ]]; then
  E2E_PORT=8090
elif [[ ! "$raw_e2e_port" =~ ^[0-9]+$ ]]; then
  printf 'AGENTSVIEW_E2E_PORT must be an integer from 1 to 65535 (got %q)\n' \
    "$raw_e2e_port" >&2
  exit 1
else
  normalized_e2e_port="${raw_e2e_port#"${raw_e2e_port%%[!0]*}"}"
  if [[ -z "$normalized_e2e_port" ]]; then
    normalized_e2e_port=0
  fi
  if [[ ${#normalized_e2e_port} -gt 5 ]]; then
    printf 'AGENTSVIEW_E2E_PORT must be an integer from 1 to 65535 (got %q)\n' \
      "$raw_e2e_port" >&2
    exit 1
  fi
  E2E_PORT=$((10#$normalized_e2e_port))
  if [[ "$E2E_PORT" -lt 1 || "$E2E_PORT" -gt 65535 ]]; then
    printf 'AGENTSVIEW_E2E_PORT must be an integer from 1 to 65535 (got %q)\n' \
      "$raw_e2e_port" >&2
    exit 1
  fi
fi
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
    FIXTURE="$E2E_TMP/testfixture"
    CGO_ENABLED=1 go build -tags "fts5,kit_posthog_disabled" \
      -o "$FIXTURE" "$ROOT/cmd/testfixture"
fi
fixture_args=(-out "$DB_PATH")
if [ "$BACKEND" = "duckdb" ]; then
  fixture_args+=(-duckdb-out "$DUCKDB_PATH")
else
  fixture_args+=(-file-evidence)
fi
"$FIXTURE" "${fixture_args[@]}"
if [ "$BACKEND" = "sqlite" ]; then
  "$FIXTURE" -out "$DB_PATH" -docbank-config "$E2E_TMP/config.toml" &
  docbank_pid=$!
  for ((attempt = 0; attempt < 100; attempt++)); do
    [[ -f "$E2E_TMP/config.toml" ]] && break
    kill -0 "$docbank_pid" 2>/dev/null || { echo 'Docbank fixture exited before readiness' >&2; exit 1; }
    sleep 0.05
  done
  [[ -f "$E2E_TMP/config.toml" ]] || { echo 'Docbank fixture readiness timed out' >&2; exit 1; }
fi

if [ -n "$SERVER" ] && [ -f "$SERVER" ] && [ -x "$SERVER" ]; then
    echo "Using pre-built server: $SERVER"
else
    echo "Building server..."
    restore_pricing_snapshot
    SERVER="$E2E_TMP/agentsview"
    cd "$ROOT/frontend" && \
      VITE_PROJECT_MAPPING_WORKSPACE="${VITE_PROJECT_MAPPING_WORKSPACE:-true}" \
      npm run build
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
agent_env=("AGENTSVIEW_DATA_DIR=$E2E_TMP")
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
    echo "Starting sqlite e2e server on :$E2E_PORT..."
    env "${agent_env[@]}" "$SERVER" serve \
      --port "$E2E_PORT" \
      --no-browser &
    server_pid=$!
    wait "$server_pid"
    ;;
  duckdb)
    echo "Starting duckdb e2e server on :$E2E_PORT..."
    env "${agent_env[@]}" \
      AGENTSVIEW_DUCKDB_PATH="$DUCKDB_PATH" \
      "$SERVER" duckdb serve \
      --port "$E2E_PORT" \
      --no-browser &
    server_pid=$!
    wait "$server_pid"
    ;;
  *)
    echo "unsupported AGENTSVIEW_E2E_BACKEND=$BACKEND" >&2
    exit 1
    ;;
esac
