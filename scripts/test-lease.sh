#!/bin/sh
# The lease name is the Compose project and the test-service.lease label.
# The inheritable flock stays held until the last test process exits.
# Cleanup holds the lease while removing the project.
set -eu

if ! command -v python3 >/dev/null 2>&1; then
    echo 'python3 is required; use postgres-up or clickhouse-up for a manual service' >&2
    exit 2
fi
exec python3 - "$@" <<'PY'
import os
import signal
import subprocess
import sys
import time

if os.name == "nt":
    sys.exit("native Windows is unsupported; run leased test targets from WSL")

import fcntl

command = sys.argv[1:]
lease = f"agentsview-{int(time.time())}-{os.getpid()}"
directory = os.path.join(os.environ["HOME"], ".local", "state", "test-service-leases")
os.makedirs(directory, mode=0o700, exist_ok=True)
info = os.stat(directory)
if info.st_uid != os.getuid() or info.st_mode & 0o077:
    sys.exit(f"lease directory {directory} must be owned by the current user and grant no permissions to group or other users")
path = os.path.join(directory, lease)
fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_RDWR, 0o600)
os.set_inheritable(fd, True)
fcntl.flock(fd, fcntl.LOCK_EX)
os.environ["TEST_SERVICE_LEASE"] = lease
process_group = None
received = None
pending = False

def forward(signum, frame):
    global received, pending
    received, pending = signum, process_group is None
    if not pending:
        try:
            os.killpg(process_group, signum)
        except ProcessLookupError:
            pass

for sig in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
    signal.signal(sig, forward)
child = subprocess.Popen(command, pass_fds=(fd,), start_new_session=True)
process_group = child.pid
if pending:
    forward(received, None)
status = child.wait()
if status < 0:
    status = 128 - status
os.close(fd)
try:
    lock = open(path, "r+")
except FileNotFoundError:
    sys.exit(128 + received if received is not None else status)
with lock:
    fcntl.flock(lock, fcntl.LOCK_EX)
    process_group = None
    try:
        same_file = os.stat(path).st_ino == os.fstat(lock.fileno()).st_ino
    except FileNotFoundError:
        same_file = False
    if same_file:
        cleanup = ["docker", "compose", "-p", lease, "-f", "docker-compose.test.yml", "-f", "docker-compose.test-lease.yml", "down", "-v", "--remove-orphans"]
        cleanup_vars = {
            "TEST_SERVICE_LEASE": lease,
            "TEST_PG_PUBLISH": "127.0.0.1::5432",
            "TEST_CH_HTTP_PUBLISH": "127.0.0.1::8123",
            "TEST_CH_NATIVE_PUBLISH": "127.0.0.1::9000",
        }
        cleanup_env = os.environ.copy()
        cleanup_env.update(cleanup_vars)
        failed = subprocess.call(cleanup, env=cleanup_env, start_new_session=True) != 0
        # Testcontainers started outside Compose carry the same lease label.
        listing = ["docker", "ps", "-aq", "--filter", f"label=test-service.lease={lease}"]
        try:
            containers = subprocess.check_output(listing, text=True, start_new_session=True).split()
            if containers:
                subprocess.check_call(["docker", "rm", "-f", "-v", *containers], start_new_session=True)
            failed |= bool(subprocess.check_output(listing, text=True, start_new_session=True).strip())
        except subprocess.CalledProcessError:
            failed = True
        if failed:
            print(f"cleanup failed; Compose project {lease} may still be running", file=sys.stderr)
            if status == 0:
                status = 1
        else:
            os.unlink(path)
if received is not None:
    status = 128 + received
sys.exit(status)
PY
