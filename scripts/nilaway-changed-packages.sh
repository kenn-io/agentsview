#!/bin/bash
# Run NilAway only on packages containing changed Go files.
set -euo pipefail

NILAWAY_INCLUDE_PKGS="${NILAWAY_INCLUDE_PKGS:-github.com/wesm/agentsview}"
export GOFLAGS="${GOFLAGS:+$GOFLAGS }-buildvcs=false"

tmp_dirs="$(mktemp)"
trap 'rm -f "$tmp_dirs"' EXIT

for file in "$@"; do
    case "$file" in
        *.go)
            if [ -f "$file" ]; then
                dir="$(dirname "$file")"
                if [ "$dir" = "." ]; then
                    printf '%s\n' "." >> "$tmp_dirs"
                elif [[ "$dir" = ./* ]]; then
                    printf '%s\n' "$dir" >> "$tmp_dirs"
                else
                    printf '%s\n' "./$dir" >> "$tmp_dirs"
                fi
            fi
            ;;
    esac
done

if [ ! -s "$tmp_dirs" ]; then
    echo "nilaway: no changed Go files"
    exit 0
fi

if ! command -v go >/dev/null 2>&1; then
    echo "go not found" >&2
    exit 1
fi
if ! command -v nilaway >/dev/null 2>&1; then
    echo "nilaway not found. Install with: make lint-tools" >&2
    exit 1
fi

dirs=()
while IFS= read -r dir; do
    dirs+=("$dir")
done < <(sort -u "$tmp_dirs")
go list -f '{{.ImportPath}}' "${dirs[@]}" >/dev/null
packages=("${dirs[@]}")

if [ "${#packages[@]}" -eq 0 ]; then
    echo "nilaway: no changed Go packages"
    exit 0
fi

nilaway -test=false -include-pkgs="$NILAWAY_INCLUDE_PKGS" "${packages[@]}"
