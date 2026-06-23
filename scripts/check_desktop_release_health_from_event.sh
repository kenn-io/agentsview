#!/bin/bash
# Derive desktop release health inputs from the GitHub event payload.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

event_name="${GITHUB_EVENT_NAME:-}"
event_path="${GITHUB_EVENT_PATH:-}"

if [ -z "$event_path" ] || [ ! -f "$event_path" ]; then
    echo "::error::GITHUB_EVENT_PATH is not set or does not exist" >&2
    exit 1
fi

case "$event_name" in
    workflow_dispatch)
        tag="$(jq -r '.inputs.tag // ""' "$event_path")"
        conclusion="success"
        ;;
    workflow_run)
        tag="$(jq -r '.workflow_run.head_branch // ""' "$event_path")"
        conclusion="$(jq -r '.workflow_run.conclusion // ""' "$event_path")"
        ;;
    *)
        echo "::error::unsupported event for desktop release health: ${event_name:-<empty>}" >&2
        exit 1
        ;;
esac

DESKTOP_RELEASE_HEALTH_WORKFLOW_CONCLUSION="$conclusion" \
    "$SCRIPT_DIR/check_desktop_release_health.sh" "$tag"
