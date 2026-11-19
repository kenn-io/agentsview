#!/usr/bin/env bash
# Prints a `go test -run` pattern that selects the tests exercising the macOS
# file watcher in the given packages. A test is selected when the file that
# declares it is named watch*_test.go or *_darwin_test.go, or when the file's
# build constraint names darwin. go list applies the build constraints of the
# host, so run this on macOS to get the macOS selection.
#
# Usage: scripts/macos-watcher-tests.sh <package>...
# Extra `go list` flags, such as build tags, come from GOFLAGS.
set -euo pipefail

if (($# == 0)); then
    echo "usage: $0 <package>..." >&2
    exit 2
fi

list_format='{{range .TestGoFiles}}{{$.Dir}}/{{.}}{{"\n"}}{{end}}'
list_format+='{{range .XTestGoFiles}}{{$.Dir}}/{{.}}{{"\n"}}{{end}}'

names=$(
    go list -f "$list_format" "$@" | while IFS= read -r file; do
        [[ -n $file ]] || continue
        base=${file##*/}
        [[ $base == watch*_test.go || $base == *_darwin_test.go ]] ||
            grep -q '^//go:build.*darwin' "$file" || continue
        sed -nE '/^func TestMain\(/d; s/^func (Test[A-Z0-9_][A-Za-z0-9_]*)\(.*/\1/p' "$file"
    done | LC_ALL=C sort -u
)

if [[ -z $names ]]; then
    echo "no macOS watcher tests found in: $*" >&2
    exit 1
fi
printf '^(%s)$\n' "$(printf '%s\n' "$names" | paste -s -d '|' -)"
