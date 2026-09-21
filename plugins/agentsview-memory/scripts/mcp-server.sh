#!/bin/sh
# Shared by the plugin's entry-point scripts: resolve the AgentsView binary
# for MCP server launch. The user's registered absolute path (recorded by
# `agentsview memory session-start` in the default data directory) wins;
# otherwise the documented PATH install contract applies, because the MCP
# server is launched by the client in the user's own environment and must
# keep working everywhere the plugin is used.

agentsview_bin=""
registered="${HOME:-}/.agentsview/registered-bin"
if [ -r "$registered" ]; then
  IFS= read -r agentsview_bin < "$registered"
fi
if [ -n "$agentsview_bin" ] && [ -x "$agentsview_bin" ]; then
  exec "$agentsview_bin" mcp --profile memory "$@"
fi

exec agentsview mcp --profile memory "$@"
