#!/usr/bin/env bash
set -euo pipefail

command_name="${1:-}"
if [[ "$command_name" != "build" && "$command_name" != "serve" ]]; then
  printf 'usage: %s {build|serve} [zensical args...]\n' "$0" >&2
  exit 2
fi
shift || true

# Physical path so the output-directory guard compares canonical paths.
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
docs_root="$script_dir"
site_dir="${AGENTSVIEW_DOCS_SITE_DIR:-site}"
site_output_dir="$docs_root/$site_dir"
case "$site_dir" in
  /*)
    site_output_dir="$site_dir"
    ;;
esac

resolve_output_dir() {
  local path="$1"
  if [[ -d "$path" ]]; then
    (cd "$path" && pwd -P)
    return
  fi
  local parent
  parent="$(dirname "$path")"
  if [[ ! -d "$parent" ]]; then
    printf 'site output parent directory does not exist: %s\n' "$parent" >&2
    return 1
  fi
  printf '%s/%s\n' "$(cd "$parent" && pwd -P)" "$(basename "$path")"
}

# The build deletes the output directory before rendering, so refuse any
# path that resolves to the docs sources or one of their ancestors.
site_output_dir="$(resolve_output_dir "$site_output_dir")"
if [[ "$site_output_dir" == "/" || "$docs_root/" == "$site_output_dir/"* ]]; then
  printf 'refusing site output directory %s: it contains the docs sources\n' \
    "$site_output_dir" >&2
  exit 2
fi

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
    --exclude './agents' \
    --exclude './overrides' \
    --exclude './scripts' \
    --exclude './website' \
    --exclude './llms.txt' \
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

# Docs render under /docs/; the hand-written website tier owns the site root.
docs_output_dir="$site_output_dir/docs"

copy_route_markdown_pages() {
  mkdir -p "$docs_output_dir"
  find "$docs_output_dir" -maxdepth 1 -type f -name '*.md' -delete
  find "$tmp_docs" -maxdepth 1 -type f -name '*.md' ! -name 'README.md' -print0 |
    while IFS= read -r -d '' source; do
      cp "$source" "$docs_output_dir/$(basename "$source")"
    done
}

assemble_site_root() {
  mkdir -p "$site_output_dir"

  local entry
  for entry in index.html index.md guide guide.md 404.html favicon.svg \
    styles scripts fonts; do
    if [[ ! -e "$docs_root/website/$entry" ]]; then
      printf 'missing website tier entry: %s\n' "$docs_root/website/$entry" >&2
      exit 1
    fi
    cp -R "$docs_root/website/$entry" "$site_output_dir/$entry"
  done

  if [[ ! -s "$docs_root/llms.txt" ]]; then
    printf 'missing docs/llms.txt\n' >&2
    exit 1
  fi
  cp "$docs_root/llms.txt" "$site_output_dir/llms.txt"

  # Crawlers expect the sitemap at the origin root; Zensical's instant
  # navigation also fetches /docs/sitemap.xml, so keep both copies.
  local sitemap
  for sitemap in sitemap.xml sitemap.xml.gz; do
    if [[ -f "$docs_output_dir/$sitemap" ]]; then
      cp "$docs_output_dir/$sitemap" "$site_output_dir/$sitemap"
    fi
  done
}

awk -v docs_dir="$tmp_docs_name" -v site_dir="$site_dir/docs" '
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
    # Zensical only cleans its own site/docs subtree; clear the whole output
    # directory so files from earlier builds or layouts never ship.
    rm -rf "$site_output_dir"
    (cd "$docs_root" && "$zensical_bin" build --strict --config-file "$tmp_config_name" "$@")
    copy_route_markdown_pages
    assemble_site_root
    ;;
  serve)
    (cd "$docs_root" && "$zensical_bin" serve --config-file "$tmp_config_name" "$@")
    ;;
esac
