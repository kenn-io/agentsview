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
{
  "version": "${manifest_version}",
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

cat >"$tmp/latest.json" <<'EOF'
{"version":"0.34.5"}
EOF
assert_failure_contains \
    "manifest without updater platforms fails" \
    "updater manifest platforms must be an object with signatures for darwin-aarch64, darwin-x86_64, windows-x86_64, linux-x86_64" \
    run_checker "$tmp" "v0.34.5"

cat >"$tmp/latest.json" <<'EOF'
{"version":"0.34.5","platforms":{}}
EOF
assert_failure_contains \
    "manifest with empty updater platforms fails" \
    "updater manifest missing platform darwin-aarch64" \
    run_checker "$tmp" "v0.34.5"

cat >"$tmp/latest.json" <<'EOF'
{"version":"0.34.5","platforms":[]}
EOF
assert_failure_contains \
    "manifest with non-object updater platforms fails" \
    "updater manifest platforms must be an object with signatures for darwin-aarch64, darwin-x86_64, windows-x86_64, linux-x86_64" \
    run_checker "$tmp" "v0.34.5"

cat >"$tmp/latest.json" <<'EOF'
{
  "version": "0.34.5",
  "platforms": {
    "darwin-aarch64": {
      "url": "https://github.com/kenn-io/agentsview/releases/download/updater/AgentsView_aarch64.app.tar.gz"
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
assert_failure_contains \
    "manifest with missing updater signature fails" \
    "updater manifest missing signature for darwin-aarch64" \
    run_checker "$tmp" "v0.34.5"

cat >"$tmp/latest.json" <<'EOF'
{
  "version": "0.34.5",
  "platforms": {
    "darwin-aarch64": {
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
assert_failure_contains \
    "manifest with missing updater URL fails" \
    "updater manifest missing url for darwin-aarch64" \
    run_checker "$tmp" "v0.34.5"

cat >"$tmp/latest.json" <<'EOF'
{
  "version": "0.34.5",
  "platforms": {
    "darwin-aarch64": {
      "url": "",
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
assert_failure_contains \
    "manifest with empty updater URL fails" \
    "updater manifest missing url for darwin-aarch64" \
    run_checker "$tmp" "v0.34.5"

cat >"$tmp/latest.json" <<'EOF'
{
  "version": "0.34.5",
  "platforms": {
    "linux-x86_64": {
      "url": "https://github.com/kenn-io/agentsview/releases/download/updater/AgentsView_0.34.5_amd64.AppImage.tar.gz",
      "signature": "
Public signature:
YWJjCg=="
    }
  }
}
EOF
assert_failure_contains \
    "invalid updater manifest JSON fails" \
    "updater manifest is not valid JSON" \
    run_checker "$tmp" "v0.34.5"

cat >"$tmp/latest.json" <<'EOF'
{
  "version": "0.34.5",
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
      "signature": "Public signature: YWJjCg=="
    }
  }
}
EOF
assert_failure_contains \
    "non-base64 updater signature fails" \
    "updater manifest signature for linux-x86_64 is not a base64 payload" \
    run_checker "$tmp" "v0.34.5"

echo
echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
