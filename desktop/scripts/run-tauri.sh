#!/usr/bin/env bash
set -euo pipefail

# Wrapper that restores tauri.conf.json after `tauri` exits,
# undoing the version patch applied by prepare-sidecar.sh.
# Uses the .orig backup instead of git checkout to preserve
# any pre-existing uncommitted edits.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONF="$SCRIPT_DIR/../src-tauri/tauri.conf.json"

detect_host_triple() {
  if ! command -v rustc >/dev/null 2>&1; then
    echo "error: rustc is required to determine the Tauri target triple" >&2
    return 1
  fi
  local host
  host="$(rustc -vV | awk '/^host: /{print $2}')"
  if [ -z "$host" ]; then
    echo "error: could not determine host target triple" >&2
    return 1
  fi
  echo "$host"
}

resolve_build_target_arg() {
  if [ -n "${TAURI_ENV_TARGET_TRIPLE:-}" ]; then
    echo "$TAURI_ENV_TARGET_TRIPLE"
    return 0
  fi
  if [ -n "${CARGO_BUILD_TARGET:-}" ]; then
    echo "$CARGO_BUILD_TARGET"
    return 0
  fi

  # Tauri currently defaults to x64 on native Windows ARM64 shells in some
  # Node/npm environments, while prepare-sidecar builds the ARM64 sidecar from
  # rustc's host triple. Pin only that host so the app and sidecar match,
  # leaving other native builds on Tauri's default target/release layout.
  local host
  host="$(detect_host_triple)"
  if [ "$host" = "aarch64-pc-windows-msvc" ]; then
    echo "$host"
  fi
}

has_explicit_target() {
  local arg
  for arg in "$@"; do
    case "$arg" in
      --target | --target=*) return 0 ;;
    esac
  done
  return 1
}

cleanup() {
  if [ -f "$CONF.orig" ]; then
    mv "$CONF.orig" "$CONF"
  fi
}
trap cleanup EXIT INT TERM

args=("$@")
if [ "${args[0]:-}" = "build" ] && ! has_explicit_target "$@"; then
  target_triple="$(resolve_build_target_arg)"
  if [ -n "$target_triple" ]; then
    export TAURI_ENV_TARGET_TRIPLE="$target_triple"
    args=("build" "--target" "$target_triple" "${args[@]:1}")
    echo "Building Tauri target: $target_triple"
  fi
fi

tauri "${args[@]}"
