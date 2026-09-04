#!/bin/bash
# Runs a Go test binary on behalf of `go test -exec`, holding the
# binary's stderr back until it exits.
#
# `go test` hands the test binary one merged stdout+stderr pipe. The
# testing package prints a benchmark's name ("BenchmarkFoo-4 \t")
# before the timed loop runs and the numbers after it, so any log line
# the code under test writes to stderr in between lands in the middle
# of the result line. benchfmt cannot parse either half, the sample is
# lost, and cmd/benchgate fails the gate on the corrupted capture.
#
# Diverting stderr to a file and replaying it after the binary exits
# keeps every result line intact. Logs, panics, and stack traces still
# reach the captured output, just after the package's results instead
# of inside them. The binary's exit status is preserved.
set -u

if [ "$#" -lt 1 ]; then
    echo "usage: bench-gate-exec.sh <test-binary> [args...]" >&2
    exit 2
fi

stderr_file="$(mktemp)" || exit 2
trap 'rm -f "$stderr_file"' EXIT

"$@" 2>"$stderr_file"
status=$?
cat "$stderr_file" >&2
exit "$status"
