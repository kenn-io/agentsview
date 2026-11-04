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

# Name the top-level entries to copy instead of passing --exclude './name'.
# Both tars match --exclude patterns unanchored. GNU tar drops only the
# top-level './name' because only top-level members start with './'.
# libarchive's bsdtar strips the leading './' from the pattern and matches it
# at the start of any path element, so it also dropped nested paths such as
# assets/generated/screenshots/. Picking the top-level entries here gives both
# tars the same result. '--exclude ./*.go' stays on tar: '*' crosses '/' in
# both, so both drop .go files at any depth.
copy_entries=()
while IFS= read -r -d '' entry; do
  case "${entry#./}" in
    .venv | .vercel | .env*.local | site | zensical-public-docs.* | \
      .zensical-build.* | .ruff_cache | .mypy_cache | superpowers | \
      internal | agents | overrides | scripts | website | llms.txt | \
      screenshots | README.md | pyproject.toml | uv.lock | vercel.json | \
      vercel-build.sh | zensical-docs.sh | zensical.toml)
      ;;
    *)
      copy_entries+=("$entry")
      ;;
  esac
done < <(cd "$docs_root" && find . -mindepth 1 -maxdepth 1 -print0)
# Besides reporting an empty docs folder, this keeps bash 3.2 (macOS) from
# failing on an empty "${copy_entries[@]}" under set -u.
if [[ ${#copy_entries[@]} -eq 0 ]]; then
  printf 'no docs entries to copy from %s\n' "$docs_root" >&2
  exit 1
fi

(
  cd "$docs_root"
  tar \
    --exclude './*.go' \
    -cf - "${copy_entries[@]}"
) | (cd "$tmp_docs" && tar -xf -)

# Below the top-level assets/ only, drop everything named *.sh at any depth
# (files, links and directories), as GNU tar's './assets/*.sh' exclude did.
if [[ -d "$tmp_docs/assets" ]]; then
  find "$tmp_docs/assets" -name '*.sh' -prune -exec rm -rf -- {} +
fi

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
