#!/usr/bin/env bash
set -euo pipefail

package=$1 shards=$2 excluded=$3 timeout=$4
: "${TEST_PG_URL:?TEST_PG_URL must point to a dedicated test database}"
[[ "$shards" =~ ^[1-9][0-9]*$ ]] || { echo 'shard count must be positive' >&2; exit 1; }
work=$(mktemp -d)
pending=() databases=() pids=() statuses=()
# shellcheck disable=SC2329 # Invoked by the EXIT trap.
cleanup() {
    if ((${#pending[@]})); then
        kill "${pending[@]}" 2>/dev/null || true
        wait "${pending[@]}" 2>/dev/null || true
    fi
    for ((i=0; i<shards; i++)); do
        [[ -f "$work/$i.out" && -f "$work/$i.list" ]] || continue
        cat "$work/$i.out"
        listed=$(wc -l < "$work/$i.list")
        ran=$(awk '/^--- / {n++} END {print n+0}' "$work/$i.out")
        printf 'shard %s: %s listed, %s ran, exit %s\n' "$i" "$listed" "$ran" "${statuses[i]:-interrupted}"
    done
    for database in "${databases[@]}"; do
        psql -X -v ON_ERROR_STOP=1 "$TEST_PG_URL" -c "DROP DATABASE IF EXISTS \"${database//\"/\"\"}\" WITH (FORCE)" || true
    done
    rm -rf "$work"
}
trap 'cleanup' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

dir=$(go list -f '{{.Dir}}' "$package")
go test -c -tags "fts5,pgtest" -o "$work/pkg.test" "$package"
"$work/pkg.test" -test.list '^(Test|Example|Fuzz)' > "$work/list"
mapfile -t tests < <(awk -v excluded="$excluded" '/^(Test|Example|Fuzz)/ && $0 != excluded' "$work/list" | LC_ALL=C sort)
((${#tests[@]} >= shards)) || { echo 'empty test list or shard' >&2; exit 1; }
printf '%s tests in %s processes\n' "${#tests[@]}" "$shards"

base=${TEST_PG_URL%%\?*}
query=${TEST_PG_URL#"$base"}
database=$(psql -X -v ON_ERROR_STOP=1 "$TEST_PG_URL" -Atc 'SELECT current_database()')
for ((i=0; i<shards; i++)); do
    url=$TEST_PG_URL
    if ((i > 0)); then
        shard_db=${database}_shard$i
        databases+=("$shard_db")
        psql -X -v ON_ERROR_STOP=1 "$TEST_PG_URL" -c "DROP DATABASE IF EXISTS \"${shard_db//\"/\"\"}\" WITH (FORCE)" -c "CREATE DATABASE \"${shard_db//\"/\"\"}\""
        url=${base%/*}/$shard_db$query
    fi
    names=()
    for ((j=i; j<${#tests[@]}; j+=shards)); do names+=("${tests[j]}"); done
    printf '%s\n' "${names[@]}" > "$work/$i.list"
    pattern=$(IFS='|'; echo "${names[*]}")
    cd "$dir"
    TEST_PG_URL=$url "$work/pkg.test" -test.v -test.paniconexit0 -test.run "^($pattern)$" -test.timeout="$timeout" > "$work/$i.out" 2>&1 &
    pids[i]=$!
    pending+=("$!")
done

result=0
for ((i=0; i<shards; i++)); do
    status=0
    wait "${pids[i]}" || status=$?
    statuses[i]=$status
    pending=("${pids[@]:i+1}")
    if ((status != 0)); then result=1; fi
done
exit "$result"
