#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

export ARGUMENT_FILE="$TMP_DIR/arguments"

# A stand-in for a Go test binary that logs to stderr while the
# testing package is midway through printing a benchmark result line.
cat > "$TMP_DIR/fake.test" <<'EOF'
#!/bin/bash
printf '<%s>\n' "$@" > "$ARGUMENT_FILE"
printf 'goos: linux\npkg: example.com/x\n'
printf 'BenchmarkFoo-4 \t'
echo "2026/09/04 12:00:00 db: InsertMessages (200 msgs): 149ms" >&2
printf '      20\t 100 ns/op\t 10 B/op\t 1 allocs/op\n'
printf 'PASS\n'
exit "${FAKE_EXIT:-0}"
EOF
chmod +x "$TMP_DIR/fake.test"

fail() {
    echo "FAIL: $1" >&2
    echo "--- output ---" >&2
    printf '%s\n' "$2" >&2
    exit 1
}

# Result lines stay intact when stderr and stdout share one pipe, the
# way go test wires the binary.
set +e
output="$(bash "$SCRIPT_DIR/bench-gate-exec.sh" "$TMP_DIR/fake.test" \
    -test.bench . -test.benchmem 2>&1)"
status=$?
set -e
[ "$status" -eq 0 ] || fail "expected exit 0, got $status" "$output"

expected_result=$'BenchmarkFoo-4 \t      20\t 100 ns/op\t 10 B/op\t 1 allocs/op'
printf '%s\n' "$output" | grep -qxF "$expected_result" \
    || fail "benchmark result line was split or altered" "$output"

printf '%s\n' "$output" | grep -qF "db: InsertMessages (200 msgs): 149ms" \
    || fail "stderr log line was dropped" "$output"

pass_line=$(printf '%s\n' "$output" | grep -nxF "PASS" | cut -d: -f1)
log_line=$(printf '%s\n' "$output" | grep -nF "db: InsertMessages" | cut -d: -f1)
[ "$log_line" -gt "$pass_line" ] \
    || fail "stderr should be replayed after the binary's stdout" "$output"

expected_args=$'<-test.bench>\n<.>\n<-test.benchmem>'
[ "$(cat "$ARGUMENT_FILE")" = "$expected_args" ] \
    || fail "arguments were not forwarded verbatim" "$(cat "$ARGUMENT_FILE")"

# The binary's exit status is what go test sees.
set +e
output="$(FAKE_EXIT=3 bash "$SCRIPT_DIR/bench-gate-exec.sh" \
    "$TMP_DIR/fake.test" 2>&1)"
status=$?
set -e
[ "$status" -eq 3 ] || fail "expected exit 3 to propagate, got $status" "$output"
printf '%s\n' "$output" | grep -qF "db: InsertMessages" \
    || fail "stderr should be replayed even when the binary fails" "$output"

# Missing binary argument is a usage error.
set +e
output="$(bash "$SCRIPT_DIR/bench-gate-exec.sh" 2>&1)"
status=$?
set -e
[ "$status" -eq 2 ] || fail "expected usage exit 2, got $status" "$output"
printf '%s\n' "$output" | grep -qF "usage:" \
    || fail "usage message missing" "$output"

echo "bench-gate-exec_test.sh: all tests passed"
