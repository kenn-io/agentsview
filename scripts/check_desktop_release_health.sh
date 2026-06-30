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

if ! jq -e . >/dev/null 2>&1 <<<"$manifest"; then
    error "updater manifest is not valid JSON"
    exit 1
fi

manifest_version="$(jq -r '.version // ""' <<<"$manifest")"
if [ "$manifest_version" != "$version" ]; then
    error "updater manifest version ${manifest_version:-<empty>} does not match expected $version"
    exit 1
fi

expected_platforms=(
    "darwin-aarch64"
    "darwin-x86_64"
    "windows-x86_64"
    "linux-x86_64"
)

expected_platforms_list() {
    local label="" platform
    for platform in "${expected_platforms[@]}"; do
        if [ -n "$label" ]; then
            label+=", "
        fi
        label+="$platform"
    done
    printf "%s" "$label"
}

if ! jq -e '.platforms | type == "object"' >/dev/null <<<"$manifest"; then
    error "updater manifest platforms must be an object with signatures for $(expected_platforms_list)"
    exit 1
fi

for platform in "${expected_platforms[@]}"; do
    if ! jq -e --arg platform "$platform" '.platforms[$platform] | type == "object"' >/dev/null <<<"$manifest"; then
        error "updater manifest missing platform $platform"
        exit 1
    fi

    if ! jq -e --arg platform "$platform" '.platforms[$platform].url | select(type == "string" and length > 0)' >/dev/null <<<"$manifest"; then
        error "updater manifest missing url for $platform"
        exit 1
    fi

    if ! jq -e --arg platform "$platform" '.platforms[$platform].signature | type == "string" and length > 0' >/dev/null <<<"$manifest"; then
        error "updater manifest missing signature for $platform"
        exit 1
    fi

    signature="$(jq -r --arg platform "$platform" '.platforms[$platform].signature' <<<"$manifest")"
    if [ -z "$signature" ] ||
        ! [[ "$signature" =~ ^[A-Za-z0-9+/]+={0,2}$ ]] ||
        [ $(( ${#signature} % 4 )) -ne 0 ]; then
        error "updater manifest signature for $platform is not a base64 payload"
        exit 1
    fi
done

echo "Desktop release health OK for $tag"
