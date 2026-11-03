#!/usr/bin/env bash
set -euo pipefail

suite=$1 package=$2 shards=$3 excluded=$4 timeout=$5
: "${TEST_PG_URL:?TEST_PG_URL must point to a dedicated test database}"
[[ "$shards" =~ ^[1-9][0-9]*$ ]] || { echo 'shard count must be positive' >&2; exit 1; }
work=$(mktemp -d)
root=$PWD
pending=() databases=() pids=() statuses=()
# shellcheck disable=SC2329 # Invoked by the EXIT trap.
cleanup() {
    if ((${#pending[@]})); then
        kill "${pending[@]}" 2>/dev/null || true
        wait "${pending[@]}" 2>/dev/null || true
    fi
    mkdir -p "$root/gotestsum"
    rm -f "$root/gotestsum/$suite.json" "$root/gotestsum/$suite-reruns.txt"
    for ((i=0; i<shards; i++)); do
        [[ ! -f "$work/$i.json" ]] || cat "$work/$i.json" >> "$root/gotestsum/$suite.json"
        [[ ! -f "$work/$i-reruns.txt" ]] || cat "$work/$i-reruns.txt" >> "$root/gotestsum/$suite-reruns.txt"
        [[ -f "$work/$i.out" && -f "$work/$i.list" ]] || continue
        cat "$work/$i.out"
        listed=$(wc -l < "$work/$i.list")
        ran=$(awk '/"Action":"run"/ && match($0, /"Test":"[^"/]*"/) {seen[substr($0, RSTART, RLENGTH)]=1} END {for (test in seen) n++; print n+0}' "$work/$i.json" 2>/dev/null || echo 0)
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

go test -tags "fts5,pgtest" "$package" -list '^(Test|Example|Fuzz)' > "$work/list"
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
    TEST_PG_URL=$url go tool gotestsum --format pkgname-and-test-fails --format-icons text \
        --jsonfile "$work/$i.json" --rerun-fails=2 --rerun-fails-report "$work/$i-reruns.txt" \
        --packages "$package" -- -tags "fts5,pgtest" -run "^($pattern)$" -count=1 -timeout="$timeout" > "$work/$i.out" 2>&1 &
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
