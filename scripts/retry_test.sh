#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

export ATTEMPT_FILE="$TMP_DIR/attempts"
export ARGUMENT_FILE="$TMP_DIR/arguments"
export SLEEP_FILE="$TMP_DIR/sleeps"

cat > "$TMP_DIR/eventually_succeeds" <<'EOF'
#!/bin/bash
attempt=0
if [ -f "$ATTEMPT_FILE" ]; then
    attempt="$(cat "$ATTEMPT_FILE")"
fi
attempt=$((attempt + 1))
printf '%s\n' "$attempt" > "$ATTEMPT_FILE"
printf '%s\n' "$#" >> "$ARGUMENT_FILE"
printf '<%s>\n' "$@" >> "$ARGUMENT_FILE"
[ "$attempt" -ge 3 ]
EOF

cat > "$TMP_DIR/always_fails" <<'EOF'
#!/bin/bash
attempt=0
if [ -f "$ATTEMPT_FILE" ]; then
    attempt="$(cat "$ATTEMPT_FILE")"
fi
printf '%s\n' "$((attempt + 1))" > "$ATTEMPT_FILE"
exit 17
EOF

cat > "$TMP_DIR/sleep" <<'EOF'
#!/bin/bash
printf '%s\n' "$@" >> "$SLEEP_FILE"
EOF

cat > "$TMP_DIR/invalid_command" <<'EOF'
#!/bin/bash
touch "$INVALID_COMMAND_FILE"
EOF

chmod +x "$TMP_DIR/eventually_succeeds" "$TMP_DIR/always_fails" \
    "$TMP_DIR/invalid_command" "$TMP_DIR/sleep"

export INVALID_COMMAND_FILE="$TMP_DIR/invalid-command-ran"
EXPECTED_USAGE="usage: retry.sh <max-attempts> <base-delay-seconds> <command> [args...]"

assert_invalid() {
    local description="$1"
    shift
    local output status

    rm -f "$INVALID_COMMAND_FILE"
    set +e
    output="$(bash "$SCRIPT_DIR/retry.sh" "$@" 2>&1)"
    status=$?
    set -e

    if [ "$status" -ne 2 ]; then
        printf 'FAIL: %s returned %s instead of 2\n' "$description" "$status" >&2
        return 1
    fi
    if [ "$output" != "$EXPECTED_USAGE" ]; then
        printf 'FAIL: %s emitted unexpected error: %s\n' "$description" "$output" >&2
        return 1
    fi
    if [ -e "$INVALID_COMMAND_FILE" ]; then
        printf 'FAIL: %s ran the wrapped command\n' "$description" >&2
        return 1
    fi
}

assert_invalid "nonnumeric max attempts" nope 0 "$TMP_DIR/invalid_command"
assert_invalid "zero max attempts" 0 0 "$TMP_DIR/invalid_command"
assert_invalid "negative max attempts" -1 0 "$TMP_DIR/invalid_command"
assert_invalid "nonnumeric base delay" 3 nope "$TMP_DIR/invalid_command"
assert_invalid "negative base delay" 3 -1 "$TMP_DIR/invalid_command"
assert_invalid "missing command" 3 0

PATH="$TMP_DIR:$PATH" bash "$SCRIPT_DIR/retry.sh" \
    3 10 "$TMP_DIR/eventually_succeeds" "argument with spaces"

[ "$(cat "$ATTEMPT_FILE")" = "3" ]
[ "$(cat "$ARGUMENT_FILE")" = $'1\n<argument with spaces>\n1\n<argument with spaces>\n1\n<argument with spaces>' ]
[ "$(cat "$SLEEP_FILE")" = $'10\n20' ]

rm -f "$ATTEMPT_FILE" "$SLEEP_FILE"
set +e
PATH="$TMP_DIR:$PATH" bash "$SCRIPT_DIR/retry.sh" \
    3 0 "$TMP_DIR/always_fails"
status=$?
set -e

[ "$status" -eq 17 ]
[ "$(cat "$ATTEMPT_FILE")" = "3" ]

echo "retry tests passed"
