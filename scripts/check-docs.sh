#!/usr/bin/env bash
set -euo pipefail

failed=0

fail() {
  printf '%s\n' "$1" >&2
  failed=1
}

if [[ -e "zensical.toml" ]]; then
  fail 'Zensical config must live under docs/: zensical.toml'
fi

if [[ -e "vercel.json" ]]; then
  fail 'Vercel config must live under docs/: vercel.json'
fi

tracked_media="$(
  git ls-files docs 2>/dev/null | grep -E '\.(png|svg|jpg|jpeg|webp|gif)$' || true
)"
if [[ -n "$tracked_media" ]]; then
  printf 'docs image media must live in docs asset branches, not main:\n%s\n' "$tracked_media" >&2
  failed=1
fi

tracked_hydrated_assets="$(
  git ls-files docs/assets/static docs/assets/generated 2>/dev/null || true
)"
if [[ -n "$tracked_hydrated_assets" ]]; then
  printf 'hydrated docs assets must be ignored, not tracked:\n%s\n' "$tracked_hydrated_assets" >&2
  failed=1
fi

if [[ "$failed" -ne 0 ]]; then
  exit 1
fi

python_bin="${PYTHON:-}"
if [[ -z "$python_bin" ]]; then
  if command -v python3 >/dev/null 2>&1; then
    python_bin="python3"
  elif command -v python >/dev/null 2>&1; then
    python_bin="python"
  else
    printf 'python not found; cannot validate docs markdown sources\n' >&2
    exit 127
  fi
fi
"$python_bin" docs/scripts/check_markdown_sources.py

if ! command -v rg >/dev/null 2>&1; then
  printf 'rg not found; cannot validate docs media references\n' >&2
  exit 127
fi

root_media_refs="$(
  (rg -n '(<img[^>]+src="/|!\[[^]]*\]\(/)[^)" >]+\.(png|svg|jpg|jpeg|webp|gif)' docs README.md || true) \
    | grep -v '/assets/static/' \
    | grep -v '/assets/generated/' \
    || true
)"
if [[ -n "$root_media_refs" ]]; then
  printf 'docs media references must use /assets/static or /assets/generated:\n%s\n' "$root_media_refs" >&2
  exit 1
fi

source_media_refs="$(
  (rg -n '(/screenshots/[^)" '"'"'`>]+\.(png|svg|jpg|jpeg|webp|gif)|/agents/[^)" '"'"'`>]+\.(png|svg|jpg|jpeg|webp|gif)|/architecture\.svg|https://agentsview\.io/og-image\.png|/og-image\.png)' docs README.md || true) \
    | grep -v '/assets/static/' \
    | grep -v '/assets/generated/' \
    || true
)"
if [[ -n "$source_media_refs" ]]; then
  printf 'docs source media references must use /assets/static or /assets/generated:\n%s\n' "$source_media_refs" >&2
  exit 1
fi

bash docs/assets/hydrate-assets.sh

if command -v uv >/dev/null 2>&1; then
  (
    cd docs
    uv run --frozen bash ./zensical-docs.sh build
    uv run --frozen python scripts/check_built_site.py
    uv run --frozen python scripts/check_vercel_redirects.py
  )
else
  printf 'uv not found; skipping docs build validation\n' >&2
fi
