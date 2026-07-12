#!/bin/bash
set -u

usage() {
    echo "usage: retry.sh <max-attempts> <base-delay-seconds> <command> [args...]" >&2
    exit 2
}

if [ "$#" -lt 3 ] || ! [[ "$1" =~ ^[1-9][0-9]*$ ]] || \
    ! [[ "$2" =~ ^[0-9]+$ ]]; then
    usage
fi

max_attempts="$1"
base_delay="$2"
shift 2

attempt=1
while true; do
    "$@"
    status=$?
    if [ "$status" -eq 0 ]; then
        exit 0
    fi
    if [ "$attempt" -ge "$max_attempts" ]; then
        exit "$status"
    fi

    delay=$((base_delay * attempt))
    printf 'command failed with status %s; retrying in %s seconds\n' \
        "$status" "$delay" >&2
    sleep "$delay"
    attempt=$((attempt + 1))
done
