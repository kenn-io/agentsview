#!/usr/bin/env bash
set -euo pipefail
umask 077

export AGENTSVIEW_TELEMETRY_ENABLED=0
export AGENTSVIEW_NO_DAEMON=1
export AGENTSVIEW_DATA_DIR=/state/sqlite
export TZ=UTC
APP=/application/agentsview
PORT=8090
PG_PORT=8091
mkdir -p /state/home /state/sqlite /state/pg /work/tests
printf '%s\n' '11111111111141118111111111111111' > /state/sqlite/telemetry-install-id
printf '%s\n' 'local_machine_name = "dev-laptop"' > /state/sqlite/config.toml
cp /capture/playwright.config.ts /work/
cp /capture/tests/*.ts /work/tests/
ln -s /opt/playwright/node_modules /work/node_modules
cd /work

SERVER_PID=""
PG_SERVER_PID=""
cleanup() {
  for pid in "$SERVER_PID" "$PG_SERVER_PID"; do
    if [[ -n "$pid" ]]; then kill "$pid" 2>/dev/null || true; fi
  done
}
trap cleanup EXIT
cmp /capture/package-lock.json /opt/playwright/package-lock.json
node -e 'const p = require("playwright"); const c = require("child_process"); console.log(JSON.stringify({node: process.version, playwright: require("playwright/package.json").version, browser: c.execFileSync(p.chromium.executablePath(), ["--version"], {encoding:"utf8"}).trim()}));' > /result/runtime.json
chown "$CAPTURE_UID:$CAPTURE_GID" /result/runtime.json

wait_ready() {
  local port="$1"
  local pid="$2"
  for _ in $(seq 1 60); do
    kill -0 "$pid" || return 1
    if curl -fsS "http://127.0.0.1:$port/api/v1/stats" > /dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "Application did not become ready on port $port" >&2
  return 1
}

# Discover configurable providers from this exact application, with empty state
# and no background sync. No host home or provider variables enter the container.
"$APP" serve --port "$PORT" --no-sync --no-browser --no-update-check &
SERVER_PID=$!
wait_ready "$PORT" "$SERVER_PID"
curl -fsS "http://127.0.0.1:$PORT/api/v1/settings" > /state/providers.json
kill "$SERVER_PID"
wait "$SERVER_PID" || true
SERVER_PID=""
python3 /capture/fixture.py configure /corpus/manifest.json /state/sqlite/config.toml \
  --settings /state/providers.json
"$APP" sync
python3 /capture/fixture.py apply /corpus/manifest.json /state/sqlite/sessions.db

PG_VER=$(pg_lsclusters -h | awk 'NR == 1 {print $1}')
[[ -n "$PG_VER" ]] || { echo 'No PostgreSQL cluster found' >&2; exit 1; }
pg_ctlcluster "$PG_VER" main start
su postgres -c 'createuser agentsview'
su postgres -c 'createdb -O agentsview agentsview'
PG_URL='postgres://agentsview@127.0.0.1:5432/agentsview?sslmode=disable'
cat >> /state/sqlite/config.toml <<TOML

[pg]
url = "$PG_URL"
machine_name = "dev-laptop"
allow_insecure = true
TOML
"$APP" pg push
psql -U agentsview -h 127.0.0.1 -d agentsview -q -v ON_ERROR_STOP=1 <<'SQL'
SET search_path TO agentsview;
UPDATE sessions SET machine = 'work-desktop'
WHERE id IN (
  SELECT id FROM sessions ORDER BY created_at DESC, id ASC
  LIMIT GREATEST(1, (SELECT COUNT(*) / 3 FROM sessions))
);
SQL

"$APP" serve --port "$PORT" --no-sync --no-browser --no-update-check &
SERVER_PID=$!
cat > /state/pg/config.toml <<TOML
local_machine_name = "dev-laptop"
[pg]
url = "$PG_URL"
machine_name = "dev-laptop"
allow_insecure = true
TOML
AGENTSVIEW_DATA_DIR=/state/pg "$APP" pg serve --port "$PG_PORT" --no-browser &
PG_SERVER_PID=$!
wait_ready "$PORT" "$SERVER_PID"
wait_ready "$PG_PORT" "$PG_SERVER_PID"
if curl -fsS -D - "http://127.0.0.1:$PORT/api/v1/stats" -o /dev/null \
  | grep -qi '^X-Agentsview-Archive-Stale: true'; then
  echo 'Screenshot archive still needs a rebuild' >&2
  exit 1
fi
"$APP" usage daily --no-sync > /dev/null

# Write browser artifacts as the worker, including when a container is killed.
mkdir -p /work/home
chown -R "$CAPTURE_UID:$CAPTURE_GID" /work
PG_BASE_URL="http://127.0.0.1:$PG_PORT" \
  setpriv --reuid="$CAPTURE_UID" --regid="$CAPTURE_GID" --clear-groups \
    env HOME=/work/home node /opt/playwright/node_modules/@playwright/test/cli.js test --reporter=list
