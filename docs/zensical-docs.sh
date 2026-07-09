#!/usr/bin/env bash
set -euo pipefail

command_name="${1:-}"
if [[ "$command_name" != "build" && "$command_name" != "serve" ]]; then
  printf 'usage: %s {build|serve} [zensical args...]\n' "$0" >&2
  exit 2
fi
shift || true

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
docs_root="$script_dir"
site_dir="${AGENTSVIEW_DOCS_SITE_DIR:-site}"
site_output_dir="$docs_root/$site_dir"
case "$site_dir" in
  /*)
    site_output_dir="$site_dir"
    ;;
esac

if [[ -n "${VIRTUAL_ENV:-}" && -x "$VIRTUAL_ENV/bin/zensical" ]]; then
  zensical_bin="$VIRTUAL_ENV/bin/zensical"
elif [[ -x "$docs_root/.venv/bin/zensical" ]]; then
  zensical_bin="$docs_root/.venv/bin/zensical"
elif command -v zensical >/dev/null 2>&1; then
  zensical_bin="zensical"
else
  printf 'zensical not found; install with: cd docs && uv sync --frozen --no-dev\n' >&2
  exit 127
fi

tmp_docs=""
tmp_config_base=""
tmp_config=""

cleanup() {
  if [[ -n "$tmp_docs" ]]; then
    rm -rf "$tmp_docs"
  fi
  if [[ -n "$tmp_config" ]]; then
    rm -f "$tmp_config"
  fi
  if [[ -n "$tmp_config_base" ]]; then
    rm -f "$tmp_config_base"
  fi
}
trap cleanup EXIT INT TERM

tmp_docs_name="$(cd "$docs_root" && mktemp -d zensical-public-docs.XXXXXX)"
tmp_docs="$docs_root/$tmp_docs_name"
tmp_config_base_name="$(cd "$docs_root" && mktemp .zensical-build.XXXXXX)"
tmp_config_base="$docs_root/$tmp_config_base_name"
tmp_config="$tmp_config_base.toml"
tmp_config_name="$tmp_config_base_name.toml"
if [[ -e "$tmp_config" ]]; then
  printf 'temporary config path already exists: %s\n' "$tmp_config" >&2
  exit 1
fi
mv "$tmp_config_base" "$tmp_config"
tmp_config_base=""

(
  cd "$docs_root"
  tar \
    --exclude './.venv' \
    --exclude './.vercel' \
    --exclude './.env*.local' \
    --exclude './site' \
    --exclude './zensical-public-docs.*' \
    --exclude './.zensical-build.*' \
    --exclude './.ruff_cache' \
    --exclude './.mypy_cache' \
    --exclude './superpowers' \
    --exclude './internal' \
    --exclude './overrides' \
    --exclude './scripts' \
    --exclude './screenshots/Dockerfile' \
    --exclude './screenshots/README.md' \
    --exclude './screenshots/entrypoint.sh' \
    --exclude './screenshots/extract-db.sh' \
    --exclude './screenshots/node_modules' \
    --exclude './screenshots/package-lock.json' \
    --exclude './screenshots/package.json' \
    --exclude './screenshots/playwright.config.ts' \
    --exclude './screenshots/run.sh' \
    --exclude './screenshots/test-results' \
    --exclude './screenshots/tests/*' \
    --exclude './screenshots/update-generated-assets-branch.sh' \
    --exclude './README.md' \
    --exclude './pyproject.toml' \
    --exclude './uv.lock' \
    --exclude './vercel.json' \
    --exclude './vercel-build.sh' \
    --exclude './zensical-docs.sh' \
    --exclude './zensical.toml' \
    --exclude './assets/*.sh' \
    -cf - .
) | (cd "$tmp_docs" && tar -xf -)

copy_route_markdown_pages() {
  mkdir -p "$site_output_dir"
  find "$site_output_dir" -maxdepth 1 -type f -name '*.md' -delete
  find "$tmp_docs" -maxdepth 1 -type f -name '*.md' ! -name 'README.md' -print0 |
    while IFS= read -r -d '' source; do
      cp "$source" "$site_output_dir/$(basename "$source")"
    done
}

awk -v docs_dir="$tmp_docs_name" -v site_dir="$site_dir" '
  $0 == "docs_dir = \"docs\"" {
    print "docs_dir = \"" docs_dir "\""
    next
  }
  $0 == "site_dir = \"site\"" {
    print "site_dir = \"" site_dir "\""
    next
  }
  { print }
' "$docs_root/zensical.toml" > "$tmp_config"

case "$command_name" in
  build)
    (cd "$docs_root" && "$zensical_bin" build --strict --config-file "$tmp_config_name" "$@")
    copy_route_markdown_pages
    ;;
  serve)
    (cd "$docs_root" && "$zensical_bin" serve --config-file "$tmp_config_name" "$@")
    ;;
esac
