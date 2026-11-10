#!/usr/bin/env bash
# Runs scripts/macos-watcher-tests.sh against a generated module and checks
# which tests it selects for macOS.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_TMP="$(mktemp -d)"
trap 'rm -rf "$TEST_TMP"' EXIT

mkdir -p "$TEST_TMP/mod/watched" "$TEST_TMP/mod/empty"
printf 'module example.com/fixture\n\ngo 1.21\n' > "$TEST_TMP/mod/go.mod"
printf 'package watched\n' > "$TEST_TMP/mod/watched/watched.go"
printf 'package empty\n' > "$TEST_TMP/mod/empty/empty.go"
printf '%s\n' 'package empty' 'import "testing"' \
    'func TestUnrelated(t *testing.T) {}' \
    > "$TEST_TMP/mod/empty/unrelated_test.go"

write_test() {
    local name="$1"
    shift
    printf '%s\n' "$@" > "$TEST_TMP/mod/watched/$name"
}

write_test watcher_test.go 'package watched' 'import "testing"' \
    'func TestMain(m *testing.M) { m.Run() }' \
    'func TestWatcherCrossPlatform(t *testing.T) {}' \
    'func TestWatcherMultiline(' '	t *testing.T,' ') {}' \
    'func testWatcherHelper(t *testing.T) {}'
write_test watch_backend_test.go 'package watched' 'import "testing"' \
    'func TestBackendKqueue(t *testing.T) {}'
write_test watcher_darwin_test.go '//go:build darwin && cgo' '' \
    'package watched' 'import "testing"' \
    'func TestDarwinStream(t *testing.T) {}'
write_test change_time_test.go '//go:build darwin || linux' '' \
    'package watched' 'import "testing"' \
    'func TestChangeTime(t *testing.T) {}'
write_test watch_windows_test.go 'package watched' 'import "testing"' \
    'func TestWindowsOnly(t *testing.T) {}'
write_test engine_test.go 'package watched' 'import "testing"' \
    'func TestEngineUnrelated(t *testing.T) {}'
write_test external_watch_test.go 'package watched_test' 'import "testing"' \
    'func TestExternalUnrelated(t *testing.T) {}'
write_test watch_external_test.go 'package watched_test' 'import "testing"' \
    'func TestExternalWatcher(t *testing.T) {}'

run_selection() {
    (cd "$TEST_TMP/mod" && GOOS="$1" CGO_ENABLED=1 GOWORK=off \
        "$ROOT/scripts/macos-watcher-tests.sh" "${@:2}")
}

failures=0
check() {
    local desc="$1" want="$2" got="$3"
    if [[ "$want" == "$got" ]]; then
        echo "  PASS: $desc"
    else
        echo "  FAIL: $desc"
        echo "    want: $want"
        echo "    got:  $got"
        failures=$((failures + 1))
    fi
}

check "darwin selects watcher files and darwin-constrained files" \
    '^(TestBackendKqueue|TestChangeTime|TestDarwinStream|TestExternalWatcher|TestWatcherCrossPlatform|TestWatcherMultiline)$' \
    "$(run_selection darwin ./watched)"

check "linux omits darwin-only files" \
    '^(TestBackendKqueue|TestChangeTime|TestExternalWatcher|TestWatcherCrossPlatform|TestWatcherMultiline)$' \
    "$(run_selection linux ./watched)"

if output=$(run_selection darwin ./empty 2>&1); then
    check "a package without watcher tests fails" "exit 1" "exit 0: $output"
else
    check "a package without watcher tests fails" \
        "no macOS watcher tests found in: ./empty" "$output"
fi

if ((failures)); then
    echo "$failures check(s) failed"
    exit 1
fi
