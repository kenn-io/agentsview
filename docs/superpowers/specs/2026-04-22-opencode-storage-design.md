# OpenCode Storage Compatibility Design

Date: 2026-04-22

## Goal

Update `agentsview` so OpenCode sessions are discovered, parsed, synced, and watched from both:

- legacy SQLite storage at `OPENCODE_DIR/opencode.db`
- current file-backed storage rooted at `OPENCODE_DIR/storage/`

The user-facing behavior should be transparent under the existing `OPENCODE_DIR` configuration. OpenCode should behave like a first-class harness in agentsview, including live watch support, rather than remaining a SQLite-only special case.

## Background

OpenCode currently writes live session data into a file tree under `~/.local/share/opencode/storage/`, with examples such as:

- `storage/session/<projectID>/<sessionID>.json`
- `storage/message/<sessionID>/<messageID>.json`
- `storage/part/<messageID>/<partID>.json`

Recent OpenCode docs and upstream code both show `storage/` as a real persisted session format. `opencode.db` still exists and remains relevant for older installs and historical compatibility, but recent live sessions can exist only in `storage/`.

Today, agentsview still treats OpenCode as non-file-based and reads only `opencode.db`. That causes recent OpenCode sessions to be missed and keeps OpenCode outside the normal file watcher pipeline used by other major harnesses.

## Requirements

- Keep `OPENCODE_DIR` as the only config surface.
- Auto-detect the active OpenCode source under that root.
- Prefer file-backed `storage/` when present.
- Fall back to `opencode.db` only when file-backed storage is absent.
- Preserve the external session ID format as `opencode:<sessionID>`.
- Support watch-driven updates for file-backed OpenCode sessions.
- Preserve legacy SQLite compatibility.

## Non-Goals

- No new user-facing config flags for choosing OpenCode source.
- No generalized multi-source framework refactor for all agents.
- No mixed-source merge behavior when both `storage/` and SQLite are present.

## Decision Summary

OpenCode becomes file-first in agentsview.

Source selection under `OPENCODE_DIR`:

1. If `storage/session` exists, use file-backed OpenCode mode.
2. Else if `opencode.db` exists, use SQLite fallback mode.
3. Else treat the directory as having no OpenCode sessions.

When `storage/` exists, it wins completely. Agentsview must not selectively fall back to SQLite for missing or unreadable file-backed sessions, because that can reintroduce stale copies of sessions that have already migrated.

## Architecture

### 1. OpenCode Source Resolution

Introduce a small resolver for an OpenCode root directory:

- detect file-backed mode from `storage/session`
- otherwise detect SQLite mode from `opencode.db`
- otherwise no usable source

This resolver should be used consistently by:

- bulk sync
- single-session re-sync
- source file lookup
- discovery

### 2. File-Backed OpenCode Parser

Add a file-backed OpenCode parser alongside the existing SQLite parser.

Session metadata source:

- `storage/session/<projectID>/<sessionID>.json`

Message metadata source:

- `storage/message/<sessionID>/<messageID>.json`

Part content source:

- `storage/part/<messageID>/<partID>.json`

The file-backed parser should produce the same `ParsedSession` and `ParsedMessage` shapes as the current SQLite path.

### 3. Discovery and Watch Integration

OpenCode should participate in the normal file-based machinery when `storage/` is active:

- discovery emits one discovered file per session JSON file
- watcher roots include:
  - `storage/session`
  - `storage/message`
  - `storage/part`

This makes OpenCode operationally similar to Codex and Claude Code inside agentsview.

### 4. SQLite Fallback

Retain the existing SQLite parsing logic for environments where `storage/` is absent.

The current SQLite-specific sync path may remain, but it should only run when the source resolver chooses SQLite mode.

## File-Backed Parsing Details

### Session Metadata

For a session file:

- session ID comes from `id`
- project/worktree should come from `directory`
- parent session should come from `parentID` if present
- title should come from `title`
- started/ended timestamps should come from `time.created` and `time.updated`
- file path should be the real session JSON path
- file mtime should come from the session file stat

The external stored ID remains `opencode:<sessionID>`.

### Message Ordering

Messages are ordered by message `time.created`.

When needed, messages can be sorted after loading based on the parsed timestamp rather than filesystem enumeration order.

### Message Role, Model, and Token Usage

For each message file:

- role comes from `role`
- assistant model comes from `modelID`, or from `model.modelID` if needed for observed variants
- provider may come from `providerID` or `model.providerID`
- token usage primarily comes from the message JSON `tokens`
- `step-finish.tokens` may fill or override gaps when present

The parser should preserve the current token coverage semantics used by agentsview:

- explicit zero values count as known values
- absent token structures should not fabricate coverage

### Parts

Relevant part types already observed in live storage:

- `text`
- `reasoning`
- `tool`
- `step-start`
- `step-finish`

Parsing behavior:

- `text` contributes to message content
- `reasoning` sets thinking flags and may contribute captured thinking text if current OpenCode semantics already do so
- `tool` contributes tool call metadata using the same normalization rules as the SQLite parser
- `step-start` is informational only unless needed for ordering
- `step-finish` can carry finish reason and token totals

Part ordering should use embedded times where present and otherwise fall back to a stable deterministic order.

### First Message / Title Fallback

Continue the current OpenCode behavior:

- prefer the OpenCode title when it is non-empty and not a known placeholder
- otherwise use the first user message text

Placeholder titles continue to match the known formats such as:

- `New session - <timestamp>`
- `Child session - <timestamp>`

### Empty or Partial Sessions

A file-backed OpenCode session should only be emitted when it contains at least one usable user or assistant message after parsing. Sessions with unreadable or incomplete files should be skipped safely, matching the current parser behavior of ignoring non-usable sessions rather than inventing placeholder transcripts.

## Discovery and Source Lookup

### Discovery

Add OpenCode file discovery for:

- `storage/session/<projectID>/*.json`

Each discovered item should identify:

- path to the session JSON
- agent as `AgentOpenCode`
- project name derived from session directory content or normalized from the worktree when available

### Source File Lookup

In file-backed mode, `FindSourceFile("opencode:<id>")` should return the actual session JSON path.

In SQLite fallback mode, `FindSourceFile` should continue to return the existing virtual `dbPath#sessionID` representation if needed by the current DB-backed flow.

## Watch Behavior

### Watch Roots

When file-backed OpenCode is active, watch:

- `storage/session`
- `storage/message`
- `storage/part`

This fits the existing `WatchSubdirs` pattern used by other file-backed harnesses.

### Incremental Refresh

Watch-triggered updates under `storage/message` or `storage/part` should resolve the owning session ID from the changed JSON file, then reparse only that session.

This avoids full rescans while keeping OpenCode responsive during active sessions.

Behavior by path type:

- session file changed: resync that session
- message file changed: read `sessionID`, resync that session
- part file changed: read `sessionID` from the part JSON, resync that session

If a changed file cannot be decoded, the event should be ignored and recovered by the next full sync rather than forcing a global fallback to SQLite.

## Sync Behavior

### Bulk Sync

When file-backed mode is active:

- discover session files from `storage/session`
- parse each session by joining its sibling message and part files
- use the normal file-based sync flow

When SQLite fallback mode is active:

- keep using the current OpenCode SQLite sync path

### Single-Session Sync

`SyncSingleSession("opencode:<id>")` should:

- resolve the OpenCode source mode for each configured root
- in file-backed mode, locate the session JSON and reparse that session
- in SQLite mode, continue using the existing DB-backed single-session path

## Error Handling

- File-backed parse errors should be handled per session or per file, not by switching the whole root to SQLite.
- If both `storage/` and SQLite exist, file-backed mode remains authoritative even when some files are malformed.
- Missing or transiently incomplete message/part files should not delete good historical data already stored in agentsview.
- Full sync remains the recovery path for missed watch events.

## Testing Plan

### Parser Tests

Add file-backed OpenCode parser tests for:

- standard session parsing
- title fallback from first user message
- parent session mapping
- reasoning parts
- tool parts
- text parts
- token extraction from message JSON
- token extraction or completion from `step-finish`
- partial or invalid files skipped safely

Existing SQLite OpenCode tests should remain and continue to verify fallback compatibility.

### Discovery Tests

Add tests for:

- discovery under `storage/session/<projectID>/*.json`
- source lookup by OpenCode session ID in file-backed mode
- resolver preference for file-backed storage over SQLite

### Sync Integration Tests

Add integration coverage for:

- file-backed OpenCode bulk sync
- watch-driven single-session refresh after message changes
- watch-driven single-session refresh after part changes
- SQLite fallback when `storage/` is absent
- mixed root with both `storage/` and `opencode.db`, proving file-backed source wins

## Risks and Tradeoffs

### Risk: Mixed Storage Ambiguity

If both sources exist, choosing `storage/` could ignore useful historical rows still present only in SQLite.

Resolution:

- prefer correctness for live/current sessions
- keep SQLite as fallback only when file-backed storage is absent
- if OpenCode has partial migration states, users can still force a full resync after the source stabilizes

### Risk: Watch Event Churn

OpenCode can emit many updates under `message/` and `part/` during active sessions.

Resolution:

- rely on the existing debounced watcher
- reparse only the affected session, not the entire tree

### Risk: Part Ordering Inconsistencies

Some part files may omit explicit timestamps.

Resolution:

- sort primarily by embedded time fields when present
- use deterministic fallback ordering so transcripts remain stable

## Expected Outcome

After this change:

- recent OpenCode sessions in `storage/` appear in agentsview
- OpenCode sessions update live while active
- older installs using `opencode.db` still work
- OpenCode behaves like a first-class harness in agentsview rather than a legacy SQLite special case
