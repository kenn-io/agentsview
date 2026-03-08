# Feature Research

**Domain:** Pi-agent support for agentsview (adding a new agent parser to an existing multi-agent session viewer)
**Researched:** 2026-02-27
**Confidence:** HIGH — based on direct source inspection of pi-mono session-manager.ts, types.ts, messages.ts, and a 2.3MB real session fixture

## Feature Landscape

### Table Stakes (Users Expect These)

Features required to make pi-agent support feel complete. These are the minimum bar — users of agentsview
already have full Claude parity and will expect the same for pi-agent.

| Feature | Why Expected | Complexity | Notes |
|---------|--------------|------------|-------|
| Session header parsing | Every session needs metadata (id, cwd, timestamp, agent type) | LOW | Line 1 of each JSONL: `{"type":"session","version":3,"id":"...","timestamp":"...","cwd":"..."}`. Extract `id`, `timestamp` (ISO 8601), `cwd`. `parentSession` field maps to `ParentSessionID`. |
| User message parsing | Core content — the human turns in a conversation | LOW | Entry type `"message"` with `message.role == "user"`. Content is `string` or `(TextContent | ImageContent)[]`. Map text blocks only; skip image blocks. Nested under `message.content[].type == "text"`. |
| Assistant message parsing | Core content — what the agent produced | LOW | Entry type `"message"` with `message.role == "assistant"`. Content is `(TextContent | ThinkingContent | ToolCall)[]`. Contains `model`, `provider`, `usage`, `stopReason`, `timestamp` (Unix ms). |
| Thinking block extraction | Users of pi-agent with thinking-capable models expect thinking to be visible | LOW | Content block `{"type":"thinking","thinking":"...","thinkingSignature":"..."}`. Sets `HasThinking=true`. Map identical to Claude's thinking extraction. Optional `redacted: bool` field — redacted thinking has no text, only a signature. |
| Tool call extraction | Agent is tool-heavy; not showing tool calls makes sessions unreadable | LOW | Content block `{"type":"toolCall","id":"...","name":"...","arguments":{...}}`. Maps to `ParsedToolCall`. `id` is the tool use ID. `name` is lowercase (read, write, edit, bash, grep, glob, find). `arguments` replaces Claude's `input`. |
| Tool result extraction | Pairs with tool calls to show what tools returned | LOW | Entry type `"message"` with `message.role == "toolResult"`. Fields: `toolCallId`, `toolName`, `content: (TextContent|ImageContent)[]`, `isError: bool`, `timestamp`. Maps to `ParsedToolResult`. |
| Tool taxonomy mapping | Tools must display in correct categories in the UI | LOW | Pi tool names are lowercase (`read`, `write`, `edit`, `bash`, `grep`, `glob`, `find`). taxonomy.go already handles these (lines 37–48). `find` maps to "Other" currently — add "find" → "Read" (it searches the filesystem). |
| `AgentPi` constant | Without this, the session cannot be stored or filtered | LOW | Add `AgentPi AgentType = "pi"` to types.go. Mirrors existing pattern for all 6 agents. |
| Config: `PI_DIR` env var | Users need a way to point agentsview at their sessions | LOW | Mirrors `COPILOT_DIR`, `GEMINI_DIR` pattern. Default `~/.pi/agent/sessions/`. Oh-my-pi users override to `~/.omp/agent/sessions/`. |
| Session directory discovery | Sync engine must find pi session files | LOW | Subdirectory pattern: `--{cwd-encoded}--/` (note double-dash prefix+suffix vs Claude's single-dash prefix only). Files: `{ISO-datetime}_{uuid}.jsonl`. Mirrors Claude discovery but with different dir name encoding. |
| FTS5 text indexing | Sessions must be searchable | LOW | Use existing FTS5 pipeline. Parser outputs `Content` text field from all user/assistant text blocks. No new DB work required. |
| Filter UI for pi agent | Users need to filter by agent type in the frontend | LOW | Frontend already has agent filter. Adding `AgentPi` to the backend constant is sufficient — Svelte filter component reads agent types from API response. |
| Session `FirstMessage` extraction | Session list needs a preview of what was discussed | LOW | Scan messages for first user-role entry with non-empty text content. Truncate to 300 chars. |
| Timestamp handling | Sessions need `StartedAt`/`EndedAt` for sorting and display | LOW | Two timestamp sources: entry-level `timestamp` (ISO 8601 string in entry wrapper) and `message.timestamp` (Unix ms integer). Use entry-level `timestamp` field for ordering; fall back to message timestamp. |
| Compaction entry as synthetic message | Compaction collapses history; users need to see that this happened | MEDIUM | Entry type `"compaction"` has `summary`, `tokensBefore`, `firstKeptEntryIndex` (v1 fixture uses index not ID). Represent as a synthetic user message with role `"user"` and formatted content like `[Compaction: {tokensBefore} tokens → summary]`. Does not map 1:1 to any existing message type but follows the pattern used for Claude's `isCompactSummary` handling. |

### Differentiators (Pi-Specific Features Worth Highlighting)

| Feature | Value Proposition | Complexity | Notes |
|---------|-------------------|------------|-------|
| Provider + model metadata per message | Pi supports many LLMs (Anthropic, OpenAI, Google, Bedrock, etc.) per message; this is richer metadata than Claude (which is always Anthropic) | LOW | Each assistant message has `message.api`, `message.provider`, `message.model`. Store as-is in the content text or as session-level metadata. For v1, surface model in the session detail view via Content. |
| Cost tracking per message | `usage.cost.total` is in every assistant message | LOW | Pi computes cost per-message. No other agent in agentsview exposes this. Not in `ParsedMessage` today — defer to later phase; note in session content if desired. |
| Multi-provider thinking blocks | Pi's thinking support works across Anthropic AND OpenAI reasoning (thinkingSignature is provider-agnostic) | LOW | Parsing is identical to Claude thinking. Redacted thinking blocks (thinking field empty, signature present) should still set `HasThinking=true` so the UI shows the indicator even without content. |
| `branchedFrom` in session header | Pi explicitly records the source file path when a branch is created | MEDIUM | Session header has `branchedFrom: string` (full file path to the parent JSONL file). Use this to populate `ParentSessionID` by extracting the UUID from the filename. More reliable than Claude's `parentSession` field. |
| `model_change` event | User changed the model mid-session | LOW | Entry type `"model_change"` has `provider`, `modelId`. Represent as a system/meta message in the content string: `[Model changed to {provider}/{modelId}]`. This makes session context clear when reading back. |
| `thinking_level_change` event | User changed thinking budget mid-session | LOW | Entry type `"thinking_level_change"` has `thinkingLevel: "off" | "minimal" | "low" | "medium" | "high" | "xhigh"`. Represent as meta message: `[Thinking level changed to {thinkingLevel}]`. |

### Anti-Features (Do Not Build in v1)

| Feature | Why Requested | Why Problematic | Alternative |
|---------|---------------|-----------------|-------------|
| Branch/subagent session linking via `branchedFrom` | Pi has `branchedFrom` path in the header — could automatically link related sessions | `branchedFrom` is an absolute file path on the original machine. It will not match on a different machine or after file moves. Resolving it correctly requires cross-machine path normalization, and linking UI adds complexity not justified for v1. | Populate `ParentSessionID` only when `branchedFrom` points to a file that already exists in the DB with a known ID. Leave unresolved references empty. |
| `custom`, `custom_message`, `label`, `session_info` entry parsing | These appear in the format spec | These are extension-specific or UI-only entries. `custom`/`custom_message` are injected by app-level extensions. `label` is a TUI label for branching. `session_info` holds an optional human-readable session name. None appear in the actual fixture sessions. | Skip gracefully (ignore unknown type). If `session_info.name` appears, it could populate a display name field later — defer. |
| Image content block rendering | UserMessage and ToolResultMessage content can include `ImageContent` blocks (`{type:"image", data, mimeType}`) | Images are base64-encoded in the JSONL — potentially megabytes per block. Storing them in SQLite or rendering them in the current UI architecture is not designed for this. | Skip image blocks in text extraction. Do not include in `Content` string. Set a flag if needed in future. For v1, text-only. |
| `bashExecution` custom message type | Pi has a custom `bashExecution` role for `!`-prefix shell commands run directly in the TUI | Not a standard `message` entry — only occurs in custom_message entries from the TUI mode. Not present in coding-agent JSONL files. | Skip. The tool call / tool result mechanism covers the same content for the coding agent. |
| Real-time file watching for active pi sessions | Pi sessions grow during active use | The sync engine already handles this via 15-minute periodic sync + startup scan. Adding file watch during active pi runs needs per-file tail logic. | Accept up-to-15-min delay. Users doing post-session review are the primary audience. |
| HTML export for pi sessions | Claude sessions have export support | Export infra exists but pi-specific formatting (provider info, cost) would need a new template. Adds scope without clear demand. | Can reuse existing export pipeline later with minimal changes once pi parser is stable. |

## Feature Dependencies

```
AgentPi constant
    └──required by──> Session header parsing
                          └──required by──> All message parsing
                                                └──required by──> FTS5 indexing
                                                └──required by──> Filter UI

PI_DIR config
    └──required by──> Session directory discovery
                          └──required by──> Sync engine integration

Tool call extraction
    └──required by──> Tool taxonomy mapping (find → Read addition)

Compaction entry
    └──enhances──> Session completeness (sessions with compaction are unreadable without it)

branchedFrom (header)
    └──enhances──> ParentSessionID population (optional, opportunistic)

model_change event ──enhances──> Session readability (context for model switches)
thinking_level_change event ──enhances──> Session readability (context for budget changes)
```

### Dependency Notes

- **AgentPi constant required before all parsing:** The constant is referenced by ParsedSession.Agent. Without it, the session cannot be stored.
- **Session header parsing required before message parsing:** The header provides session ID, cwd, and parentSession — all needed to construct ParsedSession. Message parsing builds on a valid session context.
- **PI_DIR config required before sync discovery:** The sync engine walks configured directories. If no config path is available, no files are discovered.
- **Tool taxonomy mapping depends on tool call extraction:** Tool calls must be extracted first; taxonomy normalization is a post-processing step on the extracted name.
- **Compaction entry is independent but improves completeness:** Sessions that have undergone compaction are technically parseable without it, but the context shown will be confusing — the conversation appears to start mid-stream. The compaction synthetic message explains the discontinuity.

## MVP Definition

### Launch With (v1)

Minimum required for pi-agent to work in agentsview at Claude-level quality.

- [ ] `AgentPi` constant in `internal/parser/types.go` — without this nothing stores
- [ ] Session header parsing (id, cwd, timestamp, parentSession, branchedFrom) — required to build ParsedSession
- [ ] User message parsing: `message.role == "user"` with text content array — core content
- [ ] Assistant message parsing: `message.role == "assistant"` with text, thinking, toolCall content blocks — core content
- [ ] Tool result parsing: `message.role == "toolResult"` — needed for tool call pairing in FTS
- [ ] Thinking block extraction (including redacted thinking → HasThinking=true) — expected by thinking-model users
- [ ] Tool call extraction with `arguments` field (not `input` as in Claude) — essential for tool rendering
- [ ] `find` tool → "Read" category in taxonomy.go — incomplete taxonomy breaks tool display
- [ ] Compaction entry as synthetic user message — without it, compacted sessions are confusing
- [ ] `model_change` and `thinking_level_change` as meta messages — these appear in real fixtures; silently dropping them loses session context
- [ ] `PI_DIR` / `PiDirs` config with `~/.pi/agent/sessions/` default — needed for discovery
- [ ] Session directory discovery for `--{cwd}--/` subdirectory pattern — needed to find files
- [ ] Unit tests: session header, user/assistant/toolResult messages, tool call extraction, thinking blocks, compaction — project convention requires tests for all new code

### Add After Validation (v1.x)

- [ ] `session_info.name` support — expose human-readable session names once the entry type is confirmed to appear in real-world files
- [ ] `branchedFrom` resolution against existing DB sessions — deferred because path matching is unreliable cross-machine but feasible once session import is stable
- [ ] Cost display per session — `usage.cost.total` sum across assistant messages; requires a new DB field or derived computation

### Future Consideration (v2+)

- [ ] HTML export for pi sessions — reuse existing export infra, low effort once v1 is stable
- [ ] Image content block handling — requires UI changes and storage decisions beyond current architecture
- [ ] `bashExecution` custom message display — only relevant if pi TUI mode sessions start being stored as custom_message entries
- [ ] Real-time file watching during active pi runs — sync-on-startup covers the primary use case

## Feature Prioritization Matrix

| Feature | User Value | Implementation Cost | Priority |
|---------|------------|---------------------|----------|
| AgentPi constant | HIGH | LOW | P1 |
| Session header parsing | HIGH | LOW | P1 |
| User/assistant/toolResult message parsing | HIGH | LOW | P1 |
| Thinking block extraction | HIGH | LOW | P1 |
| Tool call extraction (arguments field) | HIGH | LOW | P1 |
| find tool → Read taxonomy | MEDIUM | LOW | P1 |
| Compaction as synthetic message | HIGH | MEDIUM | P1 |
| model_change / thinking_level_change meta | MEDIUM | LOW | P1 |
| PI_DIR config | HIGH | LOW | P1 |
| Session directory discovery | HIGH | LOW | P1 |
| Unit tests | HIGH | MEDIUM | P1 |
| session_info.name support | LOW | LOW | P2 |
| branchedFrom resolution | MEDIUM | MEDIUM | P2 |
| Cost display | MEDIUM | MEDIUM | P2 |
| HTML export | LOW | LOW | P3 |
| Image content handling | LOW | HIGH | P3 |
| bashExecution display | LOW | MEDIUM | P3 |

**Priority key:**
- P1: Must have for launch
- P2: Should have, add when possible
- P3: Nice to have, future consideration

## Complete Entry Type Reference

This section documents every entry type in the pi-agent JSONL format for implementer reference.

### Line 1: Session Header

```json
{
  "type": "session",
  "version": 3,
  "id": "ffae836b-9420-4060-ac13-7745215f90ff",
  "timestamp": "2025-12-09T00:53:29.825Z",
  "cwd": "/Users/user/workspaces/myproject",
  "parentSession": "optional-parent-session-id",
  "branchedFrom": "/Users/user/.pi/agent/sessions/--path--/2025-12-09T00-52-54-397Z_uuid.jsonl"
}
```

Notes: `version` may be absent in v1 sessions. `parentSession` and `branchedFrom` are optional. `branchedFrom` is a full absolute file path.

### Entry: `message` (most common)

All message entries share the wrapper:
```json
{
  "type": "message",
  "id": "entry-uuid",
  "parentId": "previous-entry-uuid-or-null",
  "timestamp": "2025-12-08T22:41:05.306Z",
  "message": { ... }
}
```

#### User message (`message.role == "user"`)
```json
{
  "role": "user",
  "content": [{"type": "text", "text": "user's message text"}],
  "timestamp": 1765233665292
}
```
Content is `string` or `(TextContent | ImageContent)[]`. `timestamp` is Unix ms.

#### Assistant message (`message.role == "assistant"`)
```json
{
  "role": "assistant",
  "content": [
    {"type": "thinking", "thinking": "...", "thinkingSignature": "...", "redacted": false},
    {"type": "text", "text": "..."},
    {"type": "toolCall", "id": "toolu_...", "name": "bash", "arguments": {"command": "..."}}
  ],
  "api": "anthropic-messages",
  "provider": "anthropic",
  "model": "claude-opus-4-5",
  "usage": {
    "input": 2775, "output": 141, "cacheRead": 0, "cacheWrite": 0,
    "totalTokens": 2916,
    "cost": {"input": 0.01, "output": 0.003, "cacheRead": 0, "cacheWrite": 0, "total": 0.013}
  },
  "stopReason": "toolUse",
  "timestamp": 1765233665294,
  "errorMessage": "optional error string when stopReason is error or aborted"
}
```
`stopReason` values: `"stop"`, `"length"`, `"toolUse"`, `"error"`, `"aborted"`.

#### Tool result message (`message.role == "toolResult"`)
```json
{
  "role": "toolResult",
  "toolCallId": "toolu_01GDop9s8DBp8sZnT9Wpy9Cy",
  "toolName": "bash",
  "content": [{"type": "text", "text": "Switched to a new branch 'refactor'\n"}],
  "isError": false,
  "timestamp": 1765234031893
}
```

### Entry: `thinking_level_change`

```json
{
  "type": "thinking_level_change",
  "id": "entry-uuid",
  "parentId": "...",
  "timestamp": "2025-12-08T22:45:42.397Z",
  "thinkingLevel": "minimal"
}
```
`thinkingLevel` values: `"off"`, `"minimal"`, `"low"`, `"medium"`, `"high"`, `"xhigh"`.

### Entry: `model_change`

```json
{
  "type": "model_change",
  "id": "entry-uuid",
  "parentId": "...",
  "timestamp": "...",
  "provider": "anthropic",
  "modelId": "claude-opus-4-5"
}
```

### Entry: `compaction`

```json
{
  "type": "compaction",
  "id": "entry-uuid",
  "parentId": "...",
  "timestamp": "2025-12-08T23:22:54.411Z",
  "summary": "# Context Checkpoint: ...",
  "firstKeptEntryIndex": 293,
  "tokensBefore": 175004
}
```

Note: The fixture uses `firstKeptEntryIndex` (integer, index into the linear entry list), NOT `firstKeptEntryId` (string) as defined in the TypeScript interface. The spec says `firstKeptEntryId` but the real data uses index. Parser must handle both. The `summary` field is Markdown text generated by the LLM.

### Entry: `branch_summary`

```json
{
  "type": "branch_summary",
  "id": "entry-uuid",
  "parentId": "...",
  "timestamp": "...",
  "fromId": "entry-uuid-of-branch-point",
  "summary": "..."
}
```

Not observed in the fixture but defined in the TypeScript interface. Skip gracefully if encountered; represent as a synthetic user message with content `[Branch summary: {summary}]` if desired.

### Entries to Skip Gracefully

| Type | Action |
|------|--------|
| `custom` | Skip — extension-specific, not in fixture |
| `custom_message` | Skip — TUI-mode only, not in fixture |
| `label` | Skip — TUI branching UI label |
| `session_info` | Skip for now; could populate session name later |

## Mapping to ParsedMessage Fields

| ParsedMessage field | Source in pi-agent |
|---------------------|-------------------|
| `Ordinal` | Sequential counter across kept entries |
| `Role` | `message.role` → map `"user"` to `RoleUser`, `"assistant"` to `RoleAssistant`; `"toolResult"` treated as `RoleUser` (matches Claude pattern) |
| `Content` | Concatenation of text blocks, thinking blocks (wrapped in [Thinking]...[/Thinking]), tool call summaries, meta messages |
| `Timestamp` | Entry-level `timestamp` field (ISO 8601 string), fall back to `message.timestamp` (Unix ms) |
| `HasThinking` | True if any `thinking` content block present (including redacted) |
| `HasToolUse` | True if any `toolCall` content block present |
| `ContentLength` | `len(Content)` after all text is assembled |
| `ToolCalls` | From `toolCall` blocks: `ToolUseID=id`, `ToolName=name`, `Category=NormalizeToolCategory(name)`, `InputJSON=json(arguments)` |
| `ToolResults` | From `toolResult` messages: `ToolUseID=toolCallId`, `ContentLength=sum(len(content[].text))` |

## Sources

- `pi-mono/packages/coding-agent/src/core/session-manager.ts` — source of truth for JSONL format (interfaces: `SessionHeader`, `SessionEntry`, `CompactionEntry`, `BranchSummaryEntry`, etc.)
- `pi-mono/packages/ai/src/types.ts` — `TextContent`, `ThinkingContent`, `ToolCall`, `UserMessage`, `AssistantMessage`, `ToolResultMessage` types
- `pi-mono/packages/agent/src/types.ts` — `AgentMessage`, `ThinkingLevel` union type
- `pi-mono/packages/coding-agent/src/core/messages.ts` — `BashExecutionMessage`, `CustomMessage`, `CompactionSummaryMessage`, `BranchSummaryMessage`
- `pi-mono/packages/coding-agent/test/fixtures/before-compaction.jsonl` — 2.3MB real session fixture confirming actual field names (note: `firstKeptEntryIndex` vs spec's `firstKeptEntryId`)
- `agentsview/internal/parser/types.go` — existing `ParsedSession`, `ParsedMessage`, `ParsedToolCall` types to map into
- `agentsview/internal/parser/taxonomy.go` — existing tool name normalization (lowercase pi tools already handled at lines 37–48; `find` missing)
- `agentsview/internal/parser/content.go` — existing `ExtractTextContent` function (handles text, thinking, tool_use, tool_result blocks — pi uses same structure)

---
*Feature research for: pi-agent support in agentsview*
*Researched: 2026-02-27*
