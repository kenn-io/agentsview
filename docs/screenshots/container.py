"""Run one Docker container and remove it when its worker is interrupted."""

import re
import signal
import subprocess


def run(command, cidfile, log_path):
    def interrupt(_signum, _frame):
        raise KeyboardInterrupt

    previous = signal.signal(signal.SIGTERM, interrupt)
    try:
        with log_path.open("wb") as log:
            subprocess.run(command[:2] + ["--pull=never", "--cidfile", str(cidfile)] + command[2:],
                           stdout=log, stderr=subprocess.STDOUT, check=True)
    finally:
        try:
            if cidfile.exists():
                container_id = cidfile.read_text().strip()
                if re.fullmatch(r"[0-9a-f]{64}", container_id):
                    subprocess.run(["docker", "rm", "--force", container_id],
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                   check=False)
        finally:
            signal.signal(signal.SIGTERM, previous)
