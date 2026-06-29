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

render_install_recipe() {
    PATH="$fakebin:$PATH" HOME="$1" make -C "$REPO_ROOT" -n install |
        sed -n '/^if \[ -d /,$p'
}

recipe="$(render_install_recipe "$home")"

[ -n "$recipe" ] || fail "could not extract install recipe"

set +e
(
    cd "$work" || exit 1
    cp() {
        printf 'partial binary\n' > "$2"
        return 1
    }
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

success_home="$tmpdir/success-home"
success_work="$tmpdir/success-work"
success_install_dir="$success_home/.local/bin"
mkdir -p "$success_install_dir" "$success_work"
printf 'old binary\n' > "$success_install_dir/agentsview"
printf 'new binary\n' > "$success_work/agentsview"
chmod 755 "$success_work/agentsview"

success_recipe="$(render_install_recipe "$success_home")"
[ -n "$success_recipe" ] || fail "could not extract success install recipe"

(
    cd "$success_work" || exit 1
    eval "$success_recipe"
)

success_installed="$(cat "$success_install_dir/agentsview")"
[ "$success_installed" = "new binary" ] ||
    fail "successful install wrote unexpected content: $success_installed"

[ -x "$success_install_dir/agentsview" ] ||
    fail "successful install did not leave agentsview executable"

success_leftovers="$(find "$success_install_dir" -maxdepth 1 -type f -name 'agentsview.*' -print)"
[ -z "$success_leftovers" ] ||
    fail "successful install left temporary files: $success_leftovers"

echo "PASS: install recipe keeps installed binary executable"
