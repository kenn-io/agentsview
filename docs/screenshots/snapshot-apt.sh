#!/usr/bin/env bash
set -euo pipefail
snapshot="${1:?Debian snapshot timestamp required}"
[[ "$snapshot" =~ ^[0-9]{8}T[0-9]{6}Z$ ]] || exit 2
rm -f /etc/apt/sources.list.d/debian.sources
printf 'deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/%s bookworm main\ndeb [check-valid-until=no] https://snapshot.debian.org/archive/debian-security/%s bookworm-security main\n' \
  "$snapshot" "$snapshot" > /etc/apt/sources.list
apt-get update
