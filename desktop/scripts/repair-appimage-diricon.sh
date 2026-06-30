#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -eq 0 ]; then
  echo "usage: $0 <appimage>..." >&2
  exit 2
fi

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "error: $1 is required to repair AppImage .DirIcon" >&2
    exit 1
  fi
}

require_cmd grep
require_cmd head
require_cmd mksquashfs
require_cmd tar
require_cmd unsquashfs

normalize_signature_file() {
  local signature="$1"
  local tmp="${signature}.tmp"

  awk '
    /^Public signature:$/ {
      getline
      print
      found = 1
      exit
    }
    END {
      if (!found) {
        exit 2
      }
    }
  ' "$signature" > "$tmp" || cp "$signature" "$tmp"

  tr -d '[:space:]' < "$tmp" > "$signature"
  rm -f "$tmp"

  if ! grep -Eq '^[A-Za-z0-9+/]+={0,2}$' "$signature"; then
    echo "error: signer output for $signature is not a base64 signature payload" >&2
    exit 1
  fi

  local len
  len="$(wc -c < "$signature" | tr -d '[:space:]')"
  if [ $((len % 4)) -ne 0 ]; then
    echo "error: signer output for $signature has invalid base64 length" >&2
    exit 1
  fi

  printf '\n' >> "$signature"
}

find_squashfs_offset() {
  local appimage="$1"
  local candidate

  while IFS=: read -r candidate _; do
    if unsquashfs -s -o "$candidate" "$appimage" >/dev/null 2>&1; then
      echo "$candidate"
      return 0
    fi
  done < <(LC_ALL=C grep -abo 'hsqs' "$appimage" || true)

  echo "error: could not find SquashFS payload in $appimage" >&2
  return 1
}

refresh_updater_archive() {
  local appimage="$1"
  local archive="${appimage}.tar.gz"
  local signature="${archive}.sig"
  local appimage_dir appimage_name

  if [ ! -f "$archive" ]; then
    return 0
  fi

  appimage_dir="$(dirname "$appimage")"
  appimage_name="$(basename "$appimage")"
  tar -C "$appimage_dir" -czf "$archive" "$appimage_name"

  if [ -n "${TAURI_SIGNING_PRIVATE_KEY:-}" ] || [ -n "${TAURI_SIGNING_PRIVATE_KEY_PATH:-}" ]; then
    npx tauri signer sign "$archive" > "$signature"
    normalize_signature_file "$signature"
  elif [ -f "$signature" ]; then
    echo "error: refreshed $archive but cannot refresh $signature without a Tauri signing key" >&2
    exit 1
  fi
}

repair_one() {
  local appimage="$1"

  if [ ! -f "$appimage" ]; then
    echo "error: AppImage not found: $appimage" >&2
    exit 1
  fi

  local offset tmp appdir runtime rootfs patched mode
  offset="$(find_squashfs_offset "$appimage")"
  tmp="$(mktemp -d)"
  appdir="$tmp/AppDir"
  runtime="$tmp/runtime"
  rootfs="$tmp/rootfs.squashfs"
  patched="$tmp/patched.AppImage"

  cleanup() {
    rm -rf "$tmp"
  }
  trap cleanup RETURN

  unsquashfs -q -d "$appdir" -o "$offset" "$appimage" >/dev/null

  if [ ! -f "$appdir/AgentsView.png" ]; then
    echo "error: root AgentsView.png missing in $appimage" >&2
    exit 1
  fi

  rm -f "$appdir/.DirIcon"
  cp "$appdir/AgentsView.png" "$appdir/.DirIcon"
  chmod 0644 "$appdir/.DirIcon"

  head -c "$offset" "$appimage" > "$runtime"
  mksquashfs "$appdir" "$rootfs" -noappend -quiet >/dev/null
  cat "$runtime" "$rootfs" > "$patched"

  mode="$(stat -c '%a' "$appimage")"
  chmod "$mode" "$patched"
  mv "$patched" "$appimage"

  refresh_updater_archive "$appimage"
  echo "Repaired AppImage .DirIcon: $appimage"
}

for appimage in "$@"; do
  repair_one "$appimage"
done
