#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_TMP="$(mktemp -d)"
trap 'rm -rf "$TEST_TMP"' EXIT

FIXTURE="$TEST_TMP/testfixture"
SERVER="$TEST_TMP/agentsview"
ARGS_FILE="$TEST_TMP/server-args"
FIXTURE_MARKER="$TEST_TMP/fixture-ran"

# The quoted variables below belong to the generated fixture scripts and must
# expand only when those scripts run.
# shellcheck disable=SC2016
printf '%s\n' \
    '#!/usr/bin/env bash' \
    'set -euo pipefail' \
    ': > "${E2E_TEST_FIXTURE_MARKER:?}"' \
    > "$FIXTURE"
# shellcheck disable=SC2016
printf '%s\n' \
    '#!/usr/bin/env bash' \
    'set -euo pipefail' \
    'printf '\''%s\n'\'' "$@" > "${E2E_TEST_ARGS:?}"' \
    > "$SERVER"
chmod +x "$FIXTURE" "$SERVER"

common_env=(
    "E2E_PREBUILT_FIXTURE=$FIXTURE"
    "E2E_PREBUILT_SERVER=$SERVER"
    "E2E_TEST_ARGS=$ARGS_FILE"
    "E2E_TEST_FIXTURE_MARKER=$FIXTURE_MARKER"
)

assert_server_port() {
    local want="$1"
    local args=()
    while IFS= read -r arg; do
        args[${#args[@]}]="$arg"
    done < "$ARGS_FILE"
    local i
    for ((i = 0; i + 1 < ${#args[@]}; i++)); do
        if [[ "${args[$i]}" == "--port" && "${args[$((i + 1))]}" == "$want" ]]; then
            return
        fi
    done
    printf 'server args did not contain --port %s: %s\n' "$want" "${args[*]}" >&2
    exit 1
}

run_valid() {
    local mode="$1"
    local value="$2"
    local want="$3"
    rm -f "$ARGS_FILE" "$FIXTURE_MARKER"
    if [[ "$mode" == "unset" ]]; then
        env -u AGENTSVIEW_E2E_PORT "${common_env[@]}" \
            bash "$ROOT/scripts/e2e-server.sh" > "$TEST_TMP/stdout" 2> "$TEST_TMP/stderr"
    else
        env "${common_env[@]}" "AGENTSVIEW_E2E_PORT=$value" \
            bash "$ROOT/scripts/e2e-server.sh" > "$TEST_TMP/stdout" 2> "$TEST_TMP/stderr"
    fi
    test -f "$FIXTURE_MARKER"
    assert_server_port "$want"
}

run_invalid() {
    local value="$1"
    rm -f "$ARGS_FILE" "$FIXTURE_MARKER"
    if env "${common_env[@]}" "AGENTSVIEW_E2E_PORT=$value" \
        bash "$ROOT/scripts/e2e-server.sh" > "$TEST_TMP/stdout" 2> "$TEST_TMP/stderr"; then
        printf 'expected invalid AGENTSVIEW_E2E_PORT=%q to fail\n' "$value" >&2
        exit 1
    fi
    grep -q 'AGENTSVIEW_E2E_PORT must be an integer from 1 to 65535' \
        "$TEST_TMP/stderr"
    if [[ -e "$FIXTURE_MARKER" || -e "$ARGS_FILE" ]]; then
        printf 'invalid port %q reached the fixture or server\n' "$value" >&2
        exit 1
    fi
}

run_valid unset "" 8090
run_valid set "" 8090
run_valid set 48123 48123

for invalid in 0 -1 65536 1.5 abc " 80 "; do
    run_invalid "$invalid"
done

printf 'e2e server port tests passed\n'
