# Project Research Summary

**Project:** agentsview — pi-agent parser integration
**Domain:** Adding a new agent session parser to an existing multi-agent session viewer
**Researched:** 2026-02-27
**Confidence:** HIGH

## Executive Summary

This is a bounded feature addition to an existing, well-structured Go+SQLite+Svelte codebase. The task is to slot pi-agent JSONL session support alongside six existing agent parsers (Claude, Codex, Copilot, Gemini, OpenCode, Cursor). The approach is well-defined: all prior agents establish a clear integration pattern (parser → types constant → taxonomy → discovery → engine → config → main → frontend), and pi-agent follows the same layered architecture. No new dependencies, no schema migrations, and no architectural changes are required. The stack is fixed.

The recommended approach is to follow the Gemini agent as the primary reference for directory-based discovery, and the Claude agent as the reference for JSONL parsing logic. Pi-agent's JSONL format is close to Claude's but with important differences: a mandatory session header line, `toolCall`/`toolResult` block type names (versus Claude's `tool_use`/`tool_result`), and `arguments` replacing `input` in tool call blocks. These differences are well-documented from direct source and real file inspection. The implementation requires one new file (`internal/parser/pi.go`), one new test file, and surgical additions across seven existing files totaling roughly 180 lines of code.

The key risk is incomplete wiring: there are seven distinct touch-points across the codebase (types, taxonomy, parser, discovery, engine — four locations, config, main, frontend) and missing any one causes silent failures or wrong behavior at runtime. The second risk is directory encoding ambiguity — the pi source code specifies `--path--` double-dash encoding, but real session directories on disk show single-dash encoding (`-path/`), requiring a discovery function that validates by reading the file header rather than relying on directory name format alone.

## Key Findings

### Recommended Stack

The stack is entirely fixed by the existing codebase. No new dependencies are added. Pi-agent parsing uses `github.com/tidwall/gjson` (already imported in `claude.go`) for JSONL field extraction, mirroring the Claude parser pattern exactly. The `CGO_ENABLED=1 -tags fts5` build requirements are unchanged.

**Core technologies:**
- `github.com/tidwall/gjson`: JSONL field extraction — already used in claude.go; pi parser mirrors this pattern
- `encoding/bufio.Scanner`: Line-by-line JSONL reading — same as all existing parsers
- SQLite FTS5: Full-text search indexing — no schema changes; pi messages flow through the existing pipeline
- Svelte 5 / TypeScript: Frontend agent filter — one entry in `KNOWN_AGENTS` array covers all filter UI and badge rendering

### Expected Features

The full feature set is documented in FEATURES.md. Summary of priorities:

**Must have (table stakes — v1):**
- `AgentPi` constant in `types.go` — prerequisite for all storage
- Session header parsing (id, cwd, timestamp, parentSession, branchedFrom) — required for ParsedSession construction
- User, assistant, and toolResult message parsing — core session content
- Thinking block extraction including redacted thinking (`HasThinking=true`) — expected by thinking-model users
- Tool call extraction with `arguments` field (not `input`) — essential; silently wrong if using Claude approach
- `find` tool → "Read" taxonomy category — current taxonomy omits it
- Compaction entry as synthetic user message — sessions with compaction are unreadable without it
- `model_change` and `thinking_level_change` as meta messages — appear in real fixtures; silently dropping loses context
- `PI_DIR` / `PiDirs` config with `~/.pi/agent/sessions/` default
- Session directory discovery for encoded-cwd subdirectory pattern
- Unit tests for all of the above

**Should have (v1.x — after validation):**
- `session_info.name` display — human-readable session names
- `branchedFrom` resolution against existing DB sessions — opportunistic parent linking
- Cost display per session — `usage.cost.total` sum across assistant messages

**Defer (v2+):**
- HTML export for pi sessions
- Image content block handling (base64 storage/display is out of scope)
- `bashExecution` custom message display
- Real-time file watching during active pi runs (15-min periodic sync is sufficient)

### Architecture Approach

Pi-agent integrates through a strict top-down dependency chain that mirrors every existing agent. The parser layer produces `ParsedSession` + `[]ParsedMessage` with no DB coupling. The sync/discovery layer finds files and routes them to the parser. The config layer exposes directory configuration. The main entrypoint wires everything together. The frontend receives agent type as a string from the API and renders filter/badge automatically. No layer is aware of layers above it.

**Major components:**
1. `internal/parser/pi.go` — Parses JSONL lines into ParsedSession + []ParsedMessage; dispatches on event type; handles header, message roles, content block types
2. `internal/sync/discovery.go` — `DiscoverPiSessions()` walks encoded-cwd subdirectories; validates files by reading first line (not by directory name format); `FindPiSourceFile()` resolves session ID to path
3. `internal/sync/engine.go` — Holds `piDirs []string`; routes discovered files to `processPi()`; handles `"pi:"` session ID prefix in `FindSourceFile()` and `SyncSingleSession()`
4. `internal/config/config.go` — `PiDir`/`PiDirs` fields, `PI_DIR` env var, `ResolvePiDirs()` method
5. `cmd/agentsview/main.go` — Wires config to engine constructor and file watcher
6. Frontend (`agents.ts`, `App.svelte`, `app.css`) — Registers `"pi"` in `KNOWN_AGENTS`; adds teal badge CSS class

### Critical Pitfalls

1. **`toolCall`/`toolResult` block type names** — Do NOT reuse `ExtractTextContent` from `content.go`; it handles only `tool_use`/`tool_result`. Write a separate `extractPiContent` function that handles pi's camelCase block types and `arguments` field. Silent failure: `HasToolUse=false` on all pi assistant messages.

2. **`toolResult` messages are standalone entries** — Unlike Claude (where tool results are embedded in user message content arrays), pi emits separate top-level `type:"message"` entries with `message.role:"toolResult"`. A parser that only collects `"user"` and `"assistant"` roles silently discards all tool result data.

3. **Directory encoding ambiguity** — Pi source code specifies `--path--` double-dash encoding but real sessions on disk may use single-dash encoding (`-path/`). Discovery must validate by reading the first line of JSONL files and checking for `{"type":"session",...}` rather than relying on directory name format.

4. **Session ID from header, not filename** — Pi filenames are `{timestamp}_{id}.jsonl`. Claude's filename-is-session-ID approach produces the entire filename stem as the ID. Read `header.id` from line 1 instead. Fallback to filename stem only if header is missing.

5. **V1 sessions have no `id`/`parentId` fields** — The `version` field is absent or `1` in old sessions. Fall back to linear file-order parsing if `version` is missing or `< 2`. Do not assume `parentId` chaining is available.

6. **Engine wiring has four touch-points** — `AgentType` constant + `classifyOnePath` + `processFile` switch + `syncAllLocked` discovery loop must all be updated together. Missing any one causes `"unknown agent type: pi"` errors and silent session loss.

7. **Missing CSS variable for badge color** — `--accent-teal` does not exist in `app.css`. Adding `"pi"` to `KNOWN_AGENTS` with `"var(--accent-teal)"` before defining the variable produces invisible (transparent) badges.

## Implications for Roadmap

Based on research, the dependency chain is strict and the implementation divides naturally into three phases.

### Phase 1: Parser Foundation
**Rationale:** The `AgentPi` constant and parser are the root dependency for everything else. Nothing can be stored, discovered, or displayed without a working parser. This is also the most logic-dense work and the most test-deserving.
**Delivers:** A fully tested `ParsePiSession` function that converts real pi JSONL files into `ParsedSession` + `[]ParsedMessage` with correct message roles, tool call extraction, thinking blocks, compaction handling, and taxonomy.
**Addresses:** AgentPi constant, session header parsing, user/assistant/toolResult message parsing, thinking block extraction, tool call extraction (arguments field), `find`/`grep` taxonomy additions, compaction as synthetic message, model_change and thinking_level_change meta messages, v1 session fallback.
**Avoids:** toolCall/toolResult block type confusion (Pitfall 6), session ID from header not filename (Pitfall 5), toolResult role misidentification (Pitfall 1), v1 sessions without parentId (Pitfall 3).

### Phase 2: Sync and Config Integration
**Rationale:** With a working parser in place, the sync and config layers can be wired. These follow established patterns exactly — Gemini is the reference. The directory encoding ambiguity must be resolved here.
**Delivers:** Pi sessions discovered on startup and via 15-minute periodic sync. `PI_DIR` env var and `pi_dirs` config key working. File watcher updated to watch pi session directories.
**Addresses:** PI_DIR / PiDirs config, session directory discovery, mtime-based skip cache.
**Avoids:** Directory encoding ambiguity (Pitfall 2), engine not fully wired (Pitfall 7), missing watcher registration.

### Phase 3: Frontend Wiring
**Rationale:** Frontend changes are mechanical and depend on the backend being complete. One entry in `KNOWN_AGENTS`, one CSS class in `App.svelte`, one new CSS variable in `app.css`.
**Delivers:** Pi sessions appear in the session list with a teal badge. Agent filter button for "pi" appears in the sidebar. No regressions to existing agent filters.
**Addresses:** Filter UI for pi agent, badge color (teal, distinct from all six existing agents).
**Avoids:** Missing CSS variable for badge (Pitfall 7/anti-pattern 3), reusing an occupied accent color (anti-pattern 4).

### Phase Ordering Rationale

- Parser-first is mandatory: all other layers depend on `AgentPi` constant and `ParsePiSession` existing.
- Config and sync can technically be partially parallel with parser, but integration testing requires both. Keeping them in Phase 2 avoids partial-state integration bugs.
- Frontend is a pure leaf dependency — it reads agent type as a string from the API and is fully decoupled from parsing logic.
- The build order in ARCHITECTURE.md (10 steps) maps cleanly onto these 3 phases: steps 1-3 = Phase 1, steps 4-7 = Phase 2, steps 8-10 = Phase 3.

### Research Flags

Phases with standard, well-documented patterns (no additional research needed):
- **Phase 1 (Parser):** Pi format fully documented from direct source and real file inspection. All content block types, field names, and edge cases (v1, compaction, toolResult role) are confirmed with HIGH confidence.
- **Phase 2 (Sync/Config):** Gemini integration is the established reference pattern. All code locations confirmed by direct source inspection.
- **Phase 3 (Frontend):** Mechanical addition. Pattern confirmed from `SessionList.svelte`, `agents.ts`, `App.svelte`, `app.css`.

Phases needing validation before final implementation:
- **Phase 2 (Discovery):** Directory encoding format (`--path--` vs `-path/`) must be verified against actual session directories on the target machine before writing `DiscoverPiSessions`. The discovery function should validate by file header content rather than directory name format.

## Confidence Assessment

| Area | Confidence | Notes |
|------|------------|-------|
| Stack | HIGH | Fixed stack; no new dependencies; confirmed from direct source |
| Features | HIGH | Verified against real pi session fixture (2.3MB), pi-mono TypeScript types, and agentsview existing parser types |
| Architecture | HIGH | All integration patterns confirmed by direct source inspection of actual agentsview files at commit f397a93 |
| Pitfalls | HIGH | Most pitfalls derived from real file evidence and cross-referencing source with actual data; one confirmed ambiguity (directory encoding) |

**Overall confidence:** HIGH

### Gaps to Address

- **Directory encoding format:** Source code specifies `--path--` but real sessions on disk show `-path/`. Discovery must use file content validation, not directory name matching. Verify by ls-ing actual session directories before writing `DiscoverPiSessions`.
- **V1 session prevalence:** Unknown how many v1 sessions (no `version` field) exist in the wild. The v1 fallback is simple to implement (linear ordering) and should be included regardless, but there are no v1 fixtures available for testing.
- **`branchedFrom` path resolution:** The `branchedFrom` field is an absolute path on the original machine. Whether it resolves to an existing DB session depends on path matching. Defer full implementation; populate `ParentSessionID` opportunistically only when the referenced file is already in the DB.
- **`grep` taxonomy:** PITFALLS.md flags `grep` as missing from `NormalizeToolCategory`, but STACK.md notes the multi-case `case "search_files", "grep"` covers it. Verify with `NormalizeToolCategory("grep")` during implementation — if it returns "Grep", no change needed; if it returns "Other", add the case.

## Sources

### Primary (HIGH confidence)
- `/Users/carze/.omp/agent/sessions/-Documents-personal-misc/2026-02-14T19-40-45-439Z_146eb832ffc34f07.jsonl` — real pi session file (21 lines, all event types observed)
- `/Users/carze/Documents/personal/misc/pi-mono/packages/coding-agent/src/core/session-manager.ts` — pi-mono session format source of truth
- `/Users/carze/Documents/personal/misc/pi-mono/packages/coding-agent/test/fixtures/before-compaction.jsonl` — 2.3MB real fixture confirming compaction field names
- `internal/parser/claude.go` — reference JSONL parser implementation
- `internal/parser/gemini.go` — reference directory-based discovery and engine pattern
- `internal/sync/engine.go` — confirmed all four wiring locations for agent dispatch
- `internal/sync/discovery.go` — DiscoverGeminiSessions as discovery template
- `internal/config/config.go` — GeminiDir/GeminiDirs pattern confirmed verbatim
- `frontend/src/lib/utils/agents.ts` — KNOWN_AGENTS array confirmed
- `frontend/src/App.svelte` — badge CSS class pattern confirmed

### Secondary (MEDIUM confidence)
- `pi-mono/packages/ai/src/types.ts` — TypeScript type definitions for message content blocks
- `pi-mono/packages/agent/src/types.ts` — AgentMessage, ThinkingLevel union type
- `.planning/PROJECT.md` — project-level format specification and scope boundaries

### Tertiary (inferred)
- V1 session format — described in session-manager.ts migration code but no v1 fixture available for direct verification

---
*Research completed: 2026-02-27*
*Ready for roadmap: yes*
