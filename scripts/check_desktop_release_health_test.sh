#!/bin/bash
# Tests for the desktop release health checker.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECKER="$SCRIPT_DIR/check_desktop_release_health.sh"

PASS=0
FAIL=0

assert_success() {
    local desc="$1"
    shift
    if "$@" >/tmp/check-desktop-release-health-test.out 2>&1; then
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $desc"
        cat /tmp/check-desktop-release-health-test.out
        FAIL=$((FAIL + 1))
    fi
}

assert_failure_contains() {
    local desc="$1" expected="$2"
    shift 2
    if "$@" >/tmp/check-desktop-release-health-test.out 2>&1; then
        echo "  FAIL: $desc"
        echo "    expected failure containing: $expected"
        FAIL=$((FAIL + 1))
        return
    fi
    if grep -Fq "$expected" /tmp/check-desktop-release-health-test.out; then
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $desc"
        echo "    expected output containing: $expected"
        cat /tmp/check-desktop-release-health-test.out
        FAIL=$((FAIL + 1))
    fi
}

write_fixture() {
    local dir="$1" manifest_version="$2"
    cat >"$dir/latest.json" <<EOF
{"version":"${manifest_version}"}
EOF
}

run_checker() {
    local dir="$1" tag="$2" conclusion="${3:-success}"
    DESKTOP_RELEASE_HEALTH_MANIFEST_FILE="$dir/latest.json" \
    DESKTOP_RELEASE_HEALTH_WORKFLOW_CONCLUSION="$conclusion" \
    "$CHECKER" "$tag"
}

echo "=== desktop release health ==="

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp" /tmp/check-desktop-release-health-test.out' EXIT
write_fixture "$tmp" "0.34.5"

assert_success "healthy desktop release passes" run_checker "$tmp" "v0.34.5"

write_fixture "$tmp" "0.0.1-staging.1"
assert_success \
    "semver prerelease desktop release passes" \
    run_checker "$tmp" "v0.0.1-staging.1"

assert_failure_contains \
    "failed desktop workflow is loud" \
    "Desktop Release workflow concluded failure for v0.34.5" \
    run_checker "$tmp" "v0.34.5" "failure"

write_fixture "$tmp" "0.34.4"
assert_failure_contains \
    "stale updater manifest fails" \
    "updater manifest version 0.34.4 does not match expected 0.34.5" \
    run_checker "$tmp" "v0.34.5"

echo
echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
