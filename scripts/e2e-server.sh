#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

DB_PATH="$TMPDIR/sessions.db"
EMPTY_DIR="$TMPDIR/empty"
mkdir -p "$EMPTY_DIR"

# Use pre-built binaries if available (CI sets these),
# otherwise build from source (local dev).
FIXTURE="${E2E_PREBUILT_FIXTURE:-}"
SERVER="${E2E_PREBUILT_SERVER:-}"

if [ -n "$FIXTURE" ] && [ -f "$FIXTURE" ] && [ -x "$FIXTURE" ]; then
    echo "Using pre-built fixture: $FIXTURE"
else
    echo "Building test fixture..."
    FIXTURE="$TMPDIR/testfixture"
    CGO_ENABLED=1 go build -tags fts5 \
      -o "$FIXTURE" "$ROOT/cmd/testfixture"
fi
"$FIXTURE" -out "$DB_PATH"

if [ -n "$SERVER" ] && [ -f "$SERVER" ] && [ -x "$SERVER" ]; then
    echo "Using pre-built server: $SERVER"
else
    echo "Building server..."
    SERVER="$TMPDIR/agentsview"
    cd "$ROOT/frontend" && npm run build
    rm -rf "$ROOT/internal/web/dist"
    cp -r "$ROOT/frontend/dist" "$ROOT/internal/web/dist"
    printf '%s\n' \
      'keep embed dir for generated frontend assets' \
      > "$ROOT/internal/web/dist/.keep"
    CGO_ENABLED=1 go build -tags fts5 \
      -o "$SERVER" "$ROOT/cmd/agentsview"
fi

# Run server with test DB, no sync dirs, fixed port.
# Every agent dir must point to EMPTY_DIR to prevent
# the server from discovering real sessions on the host.
echo "Starting e2e server on :8090..."
AGENT_VIEWER_DATA_DIR="$TMPDIR" \
CLAUDE_PROJECTS_DIR="$EMPTY_DIR" \
CODEX_SESSIONS_DIR="$EMPTY_DIR" \
COPILOT_DIR="$EMPTY_DIR" \
GEMINI_DIR="$EMPTY_DIR" \
OPENCODE_DIR="$EMPTY_DIR" \
CURSOR_PROJECTS_DIR="$EMPTY_DIR" \
AMP_DIR="$EMPTY_DIR" \
IFLOW_DIR="$EMPTY_DIR" \
VSCODE_COPILOT_DIR="$EMPTY_DIR" \
PI_DIR="$EMPTY_DIR" \
OPENCLAW_DIR="$EMPTY_DIR" \
exec "$SERVER" serve \
  --port 8090 \
  --no-browser
