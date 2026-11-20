#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
first=
cleanup() {
    touch "$tmp/release"
    [[ -z "$first" ]] || wait "$first" 2>/dev/null || true
    if [[ -f "$tmp/ready" ]]; then
        for _ in {1..100}; do [[ -f "$tmp/exited" ]] && break; sleep .1; done
    fi
    find "$tmp" -depth -delete
}
trap cleanup EXIT
docker_bin=$(command -v docker)
python_bin=$(command -v python3)
if command -v mise >/dev/null 2>&1 && [[ "$python_bin" == */mise/shims/* ]]; then python_bin=$(mise which python3); fi
export HOME="$tmp/home"
mkdir -p "$HOME" "$tmp/bin"
cat > "$tmp/bin/docker" <<'DOCKER'
#!/bin/sh
printf '%s\n' "$*" >> "$DOCKER_LOG"
case " $* " in
  *' up '*)
    [ "$2" = -p ] && [ "$3" = "$TEST_SERVICE_LEASE" ] || exit 1
    python3 - "$HOME/.local/state/test-service-leases/$TEST_SERVICE_LEASE" <<'PROBE' || exit 1
import fcntl, sys
with open(sys.argv[1]) as f:
    try: fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError: sys.exit(0)
sys.exit(1)
PROBE
    printf '%s\n' "$TEST_SERVICE_LEASE" > "$CHECK_LEASE" ;;
  *' port '*) echo 127.0.0.1:49153 ;;
esac
DOCKER
chmod +x "$tmp/bin/docker"
ln -s "$python_bin" "$tmp/bin/python3"
export PATH="$tmp/bin:$PATH" DOCKER_LOG="$tmp/docker.log"

# Both services carry the project lease label in the resolved Compose config.
(cd "$root" && TEST_SERVICE_LEASE=agentsview-test-123 "$docker_bin" compose -p agentsview-test-123 -f docker-compose.test.yml -f docker-compose.test-lease.yml config --format json) | python3 -c '
import json, sys
services = json.load(sys.stdin)["services"]
for name in ("postgres", "clickhouse"):
    assert services[name]["labels"]["test-service.lease"] == "agentsview-test-123"
'

probe() {
    python3 - "$lease" <<'PY'
import fcntl, sys
with open(sys.argv[1]) as f:
    try: fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError: sys.exit(1)
PY
}

# A descendant retains the lease after the main process exits.
export READY="$tmp/ready" RELEASE="$tmp/release" EXITED="$tmp/exited" CHECK_LEASE="$tmp/project"
: > "$DOCKER_LOG"
(cd "$root" && ./scripts/test-lease.sh ./scripts/test-service-compose.sh postgres 5432 sh -c 'sh -c '\''touch "$READY"; while [ ! -f "$RELEASE" ]; do sleep 0.1; done; touch "$EXITED"'\'' >/dev/null 2>&1 & while [ ! -f "$READY" ]; do sleep 0.1; done; exit 7') &
first=$!
for _ in {1..100}; do [[ -f "$READY" ]] && break; sleep .1; done
[[ -f "$READY" ]]
lease="$HOME/.local/state/test-service-leases/$(cat "$CHECK_LEASE")"
if probe; then echo 'lease released while descendant still runs' >&2; exit 1; fi
set +e
wait "$first"
status=$?
set -e
first=
[[ $status == 7 ]]
if probe; then echo 'lease released while descendant still runs' >&2; exit 1; fi
touch "$RELEASE"
for _ in {1..100}; do [[ -f "$EXITED" ]] && break; sleep .1; done
[[ -f "$EXITED" ]]
for _ in {1..100}; do if probe; then break; fi; sleep .1; done
probe
unset READY RELEASE EXITED

# Dry runs do not start services or create leases.
: > "$DOCKER_LOG"
# Makefile reads Go environment values during parsing, even for `make -n`.
# Keep those lookups from downloading a toolchain into the temporary HOME.
cat > "$tmp/bin/go" <<'GO'
#!/bin/sh
[ "${1:-}" = env ] || { echo "unexpected go command: $*" >&2; exit 1; }
GO
chmod +x "$tmp/bin/go"
(cd "$root" && make -n test-postgres test-clickhouse bench-pg-usage) > "$tmp/make"
[[ ! -s "$DOCKER_LOG" ]]

# Each target passes the new lease and service address to nested Make.
cat > "$tmp/inspect.mk" <<'MAKE'
$(if $(filter 1,$(MAKELEVEL)),$(info TARGET_FROM_MAKE=$(MAKECMDGOALS))$(info LEASE_FROM_MAKE=$(TEST_SERVICE_LEASE))$(info PG_FROM_MAKE=$(TEST_PG_URL))$(info CH_FROM_MAKE=$(TEST_CLICKHOUSE_URL)))
MAKE
for target in test-postgres bench-pg-usage test-clickhouse; do
    : > "$DOCKER_LOG"
    (cd "$root" && MAKEFILES="$tmp/inspect.mk" make -o pricing-snapshot "$target" TEST_SERVICE_LEASE=old TEST_PG_URL=old-pg TEST_CLICKHOUSE_URL=old-ch MAKE='make -n') > "$tmp/make-url"
    project=$(cat "$CHECK_LEASE")
    [[ "$project" != old ]]
    grep -Fxq "TARGET_FROM_MAKE=$target-run" "$tmp/make-url"
    grep -Fxq "LEASE_FROM_MAKE=$project" "$tmp/make-url"
    case "$target" in
        test-clickhouse)
            service=clickhouse port=9000
            grep -Fxq 'CH_FROM_MAKE=clickhouse://127.0.0.1:49153/default' "$tmp/make-url" ;;
        *)
            service=postgres port=5432
            grep -Fxq 'PG_FROM_MAKE=postgres://agentsview_test:agentsview_test_password@127.0.0.1:49153/agentsview_test?sslmode=disable' "$tmp/make-url" ;;
    esac
    grep -qE -- "-p $project .* -f docker-compose.test-lease.yml up -d --wait $service" "$DOCKER_LOG"
    grep -qE -- "-p $project .* port $service $port" "$DOCKER_LOG"
done

# Missing prerequisites stop before Docker starts.
mkdir "$tmp/no-python"
ln -s "$tmp/bin/docker" "$tmp/no-python/docker"
: > "$DOCKER_LOG"
set +e
PATH="$tmp/no-python" /bin/sh "$root/scripts/test-lease.sh" docker info >"$tmp/no-python.out" 2>&1
status=$?
set -e
[[ $status == 2 && ! -s "$DOCKER_LOG" ]]
grep -q 'python3 is required' "$tmp/no-python.out"
set +e
HOME= "$root/scripts/test-lease.sh" docker info >"$tmp/home-error" 2>&1
status=$?
set -e
[[ $status != 0 && ! -s "$DOCKER_LOG" ]]
grep -q 'HOME must be set' "$tmp/home-error"

# A loose lease directory is rejected before Docker starts.
chmod 755 "$HOME/.local/state/test-service-leases"
: > "$DOCKER_LOG"
set +e
"$root/scripts/test-lease.sh" docker info >"$tmp/mode-error" 2>&1
status=$?
set -e
[[ $status != 0 && ! -s "$DOCKER_LOG" ]]
grep -q 'lease directory must be owned' "$tmp/mode-error"
