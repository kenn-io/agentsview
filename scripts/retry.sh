#!/bin/bash
set -u

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
