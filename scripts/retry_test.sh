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

chmod +x "$TMP_DIR/eventually_succeeds" "$TMP_DIR/always_fails" "$TMP_DIR/sleep"

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
