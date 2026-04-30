#!/bin/sh

set -eu

is_truthy() {
    case "${1:-}" in
        1|true|TRUE|True|yes|YES|on|ON)
            return 0
            ;;
    esac
    return 1
}

if [ "$#" -gt 0 ]; then
    case "$1" in
        -*)
            ;;
        *)
            exec /usr/local/bin/agentsview "$@"
            ;;
    esac
fi

if is_truthy "${PG_SERVE:-}"; then
    exec /usr/local/bin/agentsview pg serve "$@"
fi

exec /usr/local/bin/agentsview serve "$@"
