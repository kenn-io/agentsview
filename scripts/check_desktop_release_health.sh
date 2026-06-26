#!/bin/bash
# Verify that a tag's desktop release and updater manifest are in sync.
set -euo pipefail

tag="${1:-}"
repo="${GITHUB_REPOSITORY:-kenn-io/agentsview}"
workflow_conclusion="${DESKTOP_RELEASE_HEALTH_WORKFLOW_CONCLUSION-success}"
manifest_file="${DESKTOP_RELEASE_HEALTH_MANIFEST_FILE:-}"

error() {
    echo "::error::$*" >&2
}

if [[ ! "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]]; then
    error "expected release tag like v0.34.5 or v0.34.5-rc.1, got '${tag:-<empty>}'"
    exit 1
fi

version="${tag#v}"

if [ "$workflow_conclusion" != "success" ]; then
    error "Desktop Release workflow concluded ${workflow_conclusion:-<empty>} for ${tag}"
    exit 1
fi

if [ -n "$manifest_file" ]; then
    manifest="$(cat "$manifest_file")"
else
    manifest="$(curl -fsSL "https://github.com/${repo}/releases/download/updater/latest.json")"
fi

manifest_version="$(jq -r '.version // ""' <<<"$manifest")"
if [ "$manifest_version" != "$version" ]; then
    error "updater manifest version ${manifest_version:-<empty>} does not match expected $version"
    exit 1
fi

echo "Desktop release health OK for $tag"
