# Pitfalls Research

**Domain:** Adding pi-agent JSONL parser to agentsview
**Researched:** 2026-02-27
**Confidence:** HIGH (based on source reading of pi-mono session-manager.ts, real pi session files, and agentsview parser/sync code)

---

## Critical Pitfalls

### Pitfall 1: Misidentifying `role:"toolResult"` Messages as Filterable Noise

**What goes wrong:**
The Claude parser skips lines that are not `type:"user"` or `type:"assistant"` at the top level (see `internal/parser/claude.go` line 107). In pi format, tool results are separate top-level `type:"message"` entries with `message.role:"toolResult"` — not embedded in user message content arrays. A parser that only collects messages where `message.role` is `"user"` or `"assistant"` will silently discard all tool result entries, producing sessions with no tool result data and incorrect message counts.

**Why it happens:**
Developers model the pi parser after the Claude parser's role-based filtering. Claude embeds tool results in user message content blocks (`type:"tool_result"`). Pi separates them as standalone messages with a distinct role. The naming is similar enough to cause an incorrect assumption about the structure.

**How to avoid:**
During parsing, collect `type:"message"` entries where `message.role` is `"user"`, `"assistant"`, or `"toolResult"`. When building the rendered content for a message, match `toolCallId` from toolResult messages back to the preceding assistant tool call. Alternatively, treat toolResult messages as `ToolResults` on the adjacent ParsedMessage, not standalone messages. Study the real pi session files: line 6 of the example session shows `{"type":"message","id":"3a825e73","parentId":"b7d51d4b","timestamp":"...","message":{"role":"toolResult","toolCallId":"functions.read:0","toolName":"read","content":[...]}}`.

**Warning signs:**
- Test sessions show tool calls in assistant messages but zero tool results in user messages
- `HasToolUse` is true on messages but matching ToolResults list is empty
- Session message count much lower than expected (tool result messages not counted)

**Phase to address:**
Parser implementation phase (core pi parser). Cover with table-driven tests that include full round-trips of assistant-toolCall then toolResult pairs.

---

### Pitfall 2: Wrong Directory Encoding — Pi Uses `--path--` Not `-path-`

**What goes wrong:**
The sync engine's `DiscoverClaudeProjects` uses `-Users-alice-code-my-app/` directory naming (single leading dash, hyphens replacing path separators). Pi uses a different encoding: `--Documents-personal-misc--` (double leading and trailing dashes, hyphens replace separators). The discovery function for pi sessions must use the pi encoding, not the Claude encoding.

From `session-manager.ts` line 401: `const safePath = \`--${cwd.replace(/^[/\\]/, "").replace(/[/\\:]/g, "-")}--\`;`

Claude projects directory: `-Users-carze-Documents-personal-misc-agentsview/`
Pi sessions directory: `-Documents-personal-misc/` (the example real directory on disk at `~/.omp/agent/sessions/`)

Wait — looking at actual disk contents: `~/.omp/agent/sessions/` has dirs like `-Documents-personal-misc/` (single leading dash), NOT double dashes. Cross-check the source against actual disk: source says `--path--` but real directories show `-path/`. One or the other may be an older version. The real fs evidence overrides the source code.

**Concrete risk:** Building the discovery function based on the source code's `--path--` format when real sessions on disk use `-path/` means the discover function finds nothing. Alternatively, if newer pi versions write `--path--`, the discover function using `-path/` misses those.

**How to avoid:**
Before writing the discover function, check actual pi session directory names on the developer's machine. Tolerate both formats by scanning all subdirectories of the pi sessions root and validating by reading the first line of any `.jsonl` file to check for `{"type":"session",...}` rather than relying on the directory name format alone. Use `isValidSessionFile`-style validation (same as pi's own logic) as the source of truth.

**Warning signs:**
- `DiscoverPiSessions` returns 0 files even when `~/.pi/agent/sessions/` contains session directories
- Log output shows "discovered 0 pi sessions" after startup
- Manually ls-ing the sessions directory shows subdirectory names that don't match the pattern assumed in code

**Phase to address:**
Sync discovery phase. Add a test that seeds a temp directory with both encoding formats and verifies both are discovered.

---

### Pitfall 3: Ignoring `version` Field — V1 Sessions Have No `id`/`parentId`

**What goes wrong:**
Pi session files have a version field in the header (`"version":3` for current, absent for v1). The `id`/`parentId` chain used for tree traversal was only added in v2 (`migrateV1ToV2`). A v1 file has no `id` or `parentId` fields on any entry. The parser relies on `id`/`parentId` to order messages. If unhandled, old sessions from v1 produce empty or garbled message lists.

From `session-manager.ts` line 31: `version?: number; // v1 sessions don't have this`

**How to avoid:**
Read the `version` from the session header (line 1). If `version` is missing or 1, fall back to linear ordering by file position (same as the Claude linear parser fallback for entries without uuid). If version 2+, use `parentId` chain traversal. Do not assume `id`/`parentId` are present.

**Warning signs:**
- Old pi session files (pre-2025) produce empty message lists
- Parser produces 0 messages for a session that has obvious content in the file

**Phase to address:**
Parser implementation phase. Include at least one v1 fixture (no `version` field, no `id`/`parentId`) in the test suite.

---

### Pitfall 4: Missing `grep` in Taxonomy — Pi Uses Lowercase Tool Names

**What goes wrong:**
Pi tool names are all lowercase: `read`, `write`, `edit`, `bash`, `grep`, `glob`, `find`. The existing `NormalizeToolCategory` in `internal/parser/taxonomy.go` already has entries for lowercase `read`, `edit`, `write`, `bash`, `glob`, `task` (lines 37-48) but is missing `grep` (pi) and `find`. A pi `grep` tool call would fall into the `"Other"` category, breaking the tool filter UI and FTS categorization.

**How to avoid:**
Add `"grep"` → `"Grep"` and `"find"` → `"Read"` (or `"Other"`) to `NormalizeToolCategory`. Verify the full taxonomy test in `internal/parser/taxonomy_test.go` covers pi tool names.

**Warning signs:**
- `NormalizeToolCategory("grep")` returns `"Other"` not `"Grep"`
- Tool category filters in the UI show `"Other"` for pi grep calls
- Missing from `internal/parser/taxonomy_test.go`

**Phase to address:**
Parser implementation phase. This is a one-line fix but easy to overlook. Add test case `{"grep", "Grep"}` to `TestNormalizeToolCategory`.

---

### Pitfall 5: Session ID Collision — Pi IDs Are 8 Hex Chars, Not UUIDs

**What goes wrong:**
Claude session IDs are full UUIDs (`601b4282-5c5e-43d6-832f-fcb9c76c2a6f`). Pi session IDs are 8-character hex strings (`146eb832`), derived from `randomUUID().slice(0, 8)`. Pi encodes session ID in the filename as `2026-02-14T19-40-45-439Z_146eb832ffc34f07.jsonl` — the ID after the underscore is 16 chars (two 8-char parts). The session header has `"id":"146eb832ffc34f07"`. If the parser uses the filename stem as the session ID (as Claude does with `strings.TrimSuffix(filepath.Base(path), ".jsonl")`), the session ID becomes the full filename stem `2026-02-14T19-40-45-439Z_146eb832ffc34f07`, not the header's `id` field. These differ and the parser must use one consistently.

**Why it happens:**
The Claude parser uses filename-as-session-ID because Claude session files are named `{uuid}.jsonl`. Pi files are named `{timestamp}_{id}.jsonl`. Using the filename stem as session ID creates an unnecessarily long and timestamp-prefixed ID.

**How to avoid:**
Read the session header (line 1) and use `header.id` as the session ID. This matches the actual id embedded in all entries' `parentId` chains. If the header is missing or invalid, fall back to the filename stem. Do not blindly copy Claude's `strings.TrimSuffix(filepath.Base(path), ".jsonl")` pattern.

**Warning signs:**
- Session IDs in the database look like `2026-02-14T19-40-45-439Z_146eb832ffc34f07` instead of `146eb832ffc34f07`
- `parentId` chains in session entries reference `146eb832ffc34f07` but stored session ID doesn't match

**Phase to address:**
Parser implementation phase. Add test that verifies session ID comes from header, not filename.

---

### Pitfall 6: Content Array Block Types — `toolCall` Not `tool_use`, `toolResult` Not `tool_result`

**What goes wrong:**
Claude uses `"type":"tool_use"` and `"type":"tool_result"` in content arrays. Pi uses `"type":"toolCall"` and `"type":"toolResult"` (camelCase). The existing `ExtractTextContent` in `internal/parser/content.go` only handles `tool_use` and `tool_result`. Calling `ExtractTextContent` on pi content blocks produces zero tool calls extracted — all tool calls fall through as unrecognized and the function returns empty ToolCalls.

Pi block structure (from real session line 5):
```
{"type":"toolCall","id":"functions.read:0","name":"read","arguments":{...}}
```
vs. Claude:
```
{"type":"tool_use","id":"toolu_abc","name":"Read","input":{...}}
```

Note also: pi uses `"arguments"` not `"input"` for the tool input object.

**How to avoid:**
Do NOT reuse `ExtractTextContent` directly for pi content. Write a parallel `ExtractPiContent` function that handles `toolCall` (with `arguments` field) and `toolResult` (with `toolCallId` field) block types. Or extend `ExtractTextContent` to also handle these variants — but this risks leaking pi-specific logic into Claude parsing.

**Warning signs:**
- Pi assistant messages show `HasToolUse = false` even when they contain `toolCall` blocks
- ToolCalls list is empty for pi assistant messages
- Tool call rendering shows nothing for pi sessions in UI

**Phase to address:**
Parser implementation phase. This is the most common mistake since `ExtractTextContent` is tempting to reuse. Test explicitly: `TestExtractPiContent_toolCall` and `TestExtractPiContent_toolResult`.

---

### Pitfall 7: Engine Not Wired for Pi Agent Type

**What goes wrong:**
The sync engine's `processFile` switch in `internal/sync/engine.go` (line 762) dispatches on `file.Agent`. If `AgentPi` is added to `types.go` but not added to this switch, every pi session file encountered produces `"unknown agent type: pi"` errors and the sync fails silently (the error is logged, the file is cached as failed, and the session never enters the database). The UI shows no pi sessions.

**Why it happens:**
There are four places that must all be updated together: (1) `AgentType` constant in `types.go`, (2) `classifyOnePath` in `engine.go`, (3) `processFile` switch in `engine.go`, (4) `syncAllLocked` discovery call in `engine.go`. Missing any one of them causes silent failure.

**How to avoid:**
Use the existing agents as a full checklist. Search for all occurrences of `AgentCursor` (the most recently added agent) and verify pi has a parallel entry at every location. The list: `internal/parser/types.go`, `internal/sync/engine.go` (3 locations), `internal/sync/discovery.go` (new `DiscoverPiSessions` function), and `internal/config/config.go` (`PiDir`/`PiDirs` fields + `loadEnv` + `ResolvePiDirs`).

**Warning signs:**
- Log line `"sync error: unknown agent type: pi"` after startup
- Sessions are discovered (log shows count) but none appear in UI
- `internal/sync/engine.go` has `AgentCursor` in switch but no `AgentPi`

**Phase to address:**
Integration/wiring phase. Write an integration test that seeds a temp pi session directory and verifies the session appears in the database after sync.

---

## Technical Debt Patterns

| Shortcut | Immediate Benefit | Long-term Cost | When Acceptable |
|----------|-------------------|----------------|-----------------|
| Reusing `ExtractTextContent` for pi blocks | Less code to write | Tool calls silently not extracted; debugging confusion later | Never — block types differ |
| Using filename stem as session ID instead of header ID | Matches Claude pattern exactly | Session ID doesn't match `parentId` references in entries; future branching support breaks | Never — read the header |
| Hardcoding `--path--` directory format from source without verifying on disk | Matches source code | Discovery fails on real user installs if format differs | Never — verify against real session files |
| Skipping v1 session support | Simpler parser | Old user sessions silently disappear | Acceptable only if confirmed no v1 sessions exist in the wild |
| Skipping `model_change`/`thinking_level_change` entries | Simpler parser | Sessions missing model metadata; confusing diffs from Claude where model is displayed | Acceptable for MVP if model display is not required in UI |

---

## Integration Gotchas

| Integration | Common Mistake | Correct Approach |
|-------------|----------------|------------------|
| Config loading (`config.go`) | Adding `PiDir` to Config struct but forgetting to add it to `loadEnv()`, `loadFile()`, and the `ResolvePiDirs()` resolver | Search all occurrences of `CursorProjectsDir` and add parallel `PiDir` code at every location |
| Engine `NewEngine` signature | Adding `piDirs []string` parameter but forgetting to pass it at the call site in `cmd/agentsview/main.go` | Grep for `sync.NewEngine(` and update that call |
| Watcher (`watcher.go`) | Not adding pi session directories to the file watcher, so live updates don't fire | Search for where Claude dirs are added to watcher and add pi dirs in the same block |
| Frontend agent filter | Adding `AgentPi` constant but not adding `"pi"` to the filter options in the Svelte SPA | Check `frontend/src/` for the agent filter component; follow the Cursor filter as the most recent example |
| FTS search | Pi messages are inserted to FTS but tool call content uses wrong field names (`toolCall` not `tool_use`) causing content extraction to return empty text | Verify that parsed messages from pi have non-empty `Content` field before insertion |

---

## Performance Traps

| Trap | Symptoms | Prevention | When It Breaks |
|------|----------|------------|----------------|
| Treating `toolResult` messages as standalone parsed messages | Message count inflated; FTS indexed with empty content strings | Pair toolResult back to its assistant turn rather than storing as independent message | At any scale — data model is wrong |
| Reading full pi session file to detect if it's valid | Slow discovery for large sessions | Read only first line to validate `{"type":"session",...}` header, same as pi's `isValidSessionFile` | Directories with many large session files |
| No mtime-based skip cache for pi sessions | Every sync re-parses all pi files | Use `shouldSkipByPath` (same as Codex/Copilot pattern) since pi session IDs aren't in filenames | At 100+ sessions |

---

## "Looks Done But Isn't" Checklist

- [ ] **Content extraction:** `ExtractPiContent` correctly maps `toolCall.arguments` to `InputJSON`, not `tool_use.input` — verify with `TestExtractPiContent_argumentsField`
- [ ] **Tool categorization:** `NormalizeToolCategory("grep")` returns `"Grep"` — verify with taxonomy test
- [ ] **Session ID:** Stored session ID in DB matches `header.id` from file, not filename stem — verify by querying DB after sync
- [ ] **toolResult pairing:** Assistant messages with tool calls have matching ToolResults — verify message structure with a multi-turn fixture
- [ ] **Discovery:** `DiscoverPiSessions` returns files from both `~/.pi/agent/sessions/` and custom `PI_DIR` env var — verify with config test
- [ ] **Version handling:** V1 session (no `version` field, no `id`/`parentId`) parses without error and returns messages in file order — verify with v1 fixture
- [ ] **Engine wired:** After adding `AgentPi`, all four engine locations updated — verify by grepping `AgentCursor` occurrences and confirming parallel `AgentPi` at every location
- [ ] **Watcher:** Pi dirs added to fsnotify watcher so live updates work during active sessions
- [ ] **Frontend filter:** Pi appears in agent filter dropdown — manual verification

---

## Recovery Strategies

| Pitfall | Recovery Cost | Recovery Steps |
|---------|---------------|----------------|
| toolCall blocks not extracted (used wrong field name) | LOW | Fix `ExtractPiContent`, add test, re-sync (ResyncAll wipes and rebuilds DB) |
| Wrong session ID (filename vs header) | MEDIUM | Fix parser, run ResyncAll to rebuild all sessions with correct IDs; any bookmarked session URLs break |
| Discovery finds no sessions | LOW | Fix `DiscoverPiSessions`, restart — no data corruption |
| Engine not wired for AgentPi | LOW | Add missing case to switch, restart — no data corruption |
| V1 sessions produce parse errors | LOW | Add version detection branch, ResyncAll |

---

## Pitfall-to-Phase Mapping

| Pitfall | Prevention Phase | Verification |
|---------|------------------|--------------|
| toolResult role misidentified | Parser phase — write fixtures with all role types | `TestParsePiSession_toolResultMessages` passes |
| Wrong directory encoding format | Discovery phase — test with real session directories | `TestDiscoverPiSessions` finds sessions in temp dir |
| V1 sessions without id/parentId | Parser phase — include v1 fixture | `TestParsePiSession_v1NoParentId` passes |
| Missing `grep` in taxonomy | Parser phase — taxonomy test | `TestNormalizeToolCategory` covers lowercase pi tools |
| Session ID from header not filename | Parser phase — assert ID equals header.id | `TestParsePiSession_sessionIDFromHeader` passes |
| toolCall/toolResult vs tool_use/tool_result | Parser phase — content extraction test | `TestExtractPiContent_toolCallBlock` and `TestExtractPiContent_toolResultBlock` pass |
| Engine not wired | Integration phase — seed temp dir, run sync, query DB | `TestPiSessionEndToEnd` finds session in DB |

---

## Sources

- `internal/parser/claude.go` — Claude parser structure; fork detection; linear vs DAG parsing; session ID from filename
- `internal/parser/content.go` — `ExtractTextContent` uses `tool_use`/`tool_result` block types (not pi's `toolCall`/`toolResult`)
- `internal/parser/taxonomy.go` — `NormalizeToolCategory` missing `"grep"` lowercase entry
- `internal/parser/types.go` — `AgentType` constants; must add `AgentPi`
- `internal/sync/engine.go` — `processFile` switch; all four wiring points
- `internal/config/config.go` — pattern for `loadEnv` + `ResolveDirs`; must be replicated for `PiDir`
- `internal/sync/discovery.go` — `DiscoverClaudeProjects` pattern to replicate
- `/Users/carze/Documents/personal/misc/pi-mono/packages/coding-agent/src/core/session-manager.ts` — authoritative pi format: `getDefaultSessionDir` encoding, `SessionHeader` (version optional), v1/v2/v3 migration, `toolCall`/`toolResult` block types
- `/Users/carze/.omp/agent/sessions/-Documents-personal-misc/2026-02-14T19-40-45-439Z_146eb832ffc34f07.jsonl` — real pi session file confirming: `role:"toolResult"` as standalone message, `type:"toolCall"` with `arguments` field, `type:"thinking"` with `thinkingSignature`, session ID format in header vs filename
- `.planning/codebase/CONCERNS.md` — known fragile areas: DB schema migration complexity, parser fork detection fragility, sync engine file handle management

---
*Pitfalls research for: pi-agent JSONL parser integration into agentsview*
*Researched: 2026-02-27*
