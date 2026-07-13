#!/usr/bin/env bash
set -euo pipefail

# Extract only screenshot-safe project sessions from the source
# database. This runs on the host before the Docker build so the
# build context only contains the reduced screenshot database.

SOURCE="${1:-${SOURCE:-/data/source.db}}"
OUTPUT="${2:-${OUTPUT:-/data/test-sessions.db}}"
HISTORY_DAYS="${SCREENSHOT_HISTORY_DAYS:-60}"
DEFAULT_BLOCKED_TERMS_FILE="$HOME/.config/agentsview-docs/screenshot-blocked-terms.txt"
BLOCKED_TERMS_FILE="${SCREENSHOT_BLOCKED_TERMS_FILE:-$DEFAULT_BLOCKED_TERMS_FILE}"
PRIVATE_TERMS_FILE="${KENN_PRIVATE_TERMS_FILE:-$HOME/.config/kenn/private-terms.txt}"
HOME_PATH="${HOME:-}"
BLOCKED_TERMS="${SCREENSHOT_BLOCKED_TERMS:-}"
BLOCKED_PATTERNS_SQL="$(mktemp "${TMPDIR:-/tmp}/agentsview-screenshot-blocked-patterns.XXXXXX")"
trap 'rm -f "$BLOCKED_PATTERNS_SQL"' EXIT

append_blocked_terms() {
  local terms="$1"
  if [ -z "$terms" ]; then
    return
  fi

  if [ -n "$BLOCKED_TERMS" ]; then
    BLOCKED_TERMS="$BLOCKED_TERMS"$'\n'"$terms"
  else
    BLOCKED_TERMS="$terms"
  fi
}

append_blocked_terms_file() {
  local terms_file="$1"
  if [ ! -f "$terms_file" ]; then
    return
  fi

  append_blocked_terms "$(<"$terms_file")"
}

append_blocked_terms_file "$BLOCKED_TERMS_FILE"
append_blocked_terms_file "$PRIVATE_TERMS_FILE"

if [[ ! "$HISTORY_DAYS" =~ ^[0-9]+$ ]] || [ "$HISTORY_DAYS" -lt 1 ]; then
  echo "Error: SCREENSHOT_HISTORY_DAYS must be a positive integer"
  exit 1
fi

echo "Extracting screenshot-safe projects from source database..."
echo "Keeping sessions from the newest $HISTORY_DAYS-day window."

if ! command -v sqlite3 >/dev/null 2>&1; then
  echo "Error: sqlite3 is required to prepare the screenshot database"
  exit 1
fi

if [ ! -f "$SOURCE" ]; then
  echo "Error: source database not found at $SOURCE"
  exit 1
fi

if [ "$SOURCE" = "$OUTPUT" ]; then
  echo "Error: source and output database paths must differ"
  exit 1
fi

mkdir -p "$(dirname "$OUTPUT")"
rm -f "$OUTPUT"

{
  echo "CREATE TEMP TABLE screenshot_redactions(from_text TEXT PRIMARY KEY, to_text TEXT NOT NULL);"
  if [ -n "$HOME_PATH" ] && [ "$HOME_PATH" != "/" ]; then
    home_sql="${HOME_PATH//\'/\'\'}"
    printf "INSERT OR IGNORE INTO screenshot_redactions(from_text, to_text) VALUES ('%s', '~');\n" \
      "$home_sql"
  fi

  echo "CREATE TEMP TABLE screenshot_blocked_patterns(pattern TEXT PRIMARY KEY);"
  while IFS= read -r term || [ -n "$term" ]; do
    trimmed="$(printf '%s' "$term" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')"
    if [ -z "$trimmed" ]; then
      continue
    fi
    # Skip comment lines so a terms file can document itself. Without this a
    # bare '#' becomes the pattern '%#%', which matches almost every session.
    case "$trimmed" in
      '#'*) continue ;;
    esac

    pattern="$(printf '%s' "$trimmed" | tr '[:upper:]' '[:lower:]')"
    pattern="${pattern//\\/\\\\}"
    pattern="${pattern//%/\\%}"
    pattern="${pattern//_/\\_}"
    pattern="%$pattern%"
    sql_pattern="${pattern//\'/\'\'}"

    printf "INSERT OR IGNORE INTO screenshot_blocked_patterns(pattern) VALUES ('%s');\n" \
      "$sql_pattern"
  done <<<"$BLOCKED_TERMS"
} >"$BLOCKED_PATTERNS_SQL"

# Snapshot the full database with VACUUM INTO. A plain cp of a
# WAL-mode database reads a torn image and misses committed data still
# in the -wal file, surfacing as "database disk image is malformed".
# The online backup API (.backup) avoids that but restarts from
# scratch every time a live `agentsview serve` writes through its own
# connection, so it never converges against a busy daemon. VACUUM INTO
# reads one snapshot-isolated pass that concurrent writes do not
# restart, and does not touch the source. The target must not exist
# (rm -f above guarantees that).
sqlite3 "$SOURCE" "VACUUM INTO '$OUTPUT'"

# Delete sessions (and related data) for non-matching projects.
# The heredoc delimiter is quoted so bash does NOT expand $ or
# backticks inside — the embedded markdown contains both, and
# unquoted expansion would run file paths as commands.
sqlite3 "$OUTPUT" \
  -cmd ".parameter init" \
  -cmd ".parameter set @history_days $HISTORY_DAYS" \
  -cmd ".read \"$BLOCKED_PATTERNS_SQL\"" <<'SQL'
CREATE TEMP TABLE screenshot_projects(name TEXT PRIMARY KEY);
INSERT INTO screenshot_projects(name) VALUES
  ('agentsview'),
  ('agentsview_docs'),
  ('conbench'),
  ('kata'),
  ('roborev'),
  ('roborev_docs');

CREATE TEMP TABLE screenshot_safe_sessions(id TEXT PRIMARY KEY);
CREATE TEMP TABLE screenshot_root_sessions(id TEXT PRIMARY KEY);
CREATE TEMP TABLE screenshot_sessions(id TEXT PRIMARY KEY);
CREATE TEMP TABLE screenshot_bounds(
  cutoff_session_at TEXT,
  newest_session_at TEXT
);

INSERT INTO screenshot_safe_sessions(id)
SELECT id
FROM sessions s
WHERE s.project IN (SELECT name FROM screenshot_projects)
  AND s.message_count > 0
  AND s.deleted_at IS NULL
  AND NOT EXISTS (
    SELECT 1
    FROM screenshot_blocked_patterns p
    WHERE lower(
      COALESCE(s.first_message, '') || char(10) ||
      COALESCE(s.display_name, '') || char(10) ||
      COALESCE(s.session_name, '') || char(10) ||
      COALESCE(s.cwd, '') || char(10) ||
      COALESCE(s.file_path, '') || char(10) ||
      COALESCE(s.git_branch, '')
    ) LIKE p.pattern ESCAPE '\'
  )
  AND NOT EXISTS (
    SELECT 1
    FROM messages m
    JOIN screenshot_blocked_patterns p
      ON lower(
        COALESCE(m.content, '') || char(10) ||
        COALESCE(m.thinking_text, '')
      ) LIKE p.pattern ESCAPE '\'
    WHERE m.session_id = s.id
  )
  AND NOT EXISTS (
    SELECT 1
    FROM tool_calls tc
    JOIN screenshot_blocked_patterns p
      ON lower(
        COALESCE(tc.input_json, '') || char(10) ||
        COALESCE(tc.result_content, '') || char(10) ||
        COALESCE(tc.file_path, '') || char(10) ||
        COALESCE(tc.skill_name, '')
      ) LIKE p.pattern ESCAPE '\'
    WHERE tc.session_id = s.id
  )
  AND NOT EXISTS (
    SELECT 1
    FROM tool_result_events tre
    JOIN screenshot_blocked_patterns p
      ON lower(COALESCE(tre.content, '')) LIKE p.pattern ESCAPE '\'
    WHERE tre.session_id = s.id
  );

INSERT INTO screenshot_bounds(cutoff_session_at, newest_session_at)
SELECT
  datetime(max(session_at), printf('-%d days', @history_days)),
  max(session_at)
FROM (
  SELECT datetime(COALESCE(NULLIF(s.started_at, ''), s.created_at)) AS session_at
  FROM sessions s
  JOIN screenshot_safe_sessions safe ON safe.id = s.id
  WHERE COALESCE(s.parent_session_id, '') = ''
    AND s.relationship_type NOT IN ('subagent', 'fork', 'continuation')
);

INSERT INTO screenshot_root_sessions(id)
SELECT s.id
FROM sessions s
JOIN screenshot_safe_sessions safe ON safe.id = s.id
CROSS JOIN screenshot_bounds b
WHERE COALESCE(s.parent_session_id, '') = ''
  AND s.relationship_type NOT IN ('subagent', 'fork', 'continuation')
  AND datetime(COALESCE(NULLIF(s.started_at, ''), s.created_at)) >= b.cutoff_session_at;

WITH RECURSIVE screenshot_tree(id) AS (
  SELECT id FROM screenshot_root_sessions
  UNION
  SELECT child.id
  FROM sessions child
  JOIN screenshot_tree parent ON child.parent_session_id = parent.id
  JOIN screenshot_safe_sessions safe ON safe.id = child.id
)
INSERT INTO screenshot_sessions(id)
SELECT id FROM screenshot_tree;

-- Thinking-block screenshots navigate directly to one rich session because
-- thinking messages are rare and often older than the recent sidebar window.
-- Prefer a human root for this direct navigation fixture, while keeping
-- automated sessions available for usage/activity categorization screenshots.
INSERT OR IGNORE INTO screenshot_sessions(id)
SELECT s.id
FROM sessions s
JOIN screenshot_safe_sessions safe ON safe.id = s.id
WHERE COALESCE(s.parent_session_id, '') = ''
  AND s.relationship_type NOT IN ('subagent', 'fork', 'continuation')
  AND COALESCE(s.is_automated, 0) = 0
  AND EXISTS (
    SELECT 1
    FROM messages m
    WHERE m.session_id = s.id
      AND COALESCE(m.thinking_text, '') != ''
  )
ORDER BY (
  SELECT COUNT(*)
  FROM messages m
  WHERE m.session_id = s.id
    AND COALESCE(m.thinking_text, '') != ''
) DESC, s.id
LIMIT 1;

-- Drop FTS5 sync triggers before the bulk delete. Each
-- DELETE FROM messages otherwise fires messages_ad which
-- runs an FTS5 'delete' command, and that trips
-- "constraint failed (19)" at scale. We rebuild the FTS
-- index after the deletes and restore the triggers at
-- the end so the test DB still supports in-session search.
DROP TRIGGER IF EXISTS messages_ai;
DROP TRIGGER IF EXISTS messages_ad;
DROP TRIGGER IF EXISTS messages_au;

DELETE FROM tool_calls WHERE session_id IN (
  SELECT id FROM sessions
  WHERE id NOT IN (SELECT id FROM screenshot_sessions)
);
DELETE FROM tool_result_events WHERE session_id IN (
  SELECT id FROM sessions
  WHERE id NOT IN (SELECT id FROM screenshot_sessions)
);
DELETE FROM usage_events WHERE session_id IN (
  SELECT id FROM sessions
  WHERE id NOT IN (SELECT id FROM screenshot_sessions)
);
DELETE FROM pinned_messages WHERE session_id IN (
  SELECT id FROM sessions
  WHERE id NOT IN (SELECT id FROM screenshot_sessions)
);
DELETE FROM starred_sessions WHERE session_id IN (
  SELECT id FROM sessions
  WHERE id NOT IN (SELECT id FROM screenshot_sessions)
);
DELETE FROM secret_findings WHERE session_id IN (
  SELECT id FROM sessions
  WHERE id NOT IN (SELECT id FROM screenshot_sessions)
);
DELETE FROM messages WHERE session_id IN (
  SELECT id FROM sessions
  WHERE id NOT IN (SELECT id FROM screenshot_sessions)
);
DELETE FROM sessions
WHERE id NOT IN (SELECT id FROM screenshot_sessions);

-- Keep generated screenshots independent of the source machine's hostname.
-- The PostgreSQL fixture relabels a subset as work-desktop after push so the
-- multi-machine UI remains covered with deterministic example identities.
UPDATE sessions SET machine = 'dev-laptop';

UPDATE sessions
SET first_message = replace(first_message, r.from_text, r.to_text),
    display_name = replace(display_name, r.from_text, r.to_text),
    session_name = replace(session_name, r.from_text, r.to_text),
    cwd = replace(cwd, r.from_text, r.to_text),
    file_path = replace(file_path, r.from_text, r.to_text)
FROM screenshot_redactions r;

UPDATE messages
SET content = replace(content, r.from_text, r.to_text),
    thinking_text = replace(thinking_text, r.from_text, r.to_text)
FROM screenshot_redactions r;

UPDATE tool_calls
SET file_path = replace(file_path, r.from_text, r.to_text),
    input_json = replace(input_json, r.from_text, r.to_text),
    result_content = replace(result_content, r.from_text, r.to_text)
FROM screenshot_redactions r;

UPDATE tool_result_events
SET content = replace(content, r.from_text, r.to_text)
FROM screenshot_redactions r;

-- Rebuild FTS index from the surviving messages.
INSERT INTO messages_fts(messages_fts) VALUES('rebuild');

-- Restore FTS sync triggers so future inserts/updates
-- keep the index current.
CREATE TRIGGER messages_ai AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, content)
        VALUES (new.id, new.content);
END;
CREATE TRIGGER messages_ad AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content)
        VALUES('delete', old.id, old.content);
END;
CREATE TRIGGER messages_au AFTER UPDATE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content)
        VALUES('delete', old.id, old.content);
    INSERT INTO messages_fts(rowid, content)
        VALUES (new.id, new.content);
END;

-- Update stats
INSERT OR REPLACE INTO stats (key, value) VALUES
  ('session_count', (SELECT COUNT(*) FROM sessions)),
  ('message_count', (SELECT COUNT(*) FROM messages));

-- Remove all real insights and replace with safe seed data
DELETE FROM insights;

INSERT INTO insights (type, date_from, date_to, project, agent, model, content, created_at) VALUES
('daily_activity', '2026-02-20', '2026-02-20', 'roborev', 'claude', 'claude-opus-4-1',
'## Summary

3 sessions across roborev today, totaling 47 messages and 28 tool calls.

### Code Review Engine Improvements

The main focus was on the code review engine. Two sessions worked on improving how review findings are grouped and deduplicated before being presented to the user.

- **Finding deduplication** — added content-hash-based dedup to prevent the same issue from appearing multiple times when files are re-reviewed. Implemented in `internal/review/dedup.go` with a sliding window approach.
- **Severity classification** — refined the severity model to distinguish between style issues, potential bugs, and architectural concerns. Updated the prompt templates in `internal/review/prompts/`.

### Test Coverage

One session focused entirely on adding test coverage for the new dedup logic. Added table-driven tests covering edge cases: overlapping findings, findings across file boundaries, and hash collisions.

### Tool Usage

| Category | Count |
|----------|-------|
| Read     | 12    |
| Edit     | 8     |
| Bash     | 5     |
| Search   | 3     |',
'2026-02-20T18:30:00.000Z'),

('agent_analysis', '2026-02-14', '2026-02-20', NULL, 'claude', 'claude-opus-4-1',
'## Weekly Analysis — Feb 14–20

### Overview

18 sessions across 3 projects over 7 days. Total: 312 messages, 187 tool calls.

| Project | Sessions | Messages | Tool Calls |
|---------|----------|----------|------------|
| roborev | 10 | 198 | 124 |
| agentsview | 5 | 72 | 41 |

### Patterns

**Session length** — median session was 14 messages. Longest session (38 messages) was a roborev refactoring session that touched 12 files. Shorter sessions (under 8 messages) were typically quick bug fixes or documentation updates.

**Tool distribution** — Read and Edit dominate at 68% of all tool calls. Bash usage is concentrated in roborev (test runs and linting). Search tool usage is low, suggesting good familiarity with the codebases.

**Time of day** — most sessions start between 9–11am and 2–4pm, with a gap around lunch. No late-night sessions this week.

### Effectiveness

- Sessions that started with a clear, specific prompt ("fix the failing test in dedup_test.go") completed faster (avg 8 messages) than open-ended ones ("improve the review engine") which averaged 22 messages.
- Tool call errors were rare (3 out of 187), all from file paths that had been renamed mid-session.

### Recommendations

1. **Break up long sessions** — the 38-message refactoring session could have been 2–3 focused sessions with clearer scope.
2. **Use search more** — several sessions spent multiple Read calls navigating to the right file. A Grep or Glob call upfront would save turns.
3. **Pin test commands** — repeated manual test invocations could be replaced with a single Bash alias or Makefile target.',
'2026-02-21T10:15:00.000Z'),

('daily_activity', '2026-02-19', '2026-02-19', 'agentsview', 'claude', 'claude-opus-4-1',
'## Summary

4 sessions in agentsview today, totaling 62 messages and 35 tool calls.

### Frontend Improvements

Two sessions focused on the analytics dashboard. Added a stacked activity timeline chart with daily, weekly, and monthly granularity. The chart uses SVG with D3 scales for responsive rendering.

- **Timeline chart** — implemented in `frontend/src/lib/components/analytics/ActivityTimeline.svelte` with agent-colored stacking and tooltip on hover.
- **Date range picker** — added quick presets (7d, 30d, 90d, 1y, All) alongside custom date inputs.

### Session Export

One session added standalone HTML export for sessions. The exported file includes all messages, tool calls, and thinking blocks with inline CSS so it renders correctly offline.

### Bug Fix

Fixed a race condition where concurrent search and session-load requests could return stale results. Added request cancellation via `AbortController`.',
'2026-02-19T17:45:00.000Z'),

('daily_activity', '2026-02-18', '2026-02-18', 'roborev', 'claude', 'claude-opus-4-1',
'## Summary

2 sessions in roborev today, totaling 31 messages and 19 tool calls.

### API Endpoint Refactoring

Refactored the review submission API to accept batch requests. Previously each file was submitted individually, which caused N+1 request patterns on large PRs. The new endpoint accepts an array of file paths and returns all findings in a single response.

- Updated `internal/server/review_handler.go` with batch support
- Added request validation for max batch size (50 files)
- Updated the CLI client to use the batch endpoint

### Documentation

One short session updated the API documentation in `docs/api.md` to reflect the new batch endpoint and its request/response schema.',
'2026-02-18T16:20:00.000Z');

VACUUM;
SQL

SESSIONS=$(sqlite3 "$OUTPUT" "SELECT COUNT(*) FROM sessions;")
MESSAGES=$(sqlite3 "$OUTPUT" "SELECT COUNT(*) FROM messages;")
SIZE=$(du -h "$OUTPUT" | cut -f1)
RANGE=$(sqlite3 -separator ' to ' "$OUTPUT" \
  "SELECT COALESCE(min(datetime(COALESCE(NULLIF(started_at, ''), created_at))), '(none)'),
          COALESCE(max(datetime(COALESCE(NULLIF(started_at, ''), created_at))), '(none)')
   FROM sessions;")

echo "Extracted: $SESSIONS sessions, $MESSAGES messages ($SIZE)"
echo "Date range: $RANGE"
sqlite3 "$OUTPUT" \
  "SELECT '  ' || project || ': ' || COUNT(*) || ' sessions' FROM sessions GROUP BY project ORDER BY project COLLATE NOCASE;"
