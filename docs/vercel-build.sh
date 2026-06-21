#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
"$script_dir/assets/hydrate-assets.sh"
"$script_dir/zensical-docs.sh" build
