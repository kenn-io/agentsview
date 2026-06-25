#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

assert_eq() {
  local got="$1"
  local want="$2"
  local msg="$3"
  if [ "$got" != "$want" ]; then
    echo "assertion failed: $msg (got='$got' want='$want')" >&2
    exit 1
  fi
}

tmp_root="$(mktemp -d)"
cleanup() {
  rm -rf "$tmp_root"
}
trap cleanup EXIT

fake_bin="$tmp_root/bin"
mkdir -p "$fake_bin"

cat > "$fake_bin/unsquashfs" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

if [ "${1:-}" = "-s" ]; then
  exit 0
fi

dest=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -d)
      dest="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done

if [ -z "$dest" ]; then
  echo "missing -d destination" >&2
  exit 1
fi

mkdir -p "$dest"
printf 'icon\n' > "$dest/AgentsView.png"
EOF
chmod +x "$fake_bin/unsquashfs"

cat > "$fake_bin/mksquashfs" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

printf 'patched-rootfs\n' > "$2"
EOF
chmod +x "$fake_bin/mksquashfs"

cat > "$fake_bin/stat" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -eq 3 ] && [ "$1" = "-c" ] && [ "$2" = "%a" ]; then
  printf '755\n'
  exit 0
fi

echo "unsupported stat invocation: $*" >&2
exit 1
EOF
chmod +x "$fake_bin/stat"

appimage="$tmp_root/AgentsView.AppImage"
archive="${appimage}.tar.gz"
printf 'runtime hsqs old-rootfs\n' > "$appimage"
chmod 0755 "$appimage"
printf 'stale archive\n' > "$archive"

PATH="$fake_bin:$PATH" bash "$SCRIPT_DIR/repair-appimage-diricon.sh" "$appimage" >/dev/null

archive_listing="$(tar -tzf "$archive")"
assert_eq "$archive_listing" "AgentsView.AppImage" \
  "updater archive contains the repaired AppImage"

extracted_dir="$tmp_root/extracted"
mkdir -p "$extracted_dir"
tar -xzf "$archive" -C "$extracted_dir"
assert_eq "$(cat "$extracted_dir/AgentsView.AppImage")" "$(cat "$appimage")" \
  "updater archive contains current AppImage bytes"

archived_mode="$(tar -tvzf "$archive" | awk '{print $1}')"
assert_eq "$archived_mode" "-rwxr-xr-x" \
  "updater archive preserves executable AppImage mode"

echo "repair-appimage-diricon updater archive checks passed"
