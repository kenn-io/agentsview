#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
first=
second=
cleanup() {
    touch "$tmp/release"
    [[ -z "$first" ]] || wait "$first" 2>/dev/null || true
    [[ -z "$second" ]] || wait "$second" 2>/dev/null || true
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
  *' down '*)
    python3 - "$HOME/.local/state/test-service-leases/$TEST_SERVICE_LEASE" <<'PROBE' || exit 1
import fcntl, sys
with open(sys.argv[1]) as f:
    try: fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError: sys.exit(0)
sys.exit(1)
PROBE
    exit "${DOWN_STATUS:-0}" ;;
esac
DOCKER
chmod +x "$tmp/bin/docker"
ln -s "$python_bin" "$tmp/bin/python3"
export PATH="$tmp/bin:$PATH" DOCKER_LOG="$tmp/docker.log"
cleanup_cmd='docker compose -p "$TEST_SERVICE_LEASE" -f docker-compose.test.yml -f docker-compose.test-lease.yml down -v --remove-orphans'

# Both services carry the project lease label in the resolved Compose config.
export HEALTH_COMMAND="$tmp/postgres-health-command"
(cd "$root" && TEST_SERVICE_LEASE=agentsview-test-123 "$docker_bin" compose -p agentsview-test-123 -f docker-compose.test.yml -f docker-compose.test-lease.yml config --format json) | python3 -c '
import json, os, sys
services = json.load(sys.stdin)["services"]
for name in ("postgres", "clickhouse"):
    assert services[name]["labels"]["test-service.lease"] == "agentsview-test-123"
kind, command = services["postgres"]["healthcheck"]["test"]
assert kind == "CMD-SHELL"
open(os.environ["HEALTH_COMMAND"], "w").write(command)
'

# A socket-only startup server must not satisfy the PostgreSQL health check.
cat > "$tmp/bin/pg_isready" <<'PGREADY'
#!/bin/sh
case " $* " in
  *' -h 127.0.0.1 '*) [ -e "$TCP_READY" ] ;;
  *) exit 0 ;;
esac
PGREADY
chmod +x "$tmp/bin/pg_isready"
export TCP_READY="$tmp/tcp-ready"
if sh "$HEALTH_COMMAND"; then echo 'socket-only server passed health check' >&2; exit 1; fi
touch "$TCP_READY"
sh "$HEALTH_COMMAND"
find "$tmp/bin/pg_isready" -delete
unset HEALTH_COMMAND TCP_READY

probe() {
    python3 - "$lease" <<'PY'
import fcntl, sys
with open(sys.argv[1]) as f:
    try: fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError: sys.exit(1)
PY
}

# Cleanup waits for a descendant to release the lease and retains the test status.
export READY="$tmp/ready" RELEASE="$tmp/release" EXITED="$tmp/exited" CHECK_LEASE="$tmp/project"
: > "$DOCKER_LOG"
(cd "$root" && ./scripts/test-lease.sh agentsview "$cleanup_cmd" ./scripts/test-service-compose.sh postgres 5432 sh -c 'sh -c '\''touch "$READY"; while [ ! -f "$RELEASE" ]; do sleep 0.1; done; touch "$EXITED"'\'' >/dev/null 2>&1 & while [ ! -f "$READY" ]; do sleep 0.1; done; exit 7') &
first=$!
for _ in {1..100}; do [[ -f "$READY" ]] && break; sleep .1; done
[[ -f "$READY" ]]
lease="$HOME/.local/state/test-service-leases/$(cat "$CHECK_LEASE")"
if probe; then echo 'lease released while descendant still runs' >&2; exit 1; fi
if grep -q ' down ' "$DOCKER_LOG"; then echo 'cleanup ran while descendant held lease' >&2; exit 1; fi
touch "$RELEASE"
set +e
wait "$first"
status=$?
set -e
first=
[[ $status == 7 && -f "$EXITED" ]]
grep -qE -- "-p $(cat "$CHECK_LEASE") .* down -v --remove-orphans" "$DOCKER_LOG"
probe
unset READY RELEASE EXITED

# A cleanup failure turns a successful test into a failure.
set +e
(cd "$root" && DOWN_STATUS=1 ./scripts/test-lease.sh agentsview "$cleanup_cmd" true) > "$tmp/cleanup-error" 2>&1
status=$?
set -e
[[ $status == 1 ]]
grep -q 'cleanup failed; retry from the repository root:' "$tmp/cleanup-error"

# Dry runs do not start services or create leases.
: > "$DOCKER_LOG"
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
    grep -qE -- "-p $project .* down -v --remove-orphans" "$DOCKER_LOG"
done

# Concurrent runs use distinct leases and Compose projects.
export TEST_SERVICE_LEASE=agentsview-parent-123
: > "$DOCKER_LOG"
(cd "$root" && CHECK_LEASE="$tmp/project-first" ./scripts/test-lease.sh agentsview "$cleanup_cmd" ./scripts/test-service-compose.sh postgres 5432 sh -c 'sleep 1') &
first=$!
(cd "$root" && CHECK_LEASE="$tmp/project-second" ./scripts/test-lease.sh agentsview "$cleanup_cmd" ./scripts/test-service-compose.sh postgres 5432 sh -c 'sleep 1') &
second=$!
wait "$first"
first=
wait "$second"
second=
first_project=$(cat "$tmp/project-first")
second_project=$(cat "$tmp/project-second")
[[ "$first_project" != "$second_project" && "$first_project" != "$TEST_SERVICE_LEASE" && "$second_project" != "$TEST_SERVICE_LEASE" ]]

# Missing prerequisites stop before Docker starts.
mkdir "$tmp/no-python"
ln -s "$tmp/bin/docker" "$tmp/no-python/docker"
: > "$DOCKER_LOG"
set +e
PATH="$tmp/no-python" /bin/sh "$root/scripts/test-lease.sh" agentsview "$cleanup_cmd" docker info >"$tmp/no-python.out" 2>&1
status=$?
set -e
[[ $status == 2 && ! -s "$DOCKER_LOG" ]]
grep -q 'python3 is required' "$tmp/no-python.out"
set +e
HOME= "$root/scripts/test-lease.sh" agentsview "$cleanup_cmd" docker info >"$tmp/home-error" 2>&1
status=$?
set -e
[[ $status != 0 && ! -s "$DOCKER_LOG" ]]
grep -q 'HOME must be set' "$tmp/home-error"

# Cancelling the public target reaches an active test and delays cleanup until it exits.
cat > "$tmp/bin/go" <<'GO'
#!/usr/bin/env python3
import os
import signal
import sys
import time

if sys.argv[1] == "env":
    sys.exit(0)

def stop(signum, frame):
    open(os.environ["SIGNAL_SEEN"], "w").close()
    while not os.path.exists(os.environ["SIGNAL_RELEASE"]):
        time.sleep(0.05)
    open(os.environ["SIGNAL_EXITED"], "w").close()
    sys.exit(128 + signum)

signal.signal(signal.SIGTERM, stop)
signal.alarm(20)
ready = os.environ["SIGNAL_READY"]
with open(ready + ".tmp", "w") as f:
    f.write(f'{os.environ["TEST_SERVICE_LEASE"]} {os.getpid()}')
os.replace(ready + ".tmp", ready)
signal.pause()
GO
chmod +x "$tmp/bin/go"
export SIGNAL_READY="$tmp/signal-ready" SIGNAL_SEEN="$tmp/signal-seen" SIGNAL_RELEASE="$tmp/signal-release" SIGNAL_EXITED="$tmp/signal-exited"
: > "$DOCKER_LOG"
python3 - "$root" "$tmp/bin/go" <<'PYTEST'
import os
import signal
import subprocess
import sys
import time

root, fake_go = sys.argv[1:]
ready, seen, release, exited = (os.environ[k] for k in ("SIGNAL_READY", "SIGNAL_SEEN", "SIGNAL_RELEASE", "SIGNAL_EXITED"))
log = os.environ["DOCKER_LOG"]

def wait_for(path):
    deadline = time.monotonic() + 8
    while not os.path.exists(path):
        if time.monotonic() >= deadline:
            raise AssertionError(f"timed out waiting for {os.path.basename(path)}")
        time.sleep(0.05)

def stop_if_owned(pid, expected):
    if not pid:
        return
    result = subprocess.run(["ps", "-p", str(pid), "-o", "args="], capture_output=True, text=True, timeout=5)
    if result.returncode == 0 and expected in result.stdout:
        os.kill(pid, signal.SIGKILL)

output = open(os.path.join(os.path.dirname(log), "signal-output"), "w")
proc = subprocess.Popen(["make", "-o", "pricing-snapshot", "MAKE=make -o ensure-embed-dir", "test-postgres"], cwd=root, stdout=output, stderr=output)
supervisor = descendant = None
try:
    wait_for(ready)
    project, child = open(ready).read().split()
    supervisor = int(project.rsplit("-", 1)[1])
    descendant = int(child)
    args = subprocess.run(["ps", "-p", str(supervisor), "-o", "args="], check=True, capture_output=True, text=True, timeout=5).stdout
    assert "python3 - agentsview" in args
    os.kill(supervisor, signal.SIGTERM)
    wait_for(seen)
    assert " down " not in open(log).read(), "cleanup ran while the descendant held the lease"
    open(release, "w").close()
    assert proc.wait(timeout=10) == 2, "cancelled make returned an unexpected status"
    assert "Error 143" in open(output.name).read(), "the supervisor did not preserve the cancellation status"
    assert os.path.exists(exited), "the descendant did not exit"
    assert f"-p {project} " in open(log).read() and " down " in open(log).read(), "the project was not cleaned up"
finally:
    open(release, "w").close()
    stop_if_owned(descendant, fake_go)
    stop_if_owned(supervisor, "python3 - agentsview")
    if proc.poll() is None:
        proc.kill()
    proc.wait(timeout=10)
    output.close()
PYTEST
unset SIGNAL_READY SIGNAL_SEEN SIGNAL_RELEASE SIGNAL_EXITED

# Three parallel targets share the top-level snapshot preparation.
cat > "$tmp/bin/go" <<'GO'
#!/bin/sh
[ "$1" != env ] || exit 0
printf '%s\n' "$1" >> "$GO_LOG"
GO
chmod +x "$tmp/bin/go"
export GO_LOG="$tmp/go.log"
: > "$DOCKER_LOG"
(cd "$root" && make -j3 -o sqlite-vec-header -o ensure-embed-dir MAKE='make -o sqlite-vec-header -o ensure-embed-dir' test-postgres test-clickhouse bench-pg-usage) > "$tmp/parallel.log" 2>&1 || { cat "$tmp/parallel.log"; exit 1; }
[[ $(grep -c '^run$' "$GO_LOG") == 1 ]]
[[ $(grep -c ' up -d --wait ' "$DOCKER_LOG") == 3 ]]
[[ $(grep -c ' down ' "$DOCKER_LOG") == 3 ]]
