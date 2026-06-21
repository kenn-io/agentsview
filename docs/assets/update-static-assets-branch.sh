#!/usr/bin/env bash
# Update the docs-assets orphan branch from hydrated static docs assets.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
docs_root="$(cd "$script_dir/.." && pwd)"
repo_root="$(cd "$docs_root/.." && pwd)"
assets_branch="${AGENTSVIEW_DOCS_ASSETS_BRANCH:-docs-assets}"
source_dir="${AGENTSVIEW_DOCS_STATIC_ASSETS_DIR:-$docs_root/assets/static}"
push=false

expected_assets=(
  "agents/amp.svg"
  "agents/claude-code.svg"
  "agents/codex.svg"
  "agents/copilot.svg"
  "agents/cortex-code.svg"
  "agents/cursor.svg"
  "agents/forge.png"
  "agents/gemini.svg"
  "agents/hermes.svg"
  "agents/iflow.svg"
  "agents/kimi.svg"
  "agents/kiro.svg"
  "agents/openclaw.svg"
  "agents/opencode.svg"
  "agents/openhands.svg"
  "agents/pi.svg"
  "agents/positron.svg"
  "agents/vscode-copilot.svg"
  "agents/warp.svg"
  "agents/zencoder.svg"
  "architecture.svg"
  "og-image.png"
)

usage() {
  cat <<EOF
Usage: $(basename "$0") [--source DIR] [--push]

Update the local $assets_branch branch to a single orphan commit containing
curated static docs assets. Pass --push to force-push that branch to origin.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --source)
      [[ $# -ge 2 ]] || { printf 'ERROR: --source requires a directory\n' >&2; exit 2; }
      source_dir="$2"
      shift 2
      ;;
    --push)
      push=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      printf 'unknown option: %s\n' "$1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

source_dir="$(cd "$source_dir" 2>/dev/null && pwd)" || {
  printf 'static docs asset source does not exist: %s\n' "$source_dir" >&2
  exit 1
}

for asset in "${expected_assets[@]}"; do
  if [[ -L "$source_dir/$asset" ]]; then
    printf 'static docs asset source must not be a symlink: %s\n' "$asset" >&2
    exit 1
  fi
  if [[ ! -f "$source_dir/$asset" ]]; then
    printf 'static docs asset source is missing expected asset: %s\n' "$asset" >&2
    exit 1
  fi
done

is_expected_asset() {
  local path="$1"
  local asset
  for asset in "${expected_assets[@]}"; do
    [[ "$asset" == "$path" ]] && return 0
  done
  return 1
}

while IFS= read -r -d '' path; do
  rel="${path#"$source_dir"/}"
  case "$rel" in
    .DS_Store|*/.DS_Store)
      continue
      ;;
  esac
  if ! is_expected_asset "$rel"; then
    printf 'static docs asset source has unexpected file: %s\n' "$rel" >&2
    exit 1
  fi
done < <(find "$source_dir" -mindepth 1 \( -type f -o -type l \) -print0)

tmp_root="$(mktemp -d)"
asset_repo="$tmp_root/assets-repo"

cleanup() {
  rm -rf "$tmp_root"
}
trap cleanup EXIT

mkdir -p "$asset_repo"
for asset in "${expected_assets[@]}"; do
  mkdir -p "$asset_repo/$(dirname "$asset")"
  cp "$source_dir/$asset" "$asset_repo/$asset"
done

git -C "$asset_repo" init --quiet
git -C "$asset_repo" add .
git -C "$asset_repo" \
  -c user.name="${GIT_AUTHOR_NAME:-agentsview docs bot}" \
  -c user.email="${GIT_AUTHOR_EMAIL:-docs-bot@example.invalid}" \
  commit -m "docs assets" >/dev/null
asset_commit="$(git -C "$asset_repo" rev-parse HEAD)"
git -C "$asset_repo" update-ref refs/heads/assets "$asset_commit"
git -C "$repo_root" fetch "$asset_repo" "+refs/heads/assets:refs/heads/$assets_branch" >/dev/null

printf 'Updated %s -> %s\n' "$assets_branch" "$asset_commit"

if [[ "$push" == true ]]; then
  git -C "$repo_root" push --force origin "$assets_branch"
fi
