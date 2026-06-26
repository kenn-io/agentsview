#!/usr/bin/env bash
set -euo pipefail

PORT=8090
PG_PORT=8091
DATA_DIR=$(mktemp -d)
PG_DATA_DIR=$(mktemp -d)
EMPTY_DIR="$DATA_DIR/empty"
mkdir -p "$EMPTY_DIR" /output

# Copy test database so the server doesn't modify the original
cp /data/sessions.db "$DATA_DIR/sessions.db"

# ── Start PostgreSQL ─────────────────────────────────────
# The cluster is auto-created by the Debian package. pg_hba.conf
# was set to trust auth in the Dockerfile. Detect the installed
# PG version rather than hardcoding it.
echo "Starting PostgreSQL..."
PG_CLUSTER=$(pg_lsclusters -h 2>/dev/null | head -1)
PG_VER=$(echo "$PG_CLUSTER" | awk '{print $1}')
PG_PGPORT=$(echo "$PG_CLUSTER" | awk '{print $3}')
if [ -z "$PG_VER" ]; then
  echo "Error: no PostgreSQL cluster found"
  exit 1
fi
export PGPORT="$PG_PGPORT"
su postgres -c "pg_ctlcluster $PG_VER main start"

for i in $(seq 1 15); do
  if su postgres -c "pg_isready -q"; then break; fi
  if [ "$i" -eq 15 ]; then
    echo "Error: PostgreSQL failed to start"
    exit 1
  fi
  sleep 1
done

# Create role and database
su postgres -c "createuser agentsview" 2>/dev/null || true
su postgres -c "createdb -O agentsview agentsview" 2>/dev/null || true
PG_URL="postgres://agentsview@127.0.0.1:${PG_PGPORT}/agentsview?sslmode=disable"

echo "PostgreSQL ready."

# ── Push test data to PostgreSQL ─────────────────────────
echo "Pushing test data to PostgreSQL..."

cat > "$DATA_DIR/config.toml" <<TOML
[pg]
url = "$PG_URL"
machine_name = "dev-laptop"
allow_insecure = true
TOML

AGENTSVIEW_DATA_DIR="$DATA_DIR" \
CLAUDE_PROJECTS_DIR="$EMPTY_DIR" \
CODEX_SESSIONS_DIR="$EMPTY_DIR" \
GEMINI_DIR="$EMPTY_DIR" \
agentsview pg push

# Simulate a second machine by relabeling a subset of sessions
# directly in PG. This gives the UI multi-machine data so
# machine labels appear on session items.
psql -U agentsview -h 127.0.0.1 -d agentsview -q -v ON_ERROR_STOP=1 <<SQL
SET search_path TO agentsview;
UPDATE sessions
SET machine = 'work-desktop'
WHERE id IN (
  SELECT id FROM sessions
  ORDER BY created_at DESC
  LIMIT GREATEST(1, (SELECT COUNT(*) / 3 FROM sessions))
);
SQL

echo "PG data ready (two machines)."

# ── Seed usage pricing via CLI ───────────────────────────
# Workaround for a 0.21.0 bug where the /usage dashboard can
# load before pricing is populated. Running `usage daily` here
# synchronously fills the model_pricing table so the web UI
# renders cost numbers on first load. Use --offline so we rely
# on the embedded fallback catalog instead of reaching the
# network from inside the container.
echo "Seeding usage pricing..."
AGENTSVIEW_DATA_DIR="$DATA_DIR" \
CLAUDE_PROJECTS_DIR="$EMPTY_DIR" \
CODEX_SESSIONS_DIR="$EMPTY_DIR" \
GEMINI_DIR="$EMPTY_DIR" \
agentsview usage daily --offline --no-sync > /dev/null

# ── Start agentsview (SQLite mode) ───────────────────────
# 0.23.0 requires the explicit `serve` subcommand; plain
# `agentsview` now prints help instead of starting the server.
echo "Starting agentsview on port $PORT..."
AGENTSVIEW_DATA_DIR="$DATA_DIR" \
CLAUDE_PROJECTS_DIR="$EMPTY_DIR" \
CODEX_SESSIONS_DIR="$EMPTY_DIR" \
GEMINI_DIR="$EMPTY_DIR" \
agentsview serve --port "$PORT" &
SERVER_PID=$!

# ── Start agentsview pg serve ────────────────────────────
echo "Starting agentsview pg serve on port $PG_PORT..."

cat > "$PG_DATA_DIR/config.toml" <<TOML
[pg]
url = "$PG_URL"
machine_name = "dev-laptop"
allow_insecure = true
TOML

AGENTSVIEW_DATA_DIR="$PG_DATA_DIR" \
agentsview pg serve --port "$PG_PORT" &
PG_SERVER_PID=$!

# ── Wait for both servers ────────────────────────────────
echo "Waiting for servers..."
SQLITE_OK=false
PG_OK=false
for i in $(seq 1 30); do
  if ! $SQLITE_OK && curl -sf "http://127.0.0.1:$PORT/api/v1/stats" > /dev/null 2>&1; then
    SQLITE_OK=true
  fi
  if ! $PG_OK && curl -sf "http://127.0.0.1:$PG_PORT/api/v1/stats" > /dev/null 2>&1; then
    PG_OK=true
  fi
  if $SQLITE_OK && $PG_OK; then
    echo "Both servers ready."
    break
  fi
  if [ "$i" -eq 30 ]; then
    echo "Error: server(s) failed to start (sqlite=$SQLITE_OK, pg=$PG_OK)"
    kill $SERVER_PID 2>/dev/null || true
    kill $PG_SERVER_PID 2>/dev/null || true
    exit 1
  fi
  sleep 1
done

# ── Run Playwright ───────────────────────────────────────
echo ""
echo "Capturing screenshots..."
EXIT_CODE=0
SCREENSHOT_DIR=/output \
PG_BASE_URL="http://127.0.0.1:$PG_PORT" \
npx playwright test --reporter=list "$@" 2>&1 || EXIT_CODE=$?

echo ""
if [ -d /output ]; then
  COUNT=$(ls -1 /output/*.png 2>/dev/null | wc -l)
  echo "Captured $COUNT screenshots"
  ls -la /output/*.png 2>/dev/null || true
fi

kill $SERVER_PID 2>/dev/null || true
kill $PG_SERVER_PID 2>/dev/null || true
exit $EXIT_CODE
