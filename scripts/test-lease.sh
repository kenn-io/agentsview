#!/bin/sh
# The lease name is the Compose project and the test-service.lease label.
# The inheritable flock stays held until the last test process exits.
# Cleanup waits for that lock before removing the project.
set -eu

if ! command -v python3 >/dev/null 2>&1; then
    echo 'python3 is required; use postgres-up or clickhouse-up for a manual service' >&2
    exit 2
fi
exec python3 - "$@" <<'PY'
import fcntl
import os
import signal
import subprocess
import sys
import time

if not os.environ.get("HOME"):
    sys.exit("HOME must be set to create a test service lease")

name, cleanup, *command = sys.argv[1:]
lease = f"{name}-{int(time.time())}-{os.getpid()}"
directory = os.path.join(os.environ["HOME"], ".local", "state", "test-service-leases")
os.makedirs(directory, mode=0o700, exist_ok=True)
path = os.path.join(directory, lease)
fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_RDWR, 0o600)
os.set_inheritable(fd, True)
fcntl.flock(fd, fcntl.LOCK_EX)
os.environ["TEST_SERVICE_LEASE"] = lease
child = subprocess.Popen(command, pass_fds=(fd,), start_new_session=True)
def forward(signum, frame):
    try:
        os.killpg(child.pid, signum)
    except ProcessLookupError:
        pass

for sig in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
    signal.signal(sig, forward)
status = child.wait()
if status < 0:
    status = 128 - status
os.close(fd)
try:
    lock = open(path, "r")
except FileNotFoundError:
    sys.exit(status)
with lock:
    fcntl.flock(lock, fcntl.LOCK_EX)
    if subprocess.call(cleanup, shell=True) != 0:
        print(f"cleanup failed; retry from the repository root: {cleanup.replace('$TEST_SERVICE_LEASE', lease)}", file=sys.stderr)
        if status == 0:
            status = 1
sys.exit(status)
PY
