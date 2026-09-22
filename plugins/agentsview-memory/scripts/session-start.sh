#!/bin/sh

# Trust model: a repository can influence this hook's PATH and generic
# environment through project settings, so neither is trusted inside one.
# The user registers the absolute binary path by running
# `agentsview memory session-start` once from a trusted directory; that
# record lives in the default data directory — the fixed path this hook
# reads, ignoring overrides a repository could also set. Without a record,
# the hook runs only outside a repository and fails closed inside one, and
# a registered binary that itself lives inside a repository is refused.
#
# Repository detection uses an absolute git helper with a sanitized PATH:
# both the helper and what it inspects must not come from the repository
# being probed. Record reads use the shell's built-in read, not PATH
# utilities.

record="$HOME/.agentsview/registered-bin"
repo_root=$(
    PATH=/usr/bin:/bin git -C "$PWD" rev-parse --show-toplevel 2>/dev/null ||
        true
)

bin=""
if [ -r "$record" ]; then
  IFS= read -r bin < "$record"
fi

# A registered binary that itself lives inside a git repository is
# repository-controlled no matter where the hook runs. The binary's own
# directory is derived with parameter expansion so no PATH utility runs
# before trust is established.
registered_inside_repo=false
if [ -n "$bin" ]; then
  bin_dir=${bin%/*}
  [ "$bin_dir" = "$bin" ] && bin_dir=.
  if PATH=/usr/bin:/bin git -C "$bin_dir" rev-parse --show-toplevel >/dev/null 2>&1; then
    registered_inside_repo=true
  fi
fi

if [ -n "$bin" ] && [ -x "$bin" ] && [ "$registered_inside_repo" = false ]; then
  if [ -n "$CLAUDE_PLUGIN_ROOT" ]; then
    exec "$bin" memory session-start --hook --plugin-root "$CLAUDE_PLUGIN_ROOT"
  fi
  exec "$bin" memory session-start --hook
fi

if [ -n "$bin" ] && [ "$registered_inside_repo" = true ]; then
  echo "agentsview memory hook: refusing registered binary inside a repository at $bin; re-register from a trusted directory" >&2
fi
if [ ! -r "$record" ] || [ ! -s "$record" ]; then
  echo "agentsview memory hook: no registered binary path; run 'agentsview memory session-start' once from a trusted directory to register it" >&2
fi
if [ -n "$bin" ] && [ ! -x "$bin" ]; then
  echo "agentsview memory hook: registered binary '$bin' is missing; run 'agentsview memory session-start' once to re-register" >&2
fi

if [ -n "$repo_root" ]; then
  exit 0
fi

# Outside any repository this hook inherits the user's own environment, so
# PATH resolution is the documented install contract — but a stale export
# (direnv and similar) can still point at a repository checkout. Refuse a
# binary that lives inside any git repository; genuine installs do not.
if ! command -v agentsview >/dev/null 2>&1; then
  echo "agentsview memory hook: agentsview is not installed or not on PATH" >&2
  exit 0
fi
agentsview_bin=$(command -v agentsview)
if git -C "$(dirname "$agentsview_bin")" rev-parse --show-toplevel >/dev/null 2>&1; then
  echo "agentsview memory hook: refusing agentsview from a repository at $agentsview_bin" >&2
  exit 0
fi
if [ -n "$CLAUDE_PLUGIN_ROOT" ]; then
  exec agentsview memory session-start --hook --plugin-root "$CLAUDE_PLUGIN_ROOT"
fi
exec agentsview memory session-start --hook
