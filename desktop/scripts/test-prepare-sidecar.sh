#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=prepare-sidecar.sh
source "$SCRIPT_DIR/prepare-sidecar.sh"

assert_eq() {
  local got="$1"
  local want="$2"
  local msg="$3"
  if [ "$got" != "$want" ]; then
    echo "assertion failed: $msg (got='$got' want='$want')" >&2
    exit 1
  fi
}

assert_fails() {
  local msg="$1"
  shift
  if "$@" >/dev/null 2>&1; then
    echo "assertion failed: expected failure: $msg" >&2
    exit 1
  fi
}

assert_eq "$(map_go_target aarch64-apple-darwin)" "darwin arm64" "map darwin arm64"
assert_eq "$(map_go_target x86_64-apple-darwin)" "darwin amd64" "map darwin amd64"
assert_eq "$(map_go_target x86_64-pc-windows-msvc)" "windows amd64" "map windows amd64"
assert_eq "$(map_go_target x86_64-unknown-linux-gnu)" "linux amd64" "map linux amd64"
assert_fails "unsupported triple rejected" map_go_target "weird-target"

resolved_version="$(resolve_version)"
if [ -z "$resolved_version" ] || [ "$resolved_version" = "dev" ]; then
  echo "assertion failed: resolve_version should use git metadata (got '$resolved_version')" >&2
  exit 1
fi

# AGENTSVIEW_VERSION env var overrides git describe
assert_eq "$(AGENTSVIEW_VERSION=v0.5.0-staging.1 resolve_version)" \
  "v0.5.0-staging.1" "AGENTSVIEW_VERSION override"

target="$(
  TAURI_ENV_TARGET_TRIPLE="tauri-priority-target" CARGO_BUILD_TARGET="cargo-target" \
    resolve_target_triple
)"
assert_eq "$target" "tauri-priority-target" "TAURI target precedence"

target="$(
  unset TAURI_ENV_TARGET_TRIPLE
  CARGO_BUILD_TARGET="cargo-target" resolve_target_triple
)"
assert_eq "$target" "cargo-target" "Cargo target fallback"

# version_to_semver tests
assert_eq "$(version_to_semver "v0.10.0")" "0.10.0" \
  "tagged release"
assert_eq "$(version_to_semver "0.10.0")" "0.10.0" \
  "tagged release without v prefix"
assert_eq "$(version_to_semver "v0.10.0-3-gabcdef")" "0.10.0-dev.3" \
  "git-describe with distance"
assert_eq "$(version_to_semver "v1.2.3-15-g1234567")" "1.2.3-dev.15" \
  "git-describe large distance"
assert_eq "$(version_to_semver "v0.10.0-dirty")" "0.10.0" \
  "dirty tag stripped"
assert_eq "$(version_to_semver "v0.10.0-3-gabcdef-dirty")" "0.10.0-dev.3" \
  "git-describe dirty with distance"
# Non-tag inputs return empty (skip patching)
assert_eq "$(version_to_semver "abc1234")" "" \
  "bare hash returns empty"
assert_eq "$(version_to_semver "1abc234")" "" \
  "digit-leading hash returns empty"
assert_eq "$(version_to_semver "9f0e1d2")" "" \
  "digit-leading hex hash returns empty"
assert_eq "$(version_to_semver "dev")" "" \
  "dev string returns empty"

# Prerelease tags accepted
assert_eq "$(version_to_semver "v1.2.3-rc.1")" "1.2.3-rc.1" \
  "prerelease tag"
assert_eq "$(version_to_semver "v0.0.1-staging.1")" "0.0.1-staging.1" \
  "staging prerelease tag"
assert_eq "$(version_to_semver "v1.0.0-beta")" "1.0.0-beta" \
  "simple prerelease tag"
assert_eq "$(version_to_semver "v1.0.0-alpha.2-dirty")" "1.0.0-alpha.2" \
  "prerelease tag dirty stripped"

# Hyphens within identifiers
assert_eq "$(version_to_semver "v1.0.0-rc-1")" "1.0.0-rc-1" \
  "prerelease with hyphen in identifier"
assert_eq "$(version_to_semver "v2.0.0-pre-alpha.3")" "2.0.0-pre-alpha.3" \
  "prerelease with hyphenated identifier and dot"

# Malformed prerelease rejected
assert_fails "empty prerelease identifier rejected" \
  version_to_semver "v1.0.0-.."
assert_fails "trailing dot rejected" \
  version_to_semver "v1.0.0-rc."

# Malformed v-prefixed versions fail hard
assert_fails "incomplete semver rejected" version_to_semver "v1.2"
assert_fails "bare digits rejected" version_to_semver "1.2"

# patch_tauri_version test (uses a temp copy)
tmp_root="$(mktemp -d)"
mkdir -p "$tmp_root/src-tauri"
cp "$SCRIPT_DIR/../src-tauri/tauri.conf.json" "$tmp_root/src-tauri/tauri.conf.json"
saved_tauri_dir="$TAURI_DIR"
TAURI_DIR="$tmp_root"
patch_tauri_version "v0.10.0" >/dev/null
patched="$(grep '"version"' "$tmp_root/src-tauri/tauri.conf.json" | head -1)"
assert_eq "$(echo "$patched" | tr -d ' ')" '"version":"0.10.0",' \
  "patch_tauri_version applies correct version"

# patch_tauri_version skips non-tag builds
cp "$saved_tauri_dir/src-tauri/tauri.conf.json" "$tmp_root/src-tauri/tauri.conf.json"
original="$(grep '"version"' "$tmp_root/src-tauri/tauri.conf.json" | head -1)"
output="$(patch_tauri_version "abc1234")"
after="$(grep '"version"' "$tmp_root/src-tauri/tauri.conf.json" | head -1)"
assert_eq "$after" "$original" \
  "patch_tauri_version skips patching for non-tag build"
echo "$output" | grep -q "Skipping" || {
  echo "assertion failed: expected skip message (got '$output')" >&2
  exit 1
}

TAURI_DIR="$saved_tauri_dir"
rm -rf "$tmp_root"

(
  tmp_root="$(mktemp -d)"
  trap 'rm -rf "$tmp_root"' EXIT
  REPO_ROOT="$tmp_root/repo"
  mkdir -p "$REPO_ROOT/frontend/dist/assets" "$REPO_ROOT/internal/web/dist"
  printf 'prebuilt page' > "$REPO_ROOT/frontend/dist/index.html"
  printf 'prebuilt asset' > "$REPO_ROOT/frontend/dist/assets/app.js"
  printf 'stale' > "$REPO_ROOT/internal/web/dist/stale.js"
  npm() {
    assert_eq "$PWD" "$REPO_ROOT/frontend" "npm runs in frontend"
    printf '%s\n' "$*" >> "$tmp_root/npm.log"
    case "$*" in
      ci) ;;
      'run build')
        mkdir -p "$REPO_ROOT/frontend/dist"
        printf 'local page' > "$REPO_ROOT/frontend/dist/index.html"
        ;;
      *) return 1 ;;
    esac
  }

  AGENTSVIEW_FRONTEND_PREBUILT=true prepare_frontend
  assert_eq "$(cat "$REPO_ROOT/internal/web/dist/index.html")" "prebuilt page" "copy prebuilt page"
  assert_eq "$(cat "$REPO_ROOT/internal/web/dist/assets/app.js")" "prebuilt asset" "copy nested prebuilt assets"
  assert_eq "$([ -e "$tmp_root/npm.log" ] && echo called || echo skipped)" "skipped" "prebuilt skips npm"
  assert_eq "$([ -e "$REPO_ROOT/internal/web/dist/stale.js" ] && echo present || echo removed)" "removed" "replace stale assets"

  rm -rf "$REPO_ROOT/frontend/dist" "$REPO_ROOT/internal/web/dist"
  mkdir -p "$REPO_ROOT/internal/web/dist"
  printf 'stale' > "$REPO_ROOT/internal/web/dist/stale.js"
  unset AGENTSVIEW_FRONTEND_PREBUILT
  prepare_frontend
  assert_eq "$(cat "$tmp_root/npm.log")" $'ci\nrun build' "local install precedes build"
  assert_eq "$(cat "$REPO_ROOT/internal/web/dist/index.html")" "local page" "copy local build"
  assert_eq "$([ -e "$REPO_ROOT/internal/web/dist/stale.js" ] && echo present || echo removed)" "removed" "replace stale local assets"
)

echo "prepare-sidecar target mapping checks passed"
