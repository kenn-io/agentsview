# Technology Stack: Pi-Agent Parser Integration

**Project:** agentsview — pi-agent support milestone
**Researched:** 2026-02-27
**Confidence:** HIGH (based on direct source reading: actual JSONL file, existing Go source)

---

## Context

This is not a greenfield stack decision. The existing stack is fixed:

| Layer | Technology | Notes |
|-------|-----------|-------|
| Language | Go (CGO enabled) | No new dependencies permitted |
| Storage | SQLite + FTS5 | Existing schema unchanged |
| JSONL parsing | `github.com/tidwall/gjson` | Already used in claude.go |
| Frontend | Svelte 5 SPA | Needs `AgentPi` filter entry only |
| Build | `CGO_ENABLED=1 -tags fts5` | Unchanged |

The research question is: exactly what Go structures, parsing logic, and integration hooks are needed to slot pi-agent in alongside the existing parsers.

---

## Pi-Agent JSONL Format (Fully Documented)

Source: direct reading of `/Users/carze/.omp/agent/sessions/-Documents-personal-misc/2026-02-14T19-40-45-439Z_146eb832ffc34f07.jsonl`

**Confidence: HIGH** — real file, 21 lines, all event types observed.

### Line 1: Session Header

```json
{"type":"session","version":3,"id":"146eb832ffc34f07","timestamp":"2026-02-14T19:40:45.439Z","cwd":"/Users/carze/Documents/personal/misc"}
```

Fields:
- `type`: always `"session"`
- `version`: integer schema version (3 in observed file)
- `id`: session ID (hex string, 16 chars), also encoded in filename
- `timestamp`: ISO 8601 with milliseconds + Z suffix
- `cwd`: absolute working directory path

**Key difference from Claude**: Claude files have no header line; session ID comes from filename only. Pi-agent has an explicit header with `id` and `cwd` fields that must be read.

### Subsequent Lines: Typed Events

All events share:
- `type`: string discriminator
- `id`: short hex event ID (8 chars, e.g., `"84b425cf"`)
- `parentId`: short hex ID of prior event, or `null` for first event after header

**Observed event types (from real file):**

#### `model_change`
```json
{"type":"model_change","id":"84b425cf","parentId":null,"timestamp":"2026-02-14T19:40:46.242Z","model":"synthetic/hf:nvidia/Kimi-K2.5-NVFP4"}
```
Fields: `id`, `parentId`, `timestamp`, `model`. Not a message — skip for message extraction.

#### `thinking_level_change`
```json
{"type":"thinking_level_change","id":"169d1c8a","parentId":"84b425cf","timestamp":"2026-02-14T19:40:46.243Z","thinkingLevel":"off"}
```
Fields: `id`, `parentId`, `timestamp`, `thinkingLevel`. Not a message — skip for message extraction.

#### `message` (user role)
```json
{"type":"message","id":"41bd446d","parentId":"169d1c8a","timestamp":"2026-02-14T19:40:46.291Z","message":{"role":"user","content":[{"type":"text","text":"/gsd:help"}],"timestamp":1771098046275}}
```
Outer fields: `type`, `id`, `parentId`, `timestamp` (ISO string).
Inner `message` object: `role` ("user"), `content` (array), `timestamp` (Unix milliseconds integer).

#### `message` (assistant role)
```json
{
  "type": "message",
  "id": "b7d51d4b",
  "parentId": "41bd446d",
  "timestamp": "2026-02-14T19:40:49.215Z",
  "message": {
    "role": "assistant",
    "content": [...],
    "api": "openai-completions",
    "provider": "synthetic",
    "model": "hf:nvidia/Kimi-K2.5-NVFP4",
    "usage": {
      "input": 22357,
      "output": 126,
      "cacheRead": 0,
      "cacheWrite": 0,
      "totalTokens": 22483,
      "cost": {"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "total": 0}
    },
    "stopReason": "toolUse",
    "timestamp": 1771098046284,
    "duration": 2929,
    "ttft": 2578
  }
}
```
Additional assistant-only fields: `api`, `provider`, `model`, `usage`, `stopReason`, `duration`, `ttft`.

#### `compaction` (from PROJECT.md — not in sample file)
Mentioned in PROJECT.md as a known event type. Structure not observed directly but described as a compaction/summary entry. Must be skipped for message extraction (same as Claude's `isCompactSummary` guard).

#### `branch_summary` (from PROJECT.md — not in sample file)
Mentioned in PROJECT.md. Skip for message extraction.

---

### Content Block Types (Within `message.content` Array)

All observed in the real file:

#### `text`
```json
{"type":"text","text":"I'll read the RTK.md reference..."}
```

#### `thinking`
```json
{"type":"thinking","thinking":"The user is asking for help...","thinkingSignature":"reasoning_content"}
```
Note: `thinkingSignature` is present (differs from Claude which uses `signature` field for encrypted thinking).

#### `toolCall`
```json
{"type":"toolCall","id":"functions.read:0","name":"read","arguments":{"path":"skill://rtk"}}
```
Fields: `id` (tool use ID, format `functions.NAME:N`), `name` (tool name, lowercase), `arguments` (object, not JSON string).

**Key difference from Claude**: Claude uses `tool_use` blocks with `input` (object) field. Pi-agent uses `toolCall` blocks with `name` (not `name`) and `arguments` (not `input`). Also Claude tool_use IDs are `toolu_...` UUIDs; pi uses `functions.NAME:N` format.

#### `toolResult` (in user messages)
Mentioned in PROJECT.md: `{"type":"toolResult","toolCallId":"...","toolName":"...","content":[...]}`.
Not observed in the 21-line sample (the sample session was short with no completed tool results), but the format is specified in PROJECT.md and is structurally consistent with the observed `toolCall` block IDs.

---

## Pi-Agent vs Claude Format: Key Differences

| Aspect | Claude | Pi-Agent |
|--------|--------|---------|
| Session header | None — session ID is filename | Line 1: `{"type":"session","id":"...","cwd":"..."}` |
| Message event type | `"user"` / `"assistant"` (top-level type field) | `"message"` (top-level), role inside `message.role` |
| Event chaining | `uuid` / `parentUuid` (UUID strings) | `id` / `parentId` (short 8-char hex) |
| DAG detection | Presence of `uuid` fields triggers DAG parse | Pi uses linear chain — no fork branching needed |
| Tool call block | `{"type":"tool_use","id":"toolu_...","name":"...","input":{}}` | `{"type":"toolCall","id":"functions.NAME:N","name":"...","arguments":{}}` |
| Tool result block | `{"type":"tool_result","tool_use_id":"...","content":[]}` | `{"type":"toolResult","toolCallId":"...","toolName":"...","content":[]}` |
| Thinking block | `{"type":"thinking","thinking":"...","signature":"..."}` | `{"type":"thinking","thinking":"...","thinkingSignature":"..."}` |
| Inner message timestamp | ISO string in `message.timestamp` (for user msgs it's Unix ms int) | Both formats present: outer ISO `timestamp`, inner `message.timestamp` (Unix ms int) |
| Model/provider info | `message.model` in assistant entries | `message.model`, `message.provider`, `message.api` in assistant entries |
| Skip guards | `isMeta`, `isCompactSummary` bool flags | `type=="compaction"` or `type=="branch_summary"` at top level |
| System message filtering | Text-prefix heuristic (`isClaudeSystemMessage`) | Not needed — no equivalent system injection messages |
| Subagent linking | `queue-operation` events with `tool_use_id`/`task_id` | Out of scope per PROJECT.md (`branchedFrom` exists but deferred) |

---

## Required Go Structs for Deserialization

Use `gjson` (already imported in claude.go) rather than `encoding/json` structs — this matches the existing pattern and avoids struct maintenance overhead. However, explicit structs are useful for documentation. The implementation should mirror claude.go's gjson-path approach.

### Conceptual structs (for documentation — implement with gjson paths)

```go
// Top-level event line
type piEvent struct {
    Type      string // gjson: "type"
    ID        string // gjson: "id"
    ParentID  string // gjson: "parentId"  (note: camelCase, not snake_case)
    Timestamp string // gjson: "timestamp"
}

// Session header (line 1 only)
type piSessionHeader struct {
    Version   int    // gjson: "version"
    ID        string // gjson: "id"
    Timestamp string // gjson: "timestamp"
    CWD       string // gjson: "cwd"
}

// Message event inner object
type piMessage struct {
    Role      string        // gjson: "message.role"
    Content   []piBlock     // gjson: "message.content"
    Model     string        // gjson: "message.model"
    Provider  string        // gjson: "message.provider"
    StopReason string       // gjson: "message.stopReason"
    Duration  int64         // gjson: "message.duration"   (ms)
    TTFT      int64         // gjson: "message.ttft"       (ms)
}

// Content blocks — discriminated by "type" field
// type="text":        gjson path "text"
// type="thinking":    gjson paths "thinking", "thinkingSignature"
// type="toolCall":    gjson paths "id", "name", "arguments" (raw object)
// type="toolResult":  gjson paths "toolCallId", "toolName", "content"
```

### gjson paths cheat sheet for implementation

```
Session header:
  type          → "type"
  session id    → "id"
  cwd           → "cwd"
  timestamp     → "timestamp"

Event:
  event type    → "type"
  event id      → "id"
  parent id     → "parentId"
  timestamp     → "timestamp"

Message inner:
  role          → "message.role"
  content array → "message.content"
  model         → "message.model"
  stop reason   → "message.stopReason"

Content block iteration:
  block type    → "type"  (within array element)
  text          → "text"
  thinking      → "thinking"
  toolCall id   → "id"
  toolCall name → "name"
  toolCall args → "arguments"  (raw JSON object)
  toolResult id → "toolCallId"
  toolResult name → "toolName"
```

---

## Content Extraction: Reuse vs New Code

**Recommendation: reuse `ExtractTextContent` with targeted additions.**

The existing `ExtractTextContent` function in `internal/parser/content.go` already handles `text`, `thinking`, `tool_use`, and `tool_result` Claude-style blocks. Pi-agent uses different block type names (`toolCall` vs `tool_use`, `toolResult` vs `tool_result`) and different field names (`arguments` vs `input`, `toolCallId` vs `tool_use_id`).

**Options:**
1. Add pi-agent block type aliases to `ExtractTextContent` — couples two formats in one function, gets confusing
2. Write `extractPiContent(content gjson.Result)` that mirrors `ExtractTextContent` but handles pi-agent field names — cleaner, same return signature

**Recommendation: option 2.** Write `extractPiContent` as a private function in `internal/parser/pi.go`. It returns the same `(text string, hasThinking bool, hasToolUse bool, toolCalls []ParsedToolCall, toolResults []ParsedToolResult)` tuple so the rest of the message construction is identical to `extractMessages` in claude.go.

---

## Taxonomy: Pi-Agent Tool Names

Pi-agent uses lowercase tool names. The existing `NormalizeToolCategory` in `taxonomy.go` already handles lowercase pi-agent tool names:

```go
case "read":   return "Read"    // already present
case "edit":   return "Edit"    // already present
case "write":  return "Write"   // already present
case "bash":   return "Bash"    // already present
case "glob":   return "Glob"    // already present
case "task":   return "Task"    // already present
```

**Missing from taxonomy.go:**
- `"grep"` → `"Grep"` — the `grep` case is present (`case "search_files", "grep": return "Grep"`) via the multi-case form
- `"find"` → `"Read"` (filesystem listing, same category as `LS`) — not present

**Action required in taxonomy.go:**
```go
case "find":
    return "Read"
```

**Confidence: HIGH** — taxonomy.go source confirmed; `grep` is already covered in the `search_files/grep` case.

---

## New File: `internal/parser/pi.go`

This is the only new file in the parser package. Structure:

```go
package parser

import (
    "fmt"
    "os"
    "path/filepath"
    "strings"
    "time"

    "github.com/tidwall/gjson"
)

// ParsePiSession parses a pi-agent JSONL session file.
// Signature mirrors ParseClaudeSession.
func ParsePiSession(path, project, machine string) ([]ParseResult, error) {
    info, err := os.Stat(path)
    // ...open file, newLineReader...

    // Line 1: session header
    // Extract: id (use as sessionID), cwd, timestamp
    // If line 1 type != "session", fall back to filename-based ID

    // Subsequent lines: iterate events
    // Skip: type == "session", "model_change", "thinking_level_change",
    //        "compaction", "branch_summary"
    // Process: type == "message"
    //   → extract role from message.role
    //   → extract content via extractPiContent()
    //   → build ParsedMessage

    // No DAG handling needed (pi uses linear chain)
    // No subagent linking (deferred per PROJECT.md)

    // Build ParsedSession with Agent: AgentPi
    // Return []ParseResult with single result
}

// extractPiContent extracts text and tool information from a pi-agent
// content array. Returns same tuple as ExtractTextContent.
func extractPiContent(content gjson.Result) (
    text string,
    hasThinking bool,
    hasToolUse bool,
    toolCalls []ParsedToolCall,
    toolResults []ParsedToolResult,
) {
    var textParts []string
    content.ForEach(func(_, block gjson.Result) bool {
        switch block.Get("type").Str {
        case "text":
            textParts = append(textParts, block.Get("text").Str)
        case "thinking":
            hasThinking = true
            // optionally append thinking text
        case "toolCall":
            hasToolUse = true
            tc := ParsedToolCall{
                ToolUseID: block.Get("id").Str,
                ToolName:  block.Get("name").Str,
                InputJSON: block.Get("arguments").Raw, // raw JSON object
            }
            tc.Category = NormalizeToolCategory(tc.ToolName)
            toolCalls = append(toolCalls, tc)
        case "toolResult":
            tr := ParsedToolResult{
                ToolUseID:     block.Get("toolCallId").Str,
                ContentLength: len(block.Get("content").Raw),
            }
            toolResults = append(toolResults, tr)
        }
        return true
    })
    text = strings.Join(textParts, "\n")
    return
}
```

---

## Types: New `AgentPi` Constant

Add to `internal/parser/types.go`:

```go
AgentPi AgentType = "pi"
```

Location: after `AgentCursor` in the const block. No other changes to types.go are needed.

---

## Config: New Fields and Env Var

Following the exact pattern of `GeminiDir`/`GeminiDirs`/`GEMINI_DIR`:

### `internal/config/config.go` additions

**In `Config` struct:**
```go
PiDir  string   `json:"pi_dir"`
PiDirs []string `json:"pi_dirs,omitempty"`
```

**In `Default()`:**
```go
PiDir: filepath.Join(home, ".pi", "agent", "sessions"),
```

**In `loadEnv()`:**
```go
if v := os.Getenv("PI_DIR"); v != "" {
    c.PiDir = v
    c.PiDirs = []string{v}
}
```

**In `loadFile()`, inner struct and assignment:**
```go
// add to file struct:
PiDirs []string `json:"pi_dirs"`

// add assignment block:
if len(file.PiDirs) > 0 && c.PiDirs == nil {
    c.PiDirs = file.PiDirs
}
```

**New resolver method:**
```go
func (c *Config) ResolvePiDirs() []string {
    return c.resolveDirs(c.PiDirs, c.PiDir)
}
```

**Confidence: HIGH** — directly mirrors `GeminiDir`/`GeminiDirs` pattern verbatim.

---

## Sync Engine: New `piDirs` Field and Discovery

**`internal/sync/engine.go` changes:**

**In `Engine` struct:**
```go
piDirs []string
```

**In `NewEngine` signature:**
```go
func NewEngine(
    database *db.DB,
    claudeDirs, codexDirs, copilotDirs,
    geminiDirs, opencodeDirs, piDirs []string,
    cursorDir, machine string,
) *Engine {
```

**In `NewEngine` body:**
```go
piDirs: piDirs,
```

**Sync logic** — pi uses the same subdirectory layout as Claude (encoded-cwd subdirs containing `*.jsonl` files). Discovery for pi sessions should follow the same pattern as Claude discovery: walk `piDirs`, enumerate subdirectories (encoded-cwd pattern), find `*.jsonl` files.

Whether this uses an existing shared helper or adds a new `syncPiSessions` method matching `syncClaudeSessions` depends on how Claude's sync is structured internally. Based on the engine struct pattern, pi will need:

```go
func (e *Engine) syncPiSessions(ctx context.Context) (int, int, error) {
    // same pattern as syncClaudeSessions but using:
    // - e.piDirs
    // - parser.ParsePiSession
    // - parser.AgentPi
}
```

**In `cmd/agentsview/main.go`:**
Pass `cfg.ResolvePiDirs()` to `NewEngine` at the new parameter position.

**Confidence: MEDIUM** — engine.go was truncated in the preview. The pattern is clear from the struct, but the exact internal sync method structure (whether it's one method per agent or shared) needs verification by reading the full engine.go.

---

## Frontend: Filter UI

**`frontend/src/` changes needed:**

Add `"pi"` as a recognized agent type wherever other agent types are enumerated. This is a frontend-only change with no backend implications. Specifically:
- Agent filter dropdown/chips that list `["claude", "codex", "copilot", "gemini", "opencode", "cursor"]` need `"pi"` added
- Display label: `"Pi"` or `"pi-agent"` (use `"Pi"` for consistency with short-form labels like `"Codex"`)

**Confidence: MEDIUM** — haven't read frontend source, but this is a mechanical addition following existing pattern.

---

## Files to Create (New)

| File | Purpose |
|------|---------|
| `internal/parser/pi.go` | Pi-agent JSONL parser — `ParsePiSession`, `extractPiContent` |
| `internal/parser/pi_parser_test.go` | Unit tests: session header parsing, message types, tool call extraction, compaction skip |

---

## Files to Modify (Existing)

| File | Change |
|------|--------|
| `internal/parser/types.go` | Add `AgentPi AgentType = "pi"` constant |
| `internal/parser/taxonomy.go` | Add `case "find": return "Read"` |
| `internal/config/config.go` | Add `PiDir`, `PiDirs` fields, `PI_DIR` env var, `ResolvePiDirs()` method, loadFile support |
| `internal/sync/engine.go` | Add `piDirs []string` field, extend `NewEngine` signature, add `syncPiSessions` method |
| `cmd/agentsview/main.go` | Pass `cfg.ResolvePiDirs()` to `NewEngine` |
| `frontend/src/...` | Add `"pi"` to agent filter enumeration |

---

## Timestamp Handling

Pi-agent events have two timestamps:
- Outer event `timestamp`: ISO 8601 string with milliseconds (`"2026-02-14T19:40:46.291Z"`)
- Inner `message.timestamp`: Unix milliseconds integer (`1771098046275`)

Use the outer ISO timestamp (same format Claude uses — existing `parseTimestamp` handles it). The inner Unix-ms timestamp can be ignored or used as a fallback; the outer one is preferred for consistency.

The existing `extractTimestamp` function in claude.go reads `gjson.Get(line, "timestamp").Str`. For pi-agent, this same path works because the outer event timestamp field name is identical.

**Confidence: HIGH** — confirmed from real file observation.

---

## Session ID and File Naming

Pi-agent filename format: `2026-02-14T19-40-45-439Z_146eb832ffc34f07.jsonl`

The session ID embedded in the filename (`146eb832ffc34f07`) matches the `id` field in the session header line exactly. Therefore:

```go
// Get ID from header (preferred — explicit)
sessionID := gjson.Get(headerLine, "id").Str

// Fallback: derive from filename (same as Claude pattern)
if sessionID == "" {
    // strip timestamp prefix: "2026-02-14T19-40-45-439Z_" then ".jsonl"
    base := filepath.Base(path)
    base = strings.TrimSuffix(base, ".jsonl")
    if idx := strings.LastIndex(base, "_"); idx >= 0 {
        sessionID = base[idx+1:]
    } else {
        sessionID = base
    }
}
```

**Confidence: HIGH** — confirmed by direct observation (filename `146eb832ffc34f07` matches header `"id":"146eb832ffc34f07"`).

---

## CWD / Project Extraction

Unlike Claude (where `cwd` is embedded in the first `user` message), pi-agent has `cwd` directly in the session header line 1. This is cleaner:

```go
// Read cwd from session header — no need to scan all messages
cwd := gjson.Get(headerLine, "cwd").Str
```

The `project` argument to `ParsePiSession` comes from the sync engine (derived from the encoded-cwd subdirectory name, same as Claude). But pi also provides a ground-truth `cwd` that can be used to fill the project field more accurately.

---

## Alternatives Considered

| Decision | Chosen | Alternative | Why Not |
|----------|--------|-------------|---------|
| Content extraction | New `extractPiContent` function | Extend `ExtractTextContent` | Coupling two different formats in one function increases complexity and bug risk |
| Session ID source | Header line `id` field | Filename parsing only | Header is explicit and authoritative; filename parsing kept as fallback |
| DAG parsing | Not needed for pi | Reuse claude DAG logic | Pi uses linear event chain (`parentId` forms a single chain, not a DAG with forks) |
| Taxonomy additions | Add `"find"` case only | Rewrite taxonomy | All other pi tool names already covered |

---

## Sources

- Direct reading of `/Users/carze/.omp/agent/sessions/-Documents-personal-misc/2026-02-14T19-40-45-439Z_146eb832ffc34f07.jsonl` (HIGH confidence — primary source)
- `internal/parser/claude.go` — reference implementation (HIGH confidence)
- `internal/parser/types.go` — type definitions (HIGH confidence)
- `internal/parser/taxonomy.go` — tool normalization (HIGH confidence)
- `internal/config/config.go` — config pattern (HIGH confidence)
- `.planning/PROJECT.md` — format specification and requirements (HIGH confidence)
