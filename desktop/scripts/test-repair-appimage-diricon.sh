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

mkdir -p "$dest/apprun-hooks"
printf 'icon\n' > "$dest/AgentsView.png"
printf '%s\n' \
  'export GTK_PATH="$APPDIR//usr/lib/gtk-3.0"' \
  "${TEST_HOOK_GDK_LINE-export GDK_BACKEND=x11 # Crash with Wayland backend on Wayland}" \
  'export GIO_EXTRA_MODULES="$APPDIR/usr/lib/gio/modules"' \
  > "$dest/apprun-hooks/linuxdeploy-plugin-gtk.sh"
EOF
chmod +x "$fake_bin/unsquashfs"

cat > "$fake_bin/mksquashfs" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

printf 'patched-rootfs\n' > "$2"
cp "$1/apprun-hooks/linuxdeploy-plugin-gtk.sh" "$TEST_HOOK_OUT"
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

cat > "$fake_bin/npx" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

if [ "$*" != "tauri signer sign $TEST_ARCHIVE" ]; then
  echo "unsupported npx invocation: $*" >&2
  exit 1
fi

cat <<'SIGNATURE'
Your file was signed successfully, You can find the signature here:
/tmp/AgentsView.AppImage.tar.gz.sig

Public signature:
YWJjCg==

Make sure to include this into the signature field of your update server.
SIGNATURE
EOF
chmod +x "$fake_bin/npx"

appimage="$tmp_root/AgentsView.AppImage"
archive="${appimage}.tar.gz"
signature="${archive}.sig"
printf 'runtime hsqs old-rootfs\n' > "$appimage"
chmod 0755 "$appimage"
printf 'stale archive\n' > "$archive"

hook_out="$tmp_root/hook.sh"

TEST_HOOK_OUT="$hook_out" \
TEST_ARCHIVE="$archive" \
TAURI_SIGNING_PRIVATE_KEY="test-key" \
PATH="$fake_bin:$PATH" \
  bash "$SCRIPT_DIR/repair-appimage-diricon.sh" "$appimage" >/dev/null

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

assert_eq "$(cat "$signature")" "YWJjCg==" \
  "updater signature contains only the base64 public signature"

# The hook must default to x11 yet honor a caller-provided GDK_BACKEND.
grep -q '^export GDK_BACKEND="${GDK_BACKEND:-x11}" # Crash' "$hook_out" || {
  echo "assertion failed: GDK_BACKEND hook not rewritten" >&2
  exit 1
}
if grep -q '^export GDK_BACKEND=x11' "$hook_out"; then
  echo "assertion failed: forced GDK_BACKEND=x11 still present" >&2
  exit 1
fi
# Execute the line the repair script actually emitted, not a copy of it.
gdk_line="$(grep '^export GDK_BACKEND=' "$hook_out")"
assert_eq "$(env -u GDK_BACKEND bash -c "$gdk_line"$'\n'"printf %s \"\$GDK_BACKEND\"")" "x11" \
  "unset GDK_BACKEND defaults to x11"
assert_eq "$(GDK_BACKEND=wayland bash -c "$gdk_line"$'\n'"printf %s \"\$GDK_BACKEND\"")" "wayland" \
  "caller GDK_BACKEND=wayland is preserved"

# A hook without the forced line means Tauri changed; fail instead of no-op.
# Use the same signing setup as the passing run so the failure can only come
# from the missing-line check, and assert on its specific error message.
printf 'runtime hsqs old-rootfs\n' > "$appimage"
missing_status=0
missing_err="$(TEST_HOOK_GDK_LINE='# no forced backend' \
  TEST_HOOK_OUT="$hook_out" \
  TEST_ARCHIVE="$archive" \
  TAURI_SIGNING_PRIVATE_KEY="test-key" \
  PATH="$fake_bin:$PATH" \
  bash "$SCRIPT_DIR/repair-appimage-diricon.sh" "$appimage" 2>&1 >/dev/null)" || missing_status=$?
if [ "$missing_status" -eq 0 ]; then
  echo "assertion failed: missing forced GDK_BACKEND line should fail" >&2
  exit 1
fi
case "$missing_err" in
  *"forced GDK_BACKEND=x11 not found"*) ;;
  *)
    echo "assertion failed: wrong failure for missing line: $missing_err" >&2
    exit 1
    ;;
esac
echo "repair-appimage-diricon updater archive checks passed"
