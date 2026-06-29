#!/bin/bash
# Tests for install.sh version parsing logic.
# Sources install.sh directly so the test exercises the real
# get_latest_version function (with curl mocked out).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Source install.sh to get access to get_latest_version.
# The main() call is guarded so nothing runs on source.
# shellcheck source=install.sh
source "$SCRIPT_DIR/install.sh"

PASS=0
FAIL=0

assert_eq() {
    local desc="$1" expected="$2" actual="$3"
    if [ "$expected" = "$actual" ]; then
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $desc"
        echo "    expected: '$expected'"
        echo "    actual:   '$actual'"
        FAIL=$((FAIL + 1))
    fi
}

# Mock curl to simulate the redirect-follow behavior of the real install
# script. get_latest_version uses `curl ... -w '%{url_effective}'` so we
# intercept that and emit $MOCK_FINAL_URL (the URL after following 302s).
MOCK_FINAL_URL=""
MOCK_CURL_EXIT=0
curl() {
    if [ "$MOCK_CURL_EXIT" != "0" ]; then
        return "$MOCK_CURL_EXIT"
    fi
    printf '%s' "$MOCK_FINAL_URL"
}
export -f curl

echo "=== get_latest_version parsing ==="

# Normal release tag
MOCK_FINAL_URL="https://github.com/kenn-io/agentsview/releases/tag/v0.8.0"
assert_eq "release tag" "v0.8.0" "$(get_latest_version)"

# Newer release tag
MOCK_FINAL_URL="https://github.com/kenn-io/agentsview/releases/tag/v0.30.1"
assert_eq "two-digit minor" "v0.30.1" "$(get_latest_version)"

# Pre-release version
MOCK_FINAL_URL="https://github.com/kenn-io/agentsview/releases/tag/v0.9.0-rc1"
assert_eq "pre-release version" "v0.9.0-rc1" "$(get_latest_version)"

# No releases: GitHub returns the /releases page itself instead of a 302.
MOCK_FINAL_URL="https://github.com/kenn-io/agentsview/releases"
assert_eq "no releases returns empty" "" "$(get_latest_version || true)"

# Redirect to latest sentinel (no follow / no tag in URL).
MOCK_FINAL_URL="https://github.com/kenn-io/agentsview/releases/latest"
assert_eq "unresolved latest returns empty" "" "$(get_latest_version || true)"

# Network failure: curl exits non-zero, function should fail.
MOCK_FINAL_URL=""
MOCK_CURL_EXIT=22
assert_eq "curl failure returns empty" "" "$(get_latest_version || true)"
MOCK_CURL_EXIT=0

echo
echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]

bash "$SCRIPT_DIR/make_install_test.sh"
