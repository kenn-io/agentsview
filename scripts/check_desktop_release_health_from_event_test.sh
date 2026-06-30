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
    cat >"$dir/latest.json" <<EOF
{
  "version": "${version}",
  "platforms": {
    "darwin-aarch64": {
      "url": "https://github.com/kenn-io/agentsview/releases/download/updater/AgentsView_aarch64.app.tar.gz",
      "signature": "YWJjCg=="
    },
    "darwin-x86_64": {
      "url": "https://github.com/kenn-io/agentsview/releases/download/updater/AgentsView_x86_64.app.tar.gz",
      "signature": "YWJjCg=="
    },
    "windows-x86_64": {
      "url": "https://github.com/kenn-io/agentsview/releases/download/updater/AgentsView_0.34.5_x64-setup.nsis.zip",
      "signature": "YWJjCg=="
    },
    "linux-x86_64": {
      "url": "https://github.com/kenn-io/agentsview/releases/download/updater/AgentsView_0.34.5_amd64.AppImage.tar.gz",
      "signature": "YWJjCg=="
    }
  }
}
EOF
}

run_wrapper() {
    local dir="$1" event_name="$2" event_file="$3"
    GITHUB_EVENT_NAME="$event_name" \
    GITHUB_EVENT_PATH="$event_file" \
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

write_fixture "$tmp" "0.0.1-staging.1"
cat >"$tmp/workflow-run-prerelease.json" <<'EOF'
{"workflow_run":{"event":"push","head_branch":"v0.0.1-staging.1","conclusion":"success"}}
EOF
assert_success \
    "workflow_run prerelease tag passes validated tag" \
    run_wrapper "$tmp" "workflow_run" "$tmp/workflow-run-prerelease.json"
write_fixture "$tmp" "0.34.5"

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

cat >"$tmp/missing-conclusion.json" <<'EOF'
{"workflow_run":{"event":"push","head_branch":"v0.34.5"}}
EOF
assert_failure_contains \
    "missing workflow_run conclusion fails closed" \
    "Desktop Release workflow concluded <empty> for v0.34.5" \
    run_wrapper "$tmp" "workflow_run" "$tmp/missing-conclusion.json"

cat >"$tmp/unsupported.json" <<'EOF'
{"inputs":{"tag":"v0.34.5"}}
EOF
assert_failure_contains \
    "unsupported event fails" \
    "unsupported event for desktop release health: schedule" \
    run_wrapper "$tmp" "schedule" "$tmp/unsupported.json"

assert_failure_contains \
    "missing event path fails" \
    "GITHUB_EVENT_PATH is not set or does not exist" \
    run_wrapper "$tmp" "workflow_dispatch" "$tmp/missing.json"

echo
echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
