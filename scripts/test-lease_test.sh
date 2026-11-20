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
    printf 'publish %s %s %s\n' "${TEST_PG_PUBLISH-}" "${TEST_CH_HTTP_PUBLISH-}" "${TEST_CH_NATIVE_PUBLISH-}" >> "$DOCKER_LOG"
    lease-held "$HOME/.local/state/test-service-leases/$TEST_SERVICE_LEASE" || exit 1
    printf '%s\n' "$TEST_SERVICE_LEASE" > "$CHECK_LEASE"
    if [ "${BLOCK_UP:-}" = 1 ]; then touch "$READY"; while :; do sleep .1; done; fi ;;
  *' port '*) echo 127.0.0.1:49153 ;;
  *' down '*)
    [ "$2" = -p ] && [ "$3" = "$TEST_SERVICE_LEASE" ] || exit 1
    lease-held "$HOME/.local/state/test-service-leases/$TEST_SERVICE_LEASE" || exit 1
    exit "${DOWN_STATUS:-0}" ;;
esac
DOCKER
cat > "$tmp/bin/lease-held" <<'HELD'
#!/usr/bin/env python3
import fcntl, sys
with open(sys.argv[1]) as f:
    try: fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError: sys.exit(0)
sys.exit(1)
HELD
chmod +x "$tmp/bin/docker" "$tmp/bin/lease-held"
ln -s "$python_bin" "$tmp/bin/python3"
export PATH="$tmp/bin:$PATH" DOCKER_LOG="$tmp/docker.log"

# Both services carry the project lease label in the resolved Compose config.
(cd "$root" && TEST_SERVICE_LEASE=agentsview-test-123 "$docker_bin" compose -p agentsview-test-123 -f docker-compose.test.yml -f docker-compose.test-lease.yml config --format json) | python3 -c '
import json, sys
services = json.load(sys.stdin)["services"]
for name in ("postgres", "clickhouse"):
    assert services[name]["labels"]["test-service.lease"] == "agentsview-test-123"
'

# Cleanup waits for a descendant to release the lease.
export READY="$tmp/ready" RELEASE="$tmp/release" EXITED="$tmp/exited" MAIN="$tmp/main" CHECK_LEASE="$tmp/project"
: > "$DOCKER_LOG"
(cd "$root" && ./scripts/test-lease.sh ./scripts/test-service-compose.sh postgres 5432 sh -c 'echo $$ > "$MAIN"; sh -c '\''touch "$READY"; while [ ! -f "$RELEASE" ]; do sleep 0.1; done; touch "$EXITED"'\'' >/dev/null 2>&1 & while [ ! -f "$READY" ]; do sleep 0.1; done; exit 7') &
first=$!
for _ in {1..100}; do [[ -f "$READY" ]] && break; sleep .1; done
[[ -f "$READY" ]]
main_pid=$(cat "$MAIN")
for _ in {1..100}; do if ! kill -0 "$main_pid" 2>/dev/null; then break; fi; sleep .1; done
if kill -0 "$main_pid" 2>/dev/null; then echo 'foreground command did not exit' >&2; exit 1; fi
lease="$HOME/.local/state/test-service-leases/$(cat "$CHECK_LEASE")"
lease-held "$lease"
if grep -q ' down ' "$DOCKER_LOG"; then echo 'cleanup ran while descendant held lease' >&2; exit 1; fi
touch "$RELEASE"
for _ in {1..100}; do [[ -f "$EXITED" ]] && break; sleep .1; done
[[ -f "$EXITED" ]]
set +e
wait "$first"
status=$?
set -e
first=
[[ $status == 7 ]]
grep -qE -- "-p $(cat "$CHECK_LEASE") .* down -v --remove-orphans" "$DOCKER_LOG"
[[ ! -e "$lease" ]]
unset READY RELEASE EXITED MAIN

# A successful command reports cleanup failure; a failed command keeps its status.
for command_status in 0 7; do
    : > "$DOCKER_LOG"
    set +e
    (cd "$root" && DOWN_STATUS=1 ./scripts/test-lease.sh ./scripts/test-service-compose.sh postgres 5432 sh -c "exit $command_status") > "$tmp/cleanup-error" 2>&1
    status=$?
    set -e
    if [[ $command_status == 0 ]]; then [[ $status == 1 ]]; else [[ $status == 7 ]]; fi
    project=$(cat "$CHECK_LEASE")
    grep -Fxq "cleanup failed; run from the repository root: TEST_SERVICE_LEASE=$project docker compose -p $project -f docker-compose.test.yml -f docker-compose.test-lease.yml down -v --remove-orphans" "$tmp/cleanup-error"
    grep -q ' down ' "$DOCKER_LOG"
    [[ -f "$HOME/.local/state/test-service-leases/$(cat "$CHECK_LEASE")" ]]
done

# Termination during startup and while the command runs cleans up the project.
export READY="$tmp/ready"
for phase in startup command; do
    : > "$DOCKER_LOG"
    find "$tmp/ready" -delete
    if [[ $phase == startup ]]; then
        (cd "$root" && BLOCK_UP=1 exec ./scripts/test-lease.sh ./scripts/test-service-compose.sh postgres 5432 true) &
    else
        (cd "$root" && exec ./scripts/test-lease.sh ./scripts/test-service-compose.sh postgres 5432 sh -c 'touch "$READY"; exec sleep 30') &
    fi
    first=$!
    for _ in {1..100}; do [[ -f "$READY" ]] && break; sleep .1; done
    [[ -f "$READY" ]]
    kill -TERM "$first"
    set +e
    wait "$first"
    status=$?
    set -e
    first=
    [[ $status == 143 ]]
    grep -qE -- "-p $(cat "$CHECK_LEASE") .* down -v --remove-orphans" "$DOCKER_LOG"
done

# Dry runs do not start services.
: > "$DOCKER_LOG"
# Makefile reads Go environment values during parsing, even for `make -n`.
# Keep those lookups from downloading a toolchain into the temporary HOME.
cat > "$tmp/bin/go" <<'GO'
#!/bin/sh
[ "${1:-}" = env ] || { echo "unexpected go command: $*" >&2; exit 1; }
GO
chmod +x "$tmp/bin/go"
(cd "$root" && make -n test-postgres test-clickhouse bench-pg-usage) > /dev/null
[[ ! -s "$DOCKER_LOG" ]]

# Each target passes the new lease and service address to nested Make.
cat > "$tmp/inspect.mk" <<'MAKE'
$(if $(filter 1,$(MAKELEVEL)),$(info TARGET_FROM_MAKE=$(MAKECMDGOALS))$(info LEASE_FROM_MAKE=$(TEST_SERVICE_LEASE))$(info PG_FROM_MAKE=$(TEST_PG_URL))$(info CH_FROM_MAKE=$(TEST_CLICKHOUSE_URL))$(info STANDBY_FROM_MAKE=$(TEST_PG_STANDBY_URL)))
MAKE
for target in test-postgres bench-pg-usage test-clickhouse; do
    : > "$DOCKER_LOG"
    (cd "$root" && MAKEFILES="$tmp/inspect.mk" make -o pricing-snapshot "$target" TEST_SERVICE_LEASE=old TEST_PG_URL=old-pg TEST_CLICKHOUSE_URL=old-ch TEST_PG_STANDBY_URL=old-standby MAKE='make -n') > "$tmp/make-url"
    project=$(cat "$CHECK_LEASE")
    [[ "$project" != old ]]
    grep -Fxq "TARGET_FROM_MAKE=$target-run" "$tmp/make-url"
    grep -Fxq "LEASE_FROM_MAKE=$project" "$tmp/make-url"
    case "$target" in
        test-clickhouse)
            service=clickhouse port=9000
            grep -Fxq 'STANDBY_FROM_MAKE=' "$tmp/make-url"
            grep -Fxq 'PG_FROM_MAKE=' "$tmp/make-url"
            grep -Fxq 'CH_FROM_MAKE=clickhouse://127.0.0.1:49153/default' "$tmp/make-url"
            grep -Fxq 'publish 127.0.0.1::5432 127.0.0.1::8123 127.0.0.1::9000' "$DOCKER_LOG" ;;
        test-postgres)
            service=postgres port=5432
            grep -Fxq 'STANDBY_FROM_MAKE=' "$tmp/make-url"
            grep -Fxq 'CH_FROM_MAKE=' "$tmp/make-url"
            grep -Fxq 'PG_FROM_MAKE=postgres://agentsview_test:agentsview_test_password@127.0.0.1:49153/agentsview_test?sslmode=disable' "$tmp/make-url"
            grep -Fxq 'publish 127.0.0.1::5432 127.0.0.1::8123 127.0.0.1::9000' "$DOCKER_LOG" ;;
        bench-pg-usage) service=postgres port=5432 ;;
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
HOME= "$root/scripts/test-lease.sh" true >"$tmp/home-error" 2>&1
status=$?
set -e
[[ $status == 2 && ! -s "$DOCKER_LOG" ]]
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
