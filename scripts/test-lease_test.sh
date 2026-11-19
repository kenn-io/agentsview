#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
launcher=
cleanup() {
    touch "$tmp/release" "$tmp/release-up"
    if [[ -n "$launcher" ]] && kill -0 "$launcher" 2>/dev/null; then
        touch "$tmp/release"
        wait "$launcher" 2>/dev/null || true
    fi
    find "$tmp" -depth -delete
}
trap cleanup EXIT
go_bin="$(go env GOROOT)/bin/go"
docker_bin=$(command -v docker)
python_bin=$(command -v python3)
if command -v mise >/dev/null 2>&1 && [[ "$python_bin" == */mise/shims/* ]]; then python_bin=$(mise which python3); fi
export HOME="$tmp/home"
mkdir -p "$HOME" "$tmp/bin" "$tmp/module"
cat > "$tmp/bin/docker" <<'DOCKER'
#!/bin/sh
printf '%s\n' "$*" >> "$DOCKER_LOG"
case " $* " in
  *' up '*)
    if [ -n "${CHECK_LEASE:-}" ]; then
      [ "$2" = -p ] && [ "$3" = "$TEST_SERVICE_LEASE" ] || exit 1
      python3 - "$HOME/.local/state/test-service-leases/$TEST_SERVICE_LEASE" <<'PROBE' || exit 1
import fcntl, sys
with open(sys.argv[1]) as f:
    try: fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError: sys.exit(0)
sys.exit(1)
PROBE
      printf '%s\n' "$TEST_SERVICE_LEASE" > "$CHECK_LEASE"
    fi
    if [ -n "${WAIT_UP:-}" ]; then touch "$WAIT_UP/started"; while [ ! -f "$WAIT_UP/release-up" ]; do sleep .1; done; fi ;;
  *' port '*) echo 127.0.0.1:49153 ;;
  *' down '*)
    if [ -n "${CHECK_CLEANUP:-}" ]; then
      python3 - "$HOME/.local/state/test-service-leases/$TEST_SERVICE_LEASE" <<'PROBE' || exit 1
import fcntl, sys
with open(sys.argv[1]) as f:
    try: fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError: sys.exit(0)
sys.exit(1)
PROBE
      echo locked >> "$CHECK_CLEANUP"
    fi
    exit "${DOWN_STATUS:-0}" ;;
esac
DOCKER
chmod +x "$tmp/bin/docker"
ln -s "$python_bin" "$tmp/bin/python3"
ln -s "$go_bin" "$tmp/bin/go"
export PATH="$tmp/bin:$PATH" DOCKER_LOG="$tmp/docker.log"
cleanup_cmd='docker compose -p "$TEST_SERVICE_LEASE" -f docker-compose.test.yml -f docker-compose.test-lease.yml down -v --remove-orphans'

# Both services expose the lease label in the resolved Compose configuration.
(cd "$root" && TEST_SERVICE_LEASE=agentsview-test-123 "$docker_bin" compose -p agentsview-test-123 -f docker-compose.test.yml -f docker-compose.test-lease.yml config --format json) | python3 -c '
import json, sys
services = json.load(sys.stdin)["services"]
for name in ("postgres", "clickhouse"):
    assert services[name]["labels"]["test-service.lease"] == "agentsview-test-123"
'

# An existing lease directory is secured before the test starts.
mkdir -p "$HOME/.local/state/test-service-leases"
chmod 777 "$HOME/.local/state/test-service-leases"

# A real test binary retains the lease after its launcher dies.
cat > "$tmp/module/go.mod" <<'MOD'
module example.com/lease-test

go 1.21
MOD
cat > "$tmp/module/lease_test.go" <<'GO'
package lease_test
import (
    "os"
    "testing"
    "time"
)
func TestLease(t *testing.T) {
    os.WriteFile(os.Getenv("READY"), []byte("ready"), 0600)
    for { if _, err := os.Stat(os.Getenv("RELEASE")); err == nil { return }; time.Sleep(10 * time.Millisecond) }
}
GO
export READY="$tmp/ready" RELEASE="$tmp/release"
(cd "$tmp/module" && exec "$root/scripts/test-lease.sh" agentsview "$cleanup_cmd" go test ./...) >"$tmp/go.log" 2>&1 &
launcher=$!
for _ in {1..600}; do [[ -f "$READY" ]] && break; sleep .1; done
[[ -f "$READY" ]] || { cat "$tmp/go.log"; exit 1; }
python3 - "$HOME/.local/state/test-service-leases" <<'PYMODE'
import os, stat, sys
assert stat.S_IMODE(os.stat(sys.argv[1]).st_mode) == 0o700
PYMODE
lease=$(find "$HOME/.local/state/test-service-leases" -type f -print -quit)
kill -9 "$launcher"
wait "$launcher" 2>/dev/null || true
launcher=
probe() {
    python3 - "$lease" <<'PY'
import fcntl, sys
with open(sys.argv[1]) as f:
    try: fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError: sys.exit(1)
PY
}
if probe; then echo 'lease released while test still runs' >&2; exit 1; fi
touch "$RELEASE"
for _ in {1..100}; do if probe; then break; fi; sleep .1; done
probe

# A descendant retaining the lease keeps its service alive after the command exits.
: > "$DOCKER_LOG"
export DESC_READY="$tmp/desc-ready" DESC_RELEASE="$tmp/desc-release" CHECK_LEASE="$tmp/desc-project"
(cd "$root" && ./scripts/test-lease.sh agentsview "$cleanup_cmd" ./scripts/test-service-compose.sh postgres 5432 sh -c 'sh -c '\''touch "$DESC_READY"; while [ ! -f "$DESC_RELEASE" ]; do sleep 0.1; done'\'' & while [ ! -f "$DESC_READY" ]; do sleep 0.1; done')
if grep -q ' down ' "$DOCKER_LOG"; then
    touch "$DESC_RELEASE"
    echo 'cleanup ran while a descendant held the lease' >&2
    exit 1
fi
touch "$DESC_RELEASE"
lease="$HOME/.local/state/test-service-leases/$(cat "$tmp/desc-project")"
for _ in {1..100}; do if probe; then break; fi; sleep .1; done
probe
unset CHECK_LEASE

# The compose runner preserves status, labels the project, and reports cleanup failure.
export TEST_SERVICE_LEASE=agentsview-test-123 DOWN_STATUS=1 CHECK_CLEANUP="$tmp/cleanup-lock"
set +e
(cd "$root" && ./scripts/test-lease.sh agentsview "$cleanup_cmd" ./scripts/test-service-compose.sh postgres 5432 sh -c 'test "$TEST_PG_URL" = "postgres://agentsview_test:agentsview_test_password@127.0.0.1:49153/agentsview_test?sslmode=disable" || exit 8; exit 7') 2>"$tmp/error"
status=$?
set -e
[[ $status == 7 ]]
grep -q 'cleanup failed; retry from the repository root:' "$tmp/error"
grep -qE -- '-p agentsview-.* up -d --wait postgres' "$DOCKER_LOG"
grep -qE -- '-p agentsview-.* down -v --remove-orphans' "$DOCKER_LOG"
[[ -f "$lease" ]]

# Cleanup failure fails a successful test.
set +e
(cd "$root" && ./scripts/test-lease.sh agentsview "$cleanup_cmd" ./scripts/test-service-compose.sh postgres 5432 true) 2>"$tmp/cleanup-error"
status=$?
set -e
[[ $status == 1 ]]
grep -q 'cleanup failed; retry from the repository root:' "$tmp/cleanup-error"

# Dry runs do not start services; manual targets retain their project.
export DOWN_STATUS=0
: > "$DOCKER_LOG"
: > "$CHECK_CLEANUP"
(cd "$root" && ./scripts/test-lease.sh agentsview "$cleanup_cmd" true)
[[ $(wc -l < "$CHECK_CLEANUP") == 1 ]]
: > "$DOCKER_LOG"
(cd "$root" && make -n test-postgres test-clickhouse bench-pg-usage) > "$tmp/make"
[[ ! -s "$DOCKER_LOG" ]]
(cd "$root" && make -n postgres-up clickhouse-up) > "$tmp/manual"
if grep -qE 'test-lease| -p ' "$tmp/manual"; then exit 1; fi

# A command-line lease cannot override the new lease in the nested Make.
printf '%s\n' '$(info LEASE_FROM_MAKE=$(TEST_SERVICE_LEASE))' > "$tmp/inspect.mk"
(cd "$root" && CHECK_LEASE="$tmp/project" MAKEFILES="$tmp/inspect.mk" make test-postgres TEST_SERVICE_LEASE=old MAKE='make -n') > "$tmp/make-lease"
project=$(cat "$tmp/project")
[[ "$project" != old ]]
grep -Fxq "LEASE_FROM_MAKE=$project" "$tmp/make-lease"

# Each target starts its leased service and passes its address to nested Make.
cat > "$tmp/inspect.mk" <<'MAKE'
$(if $(filter 1,$(MAKELEVEL)),$(info PG_FROM_MAKE=$(TEST_PG_URL))$(info CH_FROM_MAKE=$(TEST_CLICKHOUSE_URL)))
MAKE
for target in test-postgres bench-pg-usage test-clickhouse; do
    : > "$DOCKER_LOG"
    (cd "$root" && CHECK_LEASE="$tmp/project" MAKEFILES="$tmp/inspect.mk" make "$target" TEST_PG_URL=old-pg TEST_CLICKHOUSE_URL=old-ch MAKE='make -n') > "$tmp/make-url"
    project=$(cat "$tmp/project")
    case "$target" in
        test-clickhouse)
            service=clickhouse port=9000
            grep -Fxq 'CH_FROM_MAKE=clickhouse://127.0.0.1:49153/default' "$tmp/make-url" ;;
        *)
            service=postgres port=5432
            grep -Fxq 'PG_FROM_MAKE=postgres://agentsview_test:agentsview_test_password@127.0.0.1:49153/agentsview_test?sslmode=disable' "$tmp/make-url" ;;
    esac
    grep -qE -- "-p $project .* up -d --wait $service" "$DOCKER_LOG"
    grep -qE -- "-p $project .* port $service $port" "$DOCKER_LOG"
    grep -qE -- "-p $project .* down -v --remove-orphans" "$DOCKER_LOG"
done

# Signals reach the command and cleanup runs after it releases the lease.
for sig in HUP INT TERM; do
    : > "$DOCKER_LOG"
    ready="$tmp/signal-ready-$sig"
    (cd "$root" && exec ./scripts/test-lease.sh agentsview "$cleanup_cmd" python3 -c 'import os, signal, sys, time; signal.signal(getattr(signal, "SIG" + sys.argv[1]), lambda *_: sys.exit(128 + getattr(signal, "SIG" + sys.argv[1]))); open(sys.argv[2], "w").close(); signal.pause()' "$sig" "$ready") >"$tmp/signal.log" 2>&1 &
    launcher=$!
    for _ in {1..100}; do [[ -f "$ready" ]] && break; sleep .1; done
    [[ -f "$ready" ]]
    kill -s "$sig" "$launcher"
    set +e
    wait "$launcher"
    status=$?
    set -e
    launcher=
    case "$sig" in HUP) expected=129 ;; INT) expected=130 ;; TERM) expected=143 ;; esac
    [[ $status == "$expected" ]]
    grep -qE -- '-p agentsview-.* down -v --remove-orphans' "$DOCKER_LOG"
done

# Two overlapping runs select different projects and clean up their own project.
export TEST_SERVICE_LEASE=agentsview-parent-123
: > "$DOCKER_LOG"
(cd "$root" && ./scripts/test-lease.sh agentsview "$cleanup_cmd" ./scripts/test-service-compose.sh postgres 5432 sh -c 'sleep 1') &
first=$!
(cd "$root" && ./scripts/test-lease.sh agentsview "$cleanup_cmd" ./scripts/test-service-compose.sh postgres 5432 sh -c 'sleep 1') &
second=$!
wait "$first"
wait "$second"
projects=$(awk '$1 == "compose" && $2 == "-p" && $8 == "up" {print $3}' "$DOCKER_LOG" | sort -u)
[[ $(printf '%s\n' "$projects" | wc -l) == 2 ]]
[[ "$projects" != *agentsview-parent-123* ]]
while IFS= read -r project; do
    grep -qE -- "-p $project .* down -v --remove-orphans" "$DOCKER_LOG"
done <<< "$projects"

# The lease may disappear before the supervisor can reopen it.
for expected in 0 7; do
    : > "$DOCKER_LOG"
    set +e
    (cd "$root" && ./scripts/test-lease.sh agentsview "$cleanup_cmd" sh -c 'python3 -c '\''import os; os.unlink(os.path.join(os.environ["HOME"], ".local/state/test-service-leases", os.environ["TEST_SERVICE_LEASE"]))'\''; exit "$1"' sh "$expected") >"$tmp/missing-lease.log" 2>&1
    status=$?
    set -e
    [[ $status == "$expected" ]]
    [[ ! -s "$DOCKER_LOG" ]]
done

# A missing Python interpreter stops before Docker can start anything.
mkdir "$tmp/no-python"
ln -s "$tmp/bin/docker" "$tmp/no-python/docker"
: > "$DOCKER_LOG"
set +e
PATH="$tmp/no-python" /bin/sh "$root/scripts/test-lease.sh" agentsview docker info >"$tmp/no-python.out" 2>&1
status=$?
set -e
[[ $status == 2 && ! -s "$DOCKER_LOG" ]]
grep -qE 'python3 is required' "$tmp/no-python.out"

# An absent home or unavailable locking support stops before Docker starts.
unset TEST_SERVICE_LEASE
for home_mode in unset empty; do
    : > "$DOCKER_LOG"
    set +e
    if [[ "$home_mode" == unset ]]; then
        env -u HOME "$root/scripts/test-lease.sh" agentsview docker info >"$tmp/home-error" 2>&1
    else
        HOME= "$root/scripts/test-lease.sh" agentsview docker info >"$tmp/home-error" 2>&1
    fi
    status=$?
    set -e
    [[ $status != 0 && ! -s "$DOCKER_LOG" ]]
    grep -qE 'HOME must be set' "$tmp/home-error"
done
mkdir "$tmp/no-fcntl"
cat > "$tmp/no-fcntl/python3" <<'PYTHON'
#!/bin/sh
exec "$REAL_PYTHON" -c '
import builtins, sys
original = builtins.__import__
def no_fcntl(name, *args, **kwargs):
    if name == "fcntl": raise ImportError("unavailable")
    return original(name, *args, **kwargs)
builtins.__import__ = no_fcntl
sys.argv = sys.argv[1:]
exec(sys.stdin.read())
' "$@"
PYTHON
chmod +x "$tmp/no-fcntl/python3"
: > "$DOCKER_LOG"
set +e
REAL_PYTHON="$python_bin" PATH="$tmp/no-fcntl:$PATH" "$root/scripts/test-lease.sh" agentsview docker info >"$tmp/fcntl-error" 2>&1
status=$?
set -e
[[ $status != 0 && ! -s "$DOCKER_LOG" ]]
grep -qE 'platform with Python fcntl support' "$tmp/fcntl-error"
