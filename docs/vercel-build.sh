#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
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
"$python_bin" "$script_dir/scripts/check_markdown_sources.py"
"$script_dir/assets/hydrate-assets.sh"
"$script_dir/zensical-docs.sh" build
