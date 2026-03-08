# Phase 1: Parser Foundation - Context

**Gathered:** 2026-02-27
**Status:** Ready for planning

<domain>
## Phase Boundary

Implement `internal/parser/pi.go` with `ParsePiSession` — a new Go parser that reads pi-agent JSONL session files and populates the existing `ParsedSession`/`ParsedMessage` model with full parity to Claude. No UI, no sync wiring, no config changes — pure parser layer only.

</domain>

<decisions>
## Implementation Decisions

### Test fixture strategy
- Use committed fixture files in `internal/parser/testdata/pi/`
- Fixture is synthesized from the spec (no real pi files required), covering all required message types
- Fixture includes both happy-path entries AND intentionally malformed/edge-case lines to test error handling in a single file
- Tests must pass in CI without pi installed

### Error handling
- Malformed/invalid JSON lines: skip silently (matches existing Claude parser behavior)
- Unrecognized block types inside assistant messages: skip silently
- Unrecognized top-level entry types (e.g., `thinking_level_change`): skip silently
- Consistent philosophy: no noise, no partial failures, no log output from the parser itself

### Compaction message content
- Compaction entries produce a synthetic user message whose content is the raw `summary` field
- If summary is missing or empty: use fallback text (e.g., `"[session compacted]"`) to preserve timeline continuity
- Model change entries produce a meta message with a descriptive sentence: `"Model changed to {model_name}"`

### V1 fallback
- V1 detection: if ANY entry in the file has an `id` field, treat the whole file as V2. V1 mode only for pure V1 files (no id/parentId/version anywhere)
- V1 session ID: filename basename without extension (same as Claude parser pattern)
- `branchedFrom` → `ParentSessionID`: store it always, regardless of whether the parent exists in the DB yet

### Claude's Discretion
- Exact fallback text wording for empty compaction summary
- Internal struct layout and helper function organization within pi.go
- Line reader reuse vs. reimplementation (can reuse existing `newLineReader`)

</decisions>

<specifics>
## Specific Ideas

- Follow the Claude parser's skip-silently pattern throughout — consistency over strictness
- `internal/parser/testdata/pi/` follows Go testdata convention and is co-located with the parser code

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope.

</deferred>

---

*Phase: 01-parser-foundation*
*Context gathered: 2026-02-27*
