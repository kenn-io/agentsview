#!/bin/sh
# The lease name is the Compose project and the test-service.lease label.
# The inheritable flock stays held until the last test process exits.
# A separate reaper removes the project after the lock is released.
set -eu

if ! command -v python3 >/dev/null 2>&1; then
    echo 'python3 is required; use postgres-up or clickhouse-up for a manual service' >&2
    exit 2
fi
exec python3 - "$@" <<'PY'
import fcntl
import os
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
os.execvp(command[0], command)
PY
