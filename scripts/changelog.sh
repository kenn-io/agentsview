#!/bin/bash
# Generate a changelog since the last release using an AI agent
# Usage: ./scripts/changelog.sh [version] [start_tag] [extra_instructions]
# Set CHANGELOG_AGENT=claude to use claude instead of codex (default)
# If version is not provided, uses "NEXT" as placeholder
# If start_tag is "-" or empty, auto-detects the previous tag

set -euo pipefail

VERSION="${1:-NEXT}"
START_TAG="${2:-}"
EXTRA_INSTRUCTIONS="${3:-}"
AGENT="${CHANGELOG_AGENT:-codex}"

# Determine the starting point
if [ -n "$START_TAG" ] && [ "$START_TAG" != "-" ]; then
    RANGE="$START_TAG..HEAD"
    echo "Generating changelog from $START_TAG to HEAD..." >&2
else
    PREV_TAG=$(git describe --tags --abbrev=0 2>/dev/null || echo "")
    if [ -z "$PREV_TAG" ]; then
        RANGE=""
        echo "No previous release found. Generating changelog for all commits..." >&2
    else
        RANGE="$PREV_TAG..HEAD"
        echo "Generating changelog from $PREV_TAG to HEAD..." >&2
    fi
fi

if [ -n "$RANGE" ]; then
    COMMITS=$(git log "$RANGE" --pretty=format:"- %s (%h)" --no-merges)
    DIFF_STAT=$(git diff --stat "$RANGE")
else
    COMMITS=$(git log --pretty=format:"- %s (%h)" --no-merges)
    EMPTY_TREE=$(git hash-object -t tree /dev/null)
    DIFF_STAT=$(git diff --stat "$EMPTY_TREE" HEAD)
fi

if [ -z "$COMMITS" ]; then
    echo "No commits since last release" >&2
    exit 0
fi

echo "Using $AGENT to generate changelog..." >&2

TMPFILE=$(mktemp)
PROMPTFILE=$(mktemp)
ERRFILE=$(mktemp)
trap 'rm -f "$TMPFILE" "$PROMPTFILE" "$ERRFILE"' EXIT

cat > "$PROMPTFILE" <<EOF
You are generating a changelog for agentsview version $VERSION.

IMPORTANT: Do NOT use any tools. Do NOT run any shell commands. Do NOT search or read any files.
All the information you need is provided below. Simply analyze the commit messages and output the changelog.

Here are the commits since the last release:
$COMMITS

Here is the diff summary:
$DIFF_STAT

Please generate a concise, user-focused changelog. Group changes into sections like:
- New Features
- Improvements
- Bug Fixes

Focus on user-visible changes. Skip internal refactoring unless it affects users.
Do NOT mention documentation-only changes, including docs, changelog, release
note, README, website copy, screenshot, or generated docs asset updates, unless
the documentation is the user-facing product being released.
Keep descriptions brief (one line each). Use present tense.
Do NOT mention bugs that were introduced and fixed within this same release cycle.
${EXTRA_INSTRUCTIONS:+

When writing the changelog, look for these features or improvements in the commit log above: $EXTRA_INSTRUCTIONS
Do NOT search files, read code, or do any analysis outside of the commit log provided above.}
Output ONLY the changelog content, no preamble.
EOF

AGENT_EXIT=0
case "$AGENT" in
    codex)
        CODEX_RUST_LOG="${CHANGELOG_CODEX_RUST_LOG:-${RUST_LOG:-error,codex_core::rollout::list=off}}"
        set +e
        RUST_LOG="$CODEX_RUST_LOG" codex exec --json --skip-git-repo-check --sandbox read-only -c reasoning_effort=high -o "$TMPFILE" - >/dev/null < "$PROMPTFILE" 2>"$ERRFILE"
        AGENT_EXIT=$?
        set -e
        ;;
    claude)
        set +e
        claude --print < "$PROMPTFILE" > "$TMPFILE" 2>"$ERRFILE"
        AGENT_EXIT=$?
        set -e
        ;;
    *)
        echo "Error: unknown CHANGELOG_AGENT '$AGENT' (expected 'codex' or 'claude')" >&2
        exit 1
        ;;
esac

if [ "$AGENT_EXIT" -ne 0 ] || [ ! -s "$TMPFILE" ]; then
    echo "Error: $AGENT failed to generate changelog." >&2
    if [ "${CHANGELOG_DEBUG:-0}" = "1" ]; then
        cat "$ERRFILE" >&2
    else
        FILTERED_ERR=$(grep -E -v 'rollout path for thread|failed to record rollout items: failed to queue rollout items: channel closed|^mcp startup: no servers$|^WARNING: proceeding, even though we could not update PATH:' "$ERRFILE" || true)
        if [ -n "$FILTERED_ERR" ]; then
            echo "$FILTERED_ERR" >&2
        else
            echo "Set CHANGELOG_DEBUG=1 to print full agent logs." >&2
        fi
    fi
    exit 1
fi

if [ "${CHANGELOG_DEBUG:-0}" = "1" ] && [ -s "$ERRFILE" ]; then
    cat "$ERRFILE" >&2
fi

cat "$TMPFILE"
