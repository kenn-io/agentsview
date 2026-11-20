#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
first=
shutdown_release="$tmp/shutdown-release"
cleanup() {
    touch "$tmp/release"
    touch "$tmp/down-release"
    touch "$shutdown_release"
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
  *' ps -aq '*) [ "$*" = "ps -aq --filter label=test-service.lease=$TEST_SERVICE_LEASE" ] || exit 1; [ "${LEFTOVER:-0}" = 0 ] || [ -f "$REMOVED" ] || echo container-1 ;;
  *' rm -f -v '*) [ "$*" = 'rm -f -v container-1' ] || exit 1; [ "${REMOVE_STATUS:-0}" = 0 ] || exit "$REMOVE_STATUS"; touch "$REMOVED" ;;
  *' up '*)
    [ "$2" = -p ] && [ "$3" = "$TEST_SERVICE_LEASE" ] || exit 1
    case " $* " in
      *' up -d --wait postgres '*)
        [ "${TEST_PG_PUBLISH-}" = 127.0.0.1::5432 ] && [ -z "${TEST_CH_HTTP_PUBLISH-}" ] && [ -z "${TEST_CH_NATIVE_PUBLISH-}" ] || exit 1 ;;
      *' up -d --wait clickhouse '*)
        [ -z "${TEST_PG_PUBLISH-}" ] && [ "${TEST_CH_HTTP_PUBLISH-}" = 127.0.0.1::8123 ] && [ "${TEST_CH_NATIVE_PUBLISH-}" = 127.0.0.1::9000 ] || exit 1 ;;
    esac
    lease-held "$HOME/.local/state/test-service-leases/$TEST_SERVICE_LEASE" || exit 1
    printf '%s\n' "$TEST_SERVICE_LEASE" > "$CHECK_LEASE"
    if [ "${BLOCK_UP:-}" = 1 ]; then touch "$READY"; while :; do sleep .1; done; fi ;;
  *' port postgres 5432 '*) echo 127.0.0.1:49153 ;;
  *' port clickhouse 9000 '*) echo 127.0.0.1:49153 ;;
  *' port '*) exit 1 ;;
  *' down '*)
    [ "$2" = -p ] && [ "$3" = "$TEST_SERVICE_LEASE" ] || exit 1
    [ "$TEST_PG_PUBLISH" = 127.0.0.1::5432 ] && [ "$TEST_CH_HTTP_PUBLISH" = 127.0.0.1::8123 ] && [ "$TEST_CH_NATIVE_PUBLISH" = 127.0.0.1::9000 ] || exit 1
    lease-held "$HOME/.local/state/test-service-leases/$TEST_SERVICE_LEASE" || exit 1
    if [ "${BLOCK_DOWN:-0}" = 1 ]; then
        touch "$DOWN_STARTED"
        while [ ! -f "$DOWN_RELEASE" ]; do sleep .1; done
        touch "$DOWN_FINISHED"
    fi
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
export REMOVED="$tmp/removed"

# Leased services bind to dynamic loopback ports; manually started services keep fixed ports.
(cd "$root" && TEST_SERVICE_LEASE=agentsview-test-123 TEST_PG_PUBLISH=127.0.0.1::5432 TEST_CH_HTTP_PUBLISH=127.0.0.1::8123 TEST_CH_NATIVE_PUBLISH=127.0.0.1::9000 "$docker_bin" compose -p agentsview-test-123 -f docker-compose.test.yml -f docker-compose.test-lease.yml config --format json) > "$tmp/leased-compose.json"
python3 - "$tmp/leased-compose.json" <<'PY'
import json, sys
with open(sys.argv[1]) as compose:
    services = json.load(compose)["services"]
for name in ("postgres", "clickhouse"):
    assert services[name]["labels"]["test-service.lease"] == "agentsview-test-123"
expected = {"postgres": {5432}, "clickhouse": {8123, 9000}}
for name, targets in expected.items():
    ports = services[name]["ports"]
    assert {port["target"] for port in ports} == targets
    for port in ports:
        assert port["host_ip"] == "127.0.0.1"
        assert port.get("published") in (None, "")
PY
(cd "$root" && TEST_PG_PUBLISH= TEST_CH_HTTP_PUBLISH= TEST_CH_NATIVE_PUBLISH= "$docker_bin" compose -f docker-compose.test.yml config --format json) > "$tmp/manual-compose.json"
python3 - "$tmp/manual-compose.json" <<'PY'
import json, sys
with open(sys.argv[1]) as compose:
    services = json.load(compose)["services"]
expected = {"postgres": {(5432, "5433")}, "clickhouse": {(8123, "18123"), (9000, "19000")}}
for name, ports in expected.items():
    actual = {(port["target"], str(port["published"])) for port in services[name]["ports"]}
    assert actual == ports, (name, actual)
PY

# Cleanup waits for a descendant to release the lease.
export READY="$tmp/ready" RELEASE="$tmp/release" EXITED="$tmp/exited" MAIN="$tmp/main" CHECK_LEASE="$tmp/project" DESCENDANT="$tmp/descendant"
export SHUTDOWN_RELEASE="$shutdown_release" SHUTDOWN_STARTED="$tmp/shutdown-started" SHUTDOWN_PID="$tmp/shutdown-pid" SHUTDOWN_HELPER="$tmp/shutdown-helper"
cat > "$SHUTDOWN_HELPER" <<'HELPER'
#!/bin/sh
printf '%s\n' "$$" > "$SHUTDOWN_PID"
touch "$SHUTDOWN_STARTED"
while [ ! -f "$SHUTDOWN_RELEASE" ]; do sleep 0.1; done
touch "$EXITED"
HELPER
chmod +x "$SHUTDOWN_HELPER"
cat > "$DESCENDANT" <<'CHILD'
#!/bin/sh
trap 'exec "$SHUTDOWN_HELPER"' TERM
touch "$READY"
while [ ! -f "$RELEASE" ]; do sleep 0.1; done
touch "$EXITED"
CHILD
chmod +x "$DESCENDANT"
: > "$DOCKER_LOG"
(cd "$root" && TEST_PG_PUBLISH=invalid TEST_CH_HTTP_PUBLISH=invalid TEST_CH_NATIVE_PUBLISH=invalid ./scripts/test-lease.sh ./scripts/test-service-compose.sh postgres 5432 sh -c 'echo $$ > "$MAIN"; "$DESCENDANT" >/dev/null 2>&1 & while [ ! -f "$READY" ]; do sleep 0.1; done; exit 7') &
first=$!
for _ in {1..100}; do [[ -f "$READY" ]] && break; sleep .1; done
[[ -f "$READY" ]]
main_pid=$(cat "$MAIN")
for _ in {1..100}; do if ! kill -0 "$main_pid" 2>/dev/null; then break; fi; sleep .1; done
if kill -0 "$main_pid" 2>/dev/null; then echo 'foreground command did not exit' >&2; exit 1; fi
lease="$HOME/.local/state/test-service-leases/$(cat "$CHECK_LEASE")"
if grep -q ' down ' "$DOCKER_LOG"; then echo 'cleanup ran while descendant held lease' >&2; exit 1; fi
supervisor=${lease##*-}
supervisor_args=$(ps -p "$supervisor" -o args=)
[[ "$supervisor_args" == *'python3 -'* ]]
kill -TERM "$supervisor"
for _ in {1..100}; do [[ -f "$SHUTDOWN_STARTED" ]] && break; sleep .1; done
[[ -f "$SHUTDOWN_STARTED" ]]
helper_pid=$(cat "$SHUTDOWN_PID")
for _ in {1..10}; do
    if ! kill -0 "$helper_pid" 2>/dev/null; then echo 'shutdown helper received a repeated signal' >&2; exit 1; fi
    sleep .1
done
touch "$SHUTDOWN_RELEASE"
for _ in {1..100}; do [[ -f "$EXITED" ]] && break; sleep .1; done
[[ -f "$EXITED" ]]
set +e
wait "$first"
status=$?
set -e
first=
[[ $status == 143 ]]
grep -qE -- "-p $(cat "$CHECK_LEASE") .* down -v --remove-orphans" "$DOCKER_LOG"
[[ ! -e "$lease" ]]
unset READY RELEASE EXITED MAIN SHUTDOWN_RELEASE SHUTDOWN_STARTED SHUTDOWN_PID SHUTDOWN_HELPER

# A successful command reports cleanup failure; a failed command keeps its status.
for command_status in 0 7; do
    : > "$DOCKER_LOG"
    set +e
    (cd "$root" && DOWN_STATUS=1 ./scripts/test-lease.sh ./scripts/test-service-compose.sh postgres 5432 sh -c "exit $command_status") > "$tmp/cleanup-error" 2>&1
    status=$?
    set -e
    if [[ $command_status == 0 ]]; then [[ $status == 1 ]]; else [[ $status == 7 ]]; fi
    project=$(cat "$CHECK_LEASE")
    grep -Fxq "cleanup failed; Compose project $project may still be running" "$tmp/cleanup-error"
    grep -q ' down ' "$DOCKER_LOG"
    [[ -f "$HOME/.local/state/test-service-leases/$(cat "$CHECK_LEASE")" ]]
done

# A sidecar that survives Compose cleanup keeps the lease if removal fails.
set +e
(cd "$root" && LEFTOVER=1 REMOVE_STATUS=1 ./scripts/test-lease.sh ./scripts/test-service-compose.sh clickhouse 9000 true) > "$tmp/sidecar-error" 2>&1
status=$?
set -e
[[ $status == 1 ]]
project=$(cat "$CHECK_LEASE")
[[ -f "$HOME/.local/state/test-service-leases/$project" ]]
grep -q 'cleanup failed' "$tmp/sidecar-error"
LEFTOVER=1 "$root/scripts/test-lease.sh" true
[[ -f "$REMOVED" ]]

# Cancellation during cleanup waits for Compose, which holds the lease.
export DOWN_STARTED="$tmp/down-started" DOWN_RELEASE="$tmp/down-release" DOWN_FINISHED="$tmp/down-finished"
: > "$DOCKER_LOG"
BLOCK_DOWN=1 python3 - "$root" <<'PY'
import fcntl
import os
from pathlib import Path
import signal
import subprocess
import sys
import time

root = Path(sys.argv[1])
env = os.environ.copy()
process = subprocess.Popen(
    ["./scripts/test-lease.sh", "./scripts/test-service-compose.sh", "postgres", "5432", "true"],
    cwd=root,
    env=env,
    start_new_session=True,
)
started = Path(env["DOWN_STARTED"])
release = Path(env["DOWN_RELEASE"])
finished = Path(env["DOWN_FINISHED"])
try:
    deadline = time.monotonic() + 10
    while not started.exists() and time.monotonic() < deadline:
        time.sleep(0.01)
    assert started.exists(), "cleanup did not start"
    lease = Path(env["HOME"]) / ".local/state/test-service-leases" / Path(env["CHECK_LEASE"]).read_text().strip()
    with lease.open("r+") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            pass
        else:
            raise AssertionError("cleanup did not hold the lease")
    args = subprocess.run(
        ["ps", "-p", str(process.pid), "-o", "args="],
        check=True,
        capture_output=True,
        text=True,
    ).stdout
    assert "python3 - ./scripts/test-service-compose.sh" in args, args
    os.killpg(process.pid, signal.SIGTERM)
    time.sleep(0.2)
    assert process.poll() is None, "cleanup exited before it was released"
    assert not finished.exists(), "cleanup ignored the release gate"
    release.touch()
    assert process.wait(timeout=10) == 143
    assert finished.exists(), "Compose did not finish cleanup"
    assert not lease.exists(), "successful cleanup kept the lease file"
finally:
    release.touch()
    if process.poll() is None:
        process.terminate()
        process.wait(timeout=10)
PY
unset DOWN_STARTED DOWN_RELEASE DOWN_FINISHED

# Termination during startup and while the command runs cleans up the project.
export READY="$tmp/ready"
for phase in startup command; do
    : > "$DOCKER_LOG"
    [[ ! -e "$tmp/ready" ]] || find "$tmp/ready" -delete
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
    lease="$HOME/.local/state/test-service-leases/$(cat "$CHECK_LEASE")"
    [[ ! -e "$lease" ]]
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
$(if $(filter 1,$(MAKELEVEL)),$(info LEASE_FROM_MAKE=$(TEST_SERVICE_LEASE))$(info PG_FROM_MAKE=$(TEST_PG_URL))$(info CH_FROM_MAKE=$(TEST_CLICKHOUSE_URL))$(info STANDBY_FROM_MAKE=$(TEST_PG_STANDBY_URL))$(info NO_EXCHANGE_FROM_MAKE=$(TEST_CLICKHOUSE_NO_EXCHANGE_URL)))
MAKE
for target in test-postgres bench-pg-usage test-clickhouse; do
    : > "$DOCKER_LOG"
    (cd "$root" && MAKELEVEL=0 MAKEFILES="$tmp/inspect.mk" make -o pricing-snapshot "$target" TEST_SERVICE_LEASE=old TEST_PG_URL=old-pg TEST_CLICKHOUSE_URL=old-ch TEST_PG_STANDBY_URL=old-standby TEST_CLICKHOUSE_NO_EXCHANGE_URL=old-no-exchange MAKE='make -n') > "$tmp/make-url"
    project=$(cat "$CHECK_LEASE")
    [[ "$project" != old ]]
    grep -Fxq "LEASE_FROM_MAKE=$project" "$tmp/make-url"
    grep -Fxq 'NO_EXCHANGE_FROM_MAKE=old-no-exchange' "$tmp/make-url"
    case "$target" in
        test-clickhouse)
            service=clickhouse port=9000
            grep -Fq 'go test -tags "fts5,chtest" -v ./internal/clickhouse/...' "$tmp/make-url"
            grep -Fxq 'STANDBY_FROM_MAKE=' "$tmp/make-url"
            grep -Fxq 'PG_FROM_MAKE=' "$tmp/make-url"
            grep -Fxq 'CH_FROM_MAKE=clickhouse://127.0.0.1:49153/default' "$tmp/make-url"
            ;;
        test-postgres|bench-pg-usage)
            service=postgres port=5432
            if [[ $target == bench-pg-usage ]]; then
                grep -Fq -- "-bench 'BenchmarkPGUsage(Read|Refresh)'" "$tmp/make-url"
            else
                grep -Fq 'go test -p 1 -tags "fts5,pgtest" -v ./internal/postgres/...' "$tmp/make-url"
            fi
            grep -Fxq 'STANDBY_FROM_MAKE=' "$tmp/make-url"
            grep -Fxq 'CH_FROM_MAKE=' "$tmp/make-url"
            grep -Fxq 'PG_FROM_MAKE=postgres://agentsview_test:agentsview_test_password@127.0.0.1:49153/agentsview_test?sslmode=disable' "$tmp/make-url"
            ;;
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

# A loose lease directory is rejected before Docker starts.
chmod 755 "$HOME/.local/state/test-service-leases"
: > "$DOCKER_LOG"
set +e
"$root/scripts/test-lease.sh" docker info >"$tmp/mode-error" 2>&1
status=$?
set -e
[[ $status != 0 && ! -s "$DOCKER_LOG" ]]
grep -Fq "lease directory $HOME/.local/state/test-service-leases must be owned by the current user and grant no permissions to group or other users" "$tmp/mode-error"
