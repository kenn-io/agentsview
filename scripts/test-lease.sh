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
import fcntl
import os
import signal
import subprocess
import sys
import time

if not os.environ.get("HOME"):
    sys.exit("HOME must be set to create a test service lease")

command = sys.argv[1:]
lease = f"agentsview-{int(time.time())}-{os.getpid()}"
directory = os.path.join(os.environ["HOME"], ".local", "state", "test-service-leases")
os.makedirs(directory, mode=0o700, exist_ok=True)
info = os.stat(directory)
if info.st_uid != os.getuid() or info.st_mode & 0o077:
    sys.exit("test service lease directory must be owned by the current user and private")
path = os.path.join(directory, lease)
fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_RDWR, 0o600)
os.set_inheritable(fd, True)
fcntl.flock(fd, fcntl.LOCK_EX)
os.environ["TEST_SERVICE_LEASE"] = lease
child = None
received = None

def forward(signum, frame):
    global received
    received = signum
    if child is not None:
        try:
            os.killpg(child.pid, signum)
        except ProcessLookupError:
            pass

for sig in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
    signal.signal(sig, forward)
child = subprocess.Popen(command, pass_fds=(fd,), start_new_session=True)
if received is not None:
    forward(received, None)
status = child.wait()
child = None
received = None
if status < 0:
    status = 128 - status
os.close(fd)
try:
    lock = open(path, "r")
except FileNotFoundError:
    sys.exit(status)
with lock:
    while True:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            break
        except BlockingIOError:
            if received is not None:
                sys.exit(128 + received)
            time.sleep(0.1)
    try:
        same_file = os.stat(path).st_ino == os.fstat(lock.fileno()).st_ino
    except FileNotFoundError:
        same_file = False
    if same_file and subprocess.call(["docker", "compose", "-p", lease, "-f", "docker-compose.test.yml", "-f", "docker-compose.test-lease.yml", "down", "-v", "--remove-orphans"]) != 0:
        print("cleanup failed; scheduled cleanup will retry", file=sys.stderr)
        if status == 0:
            status = 1
sys.exit(status)
PY
