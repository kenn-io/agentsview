# Phase 4: Better pi Tool Call Rendering - Context

**Gathered:** 2026-02-28
**Status:** Ready for planning

<domain>
## Phase Boundary

Improve how pi agent tool calls are displayed in the session viewer. Currently pi tool calls render as raw `key: value` fallback output (via `generateFallbackContent`) because pi's structured JSONL format uses different tool names and argument shapes than Claude Code. This phase adds pi-specific metadata extraction and content formatting so pi tool calls look as polished as Claude Code's output.

In scope: frontend rendering improvements for pi tool calls (metadata tags, expanded content format, suppression of toolResult messages).
Out of scope: changes to the pi parser/backend, adding tool result content to the UI, or adding tool coverage for non-pi agents.

</domain>

<decisions>
## Implementation Decisions

### Tool name aliasing
- Map pi tool names to Claude Code aliases for display: `str_replace` → Edit, `run_command` → Bash, `create_file` → Write, `read_file` → Read
- Use the same alias mechanism already in `content-parser.ts` (TOOL_ALIASES) — extend it with pi names

### Tool metadata tags (collapsed header)
- `str_replace` (Edit): show `file: path` tag — matches existing Edit behavior
- `run_command` (Bash): show `description` field if present — matches existing Bash behavior
- `create_file` (Write): show `file: path` tag — matches existing Write behavior
- `read_file` (Read): show `file: path` tag — matches existing Read behavior

### Expanded content format
- `str_replace` (Edit): `--- old` / `+++ new` diff style showing `old_string` and `new_string` — matches existing Claude Code Edit formatting
- `run_command` (Bash): `$ command` with full multiline support — matches existing Claude Code Bash formatting
- `create_file` (Write): truncated file content (~500 chars) with ellipsis — matches existing Write behavior

### Tool result messages
- Suppress `toolResult` messages entirely — they currently render as blank/empty user messages
- Do not surface result content length anywhere — hide completely

### Architecture
- Extend existing `tool-params.ts` (not a new file) with pi tool name handlers
- Try known pi-specific field names first (e.g. `command`, `path`, `content`, `old_string`, `new_string`), fall through to generic key:value fallback if no known fields match
- Unknown pi tool types get the existing generic fallback — safe and extensible

### Claude's Discretion
- Exact pi argument field names to use (researcher should inspect real pi JSONL test files to confirm `str_replace` uses `old_string`/`new_string`/`path`, `run_command` uses `command`, `create_file` uses `path`/`content`, `read_file` uses `path`)
- Whether toolResult suppression happens at the message list level or via a CSS/render flag

</decisions>

<specifics>
## Specific Ideas

- The visual language should match Claude Code tool blocks exactly — same collapsed header style, same diff format for edits, same `$` prefix for commands
- The toolResult suppression is purely a rendering concern — the data stays in the DB, just not shown

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope.

</deferred>

---

*Phase: 04-add-better-parsing-of-tool-calls-out-of-pi*
*Context gathered: 2026-02-28*
