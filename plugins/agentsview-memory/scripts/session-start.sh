#!/bin/sh

if ! command -v agentsview >/dev/null 2>&1; then
  echo "agentsview memory hook: agentsview is not installed or not on PATH" >&2
  exit 0
fi

plugin_root=${PLUGIN_ROOT:-${CLAUDE_PLUGIN_ROOT:-}}
if [ -n "$plugin_root" ]; then
  exec agentsview memory session-start --hook --plugin-root "$plugin_root"
fi
exec agentsview memory session-start --hook
