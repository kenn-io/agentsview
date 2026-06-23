#!/bin/bash
# Tests for deriving desktop release health inputs from GitHub event payloads.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WRAPPER="$SCRIPT_DIR/check_desktop_release_health_from_event.sh"

PASS=0
FAIL=0

assert_success() {
    local desc="$1"
    shift
    if "$@" >/tmp/check-desktop-release-health-event-test.out 2>&1; then
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $desc"
        cat /tmp/check-desktop-release-health-event-test.out
        FAIL=$((FAIL + 1))
    fi
}

assert_failure_contains() {
    local desc="$1" expected="$2"
    shift 2
    if "$@" >/tmp/check-desktop-release-health-event-test.out 2>&1; then
        echo "  FAIL: $desc"
        echo "    expected failure containing: $expected"
        FAIL=$((FAIL + 1))
        return
    fi
    if grep -Fq "$expected" /tmp/check-desktop-release-health-event-test.out; then
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $desc"
        echo "    expected output containing: $expected"
        cat /tmp/check-desktop-release-health-event-test.out
        FAIL=$((FAIL + 1))
    fi
}

write_fixture() {
    local dir="$1" version="$2"
    cat >"$dir/assets.txt" <<EOF
AgentsView_${version}_aarch64.AppImage
AgentsView_${version}_aarch64.dmg
AgentsView_${version}_amd64.AppImage
AgentsView_${version}_x64-setup.exe
AgentsView_${version}_x64.dmg
SHA256SUMS-desktop
EOF
    cat >"$dir/latest.json" <<EOF
{"version":"${version}","platforms":{"darwin-aarch64":{"url":"u","signature":"s"},"darwin-x86_64":{"url":"u","signature":"s"},"windows-x86_64":{"url":"u","signature":"s"},"linux-x86_64":{"url":"u","signature":"s"}}}
EOF
}

run_wrapper() {
    local dir="$1" event_name="$2" event_file="$3"
    GITHUB_EVENT_NAME="$event_name" \
    GITHUB_EVENT_PATH="$event_file" \
    DESKTOP_RELEASE_HEALTH_ASSETS_FILE="$dir/assets.txt" \
    DESKTOP_RELEASE_HEALTH_MANIFEST_FILE="$dir/latest.json" \
    "$WRAPPER"
}

echo "=== desktop release health event wrapper ==="

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp" /tmp/check-desktop-release-health-event-test.out' EXIT
write_fixture "$tmp" "0.34.5"

cat >"$tmp/workflow-run.json" <<'EOF'
{"workflow_run":{"event":"push","head_branch":"v0.34.5","conclusion":"success"}}
EOF
assert_success \
    "workflow_run tag event passes validated tag" \
    run_wrapper "$tmp" "workflow_run" "$tmp/workflow-run.json"

cat >"$tmp/manual.json" <<'EOF'
{"inputs":{"tag":"v0.34.5"}}
EOF
assert_success \
    "manual dispatch input passes validated tag" \
    run_wrapper "$tmp" "workflow_dispatch" "$tmp/manual.json"

cat >"$tmp/malicious.json" <<'EOF'
{"workflow_run":{"event":"push","head_branch":"v0.34.5; echo injected","conclusion":"success"}}
EOF
assert_failure_contains \
    "malformed event tag is rejected" \
    "expected release tag like v0.34.5" \
    run_wrapper "$tmp" "workflow_run" "$tmp/malicious.json"

cat >"$tmp/failed.json" <<'EOF'
{"workflow_run":{"event":"push","head_branch":"v0.34.5","conclusion":"failure"}}
EOF
assert_failure_contains \
    "failed workflow_run conclusion is propagated" \
    "Desktop Release workflow concluded failure for v0.34.5" \
    run_wrapper "$tmp" "workflow_run" "$tmp/failed.json"

echo
echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
