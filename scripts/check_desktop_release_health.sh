#!/bin/bash
# Verify that a tag's desktop release and updater manifest are in sync.
set -euo pipefail

tag="${1:-}"
repo="${GITHUB_REPOSITORY:-kenn-io/agentsview}"
workflow_conclusion="${DESKTOP_RELEASE_HEALTH_WORKFLOW_CONCLUSION:-success}"
assets_file="${DESKTOP_RELEASE_HEALTH_ASSETS_FILE:-}"
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
    error "Desktop Release workflow concluded ${workflow_conclusion} for ${tag}"
    exit 1
fi

if [ -n "$assets_file" ]; then
    assets="$(cat "$assets_file")"
else
    assets="$(gh release view "$tag" --repo "$repo" --json assets --jq '.assets[].name')"
fi

required_assets=(
    "AgentsView_${version}_aarch64.AppImage"
    "AgentsView_${version}_aarch64.dmg"
    "AgentsView_${version}_amd64.AppImage"
    "AgentsView_${version}_x64-setup.exe"
    "AgentsView_${version}_x64.dmg"
    "SHA256SUMS-desktop"
)

for asset in "${required_assets[@]}"; do
    if ! grep -Fxq "$asset" <<<"$assets"; then
        error "missing desktop release asset: $asset"
        exit 1
    fi
done

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

for platform in darwin-aarch64 darwin-x86_64 windows-x86_64 linux-x86_64; do
    url="$(jq -r --arg platform "$platform" '.platforms[$platform].url // ""' <<<"$manifest")"
    signature="$(jq -r --arg platform "$platform" '.platforms[$platform].signature // ""' <<<"$manifest")"
    if [ -z "$url" ] || [ -z "$signature" ]; then
        error "updater manifest missing url or signature for $platform"
        exit 1
    fi
done

echo "Desktop release health OK for $tag"
