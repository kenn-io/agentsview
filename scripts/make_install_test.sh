#!/bin/bash
# Behavioral tests for the Makefile install recipe.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

home="$tmpdir/home"
work="$tmpdir/work"
fakebin="$tmpdir/bin"
install_dir="$home/.local/bin"
mkdir -p "$install_dir" "$work" "$fakebin"
printf 'old binary\n' > "$install_dir/agentsview"
printf 'new binary\n' > "$work/agentsview"
cat > "$fakebin/go" <<'EOF'
#!/bin/sh
if [ "$1" = "env" ] && [ "$2" = "GOPATH" ]; then
    printf '%s\n' "$HOME/go"
    exit 0
fi
if [ "$1" = "env" ] && [ "$2" = "GOBIN" ]; then
    exit 0
fi
echo "unexpected go command: $*" >&2
exit 1
EOF
chmod +x "$fakebin/go"

recipe="$(
    PATH="$fakebin:$PATH" HOME="$home" make -C "$REPO_ROOT" -n install |
        sed -n '/^if \[ -d /,$p'
)"

[ -n "$recipe" ] || fail "could not extract install recipe"

cp() {
    printf 'partial binary\n' > "$2"
    return 1
}
export -f cp

set +e
(
    cd "$work" || exit 1
    eval "$recipe"
)
status=$?
set -e

[ "$status" -ne 0 ] || fail "install recipe succeeded after cp failed"

installed="$(cat "$install_dir/agentsview")"
[ "$installed" = "old binary" ] ||
    fail "failed copy replaced installed binary with: $installed"

leftovers="$(find "$install_dir" -maxdepth 1 -type f -name 'agentsview.*' -print)"
[ -z "$leftovers" ] || fail "temporary install files were not cleaned up: $leftovers"

echo "PASS: install recipe keeps existing binary when copy fails"
