# Phase 1: Parser Foundation - Research

**Researched:** 2026-02-27
**Domain:** Go JSONL parser — pi-agent session format, existing parser patterns, taxonomy extension
**Confidence:** HIGH

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions

#### Test fixture strategy
- Use committed fixture files in `internal/parser/testdata/pi/`
- Fixture is synthesized from the spec (no real pi files required), covering all required message types
- Fixture includes both happy-path entries AND intentionally malformed/edge-case lines to test error handling in a single file
- Tests must pass in CI without pi installed

#### Error handling
- Malformed/invalid JSON lines: skip silently (matches existing Claude parser behavior)
- Unrecognized block types inside assistant messages: skip silently
- Unrecognized top-level entry types (e.g., `thinking_level_change`): skip silently
- Consistent philosophy: no noise, no partial failures, no log output from the parser itself

#### Compaction message content
- Compaction entries produce a synthetic user message whose content is the raw `summary` field
- If summary is missing or empty: use fallback text (e.g., `"[session compacted]"`) to preserve timeline continuity
- Model change entries produce a meta message with a descriptive sentence: `"Model changed to {model_name}"`

#### V1 fallback
- V1 detection: if ANY entry in the file has an `id` field, treat the whole file as V2. V1 mode only for pure V1 files (no id/parentId/version anywhere)
- V1 session ID: filename basename without extension (same as Claude parser pattern)
- `branchedFrom` → `ParentSessionID`: store it always, regardless of whether the parent exists in the DB yet

### Claude's Discretion
- Exact fallback text wording for empty compaction summary
- Internal struct layout and helper function organization within pi.go
- Line reader reuse vs. reimplementation (can reuse existing `newLineReader`)

### Deferred Ideas (OUT OF SCOPE)

None — discussion stayed within phase scope.
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| PRSR-01 | Pi JSONL sessions parsed into `ParsedSession` with correct id, Project, Agent=AgentPi, StartedAt, EndedAt, MessageCount, UserMessageCount | Session header format documented; `cwd` → `ExtractProjectFromCwd` for Project |
| PRSR-02 | User messages (role="user") extracted as `ParsedMessage` with text content and ordinal | `message.content` is `string` or `(TextContent|ImageContent)[]`; text blocks only |
| PRSR-03 | Assistant messages extracted with text, HasThinking, HasToolUse, ToolCalls, ContentLength | Content blocks: text, thinking, toolCall — all documented with exact field names |
| PRSR-04 | Tool calls use `toolCall` block type with `arguments` field (not Claude's `tool_use`/`input`) | Pi format: `{"type":"toolCall","id":"...","name":"...","arguments":{...}}` |
| PRSR-05 | Tool results (role="toolResult" top-level entries) parsed as ParsedMessage with ToolResults | `toolCallId` maps to `ToolUseID`; content text length sums to ContentLength |
| PRSR-06 | Thinking blocks set HasThinking=true; redacted thinking (no text, only signature) also sets it | `{"type":"thinking","thinking":"...","thinkingSignature":"...","redacted":bool}` |
| PRSR-07 | model_change events surfaced as meta messages | `{"type":"model_change","provider":"...","modelId":"..."}` → content: "Model changed to {provider}/{modelId}" |
| PRSR-08 | compaction events surfaced as synthetic user messages for FTS | `{"type":"compaction","summary":"..."}` → role=user, content=summary |
| PRSR-09 | V1 sessions (no id/parentId/version) parsed using linear file-position ordering | V1 detection: no entry has an `id` field |
| PRSR-10 | branchedFrom path stored as ParentSessionID (basename without extension) | `branchedFrom` is full absolute path; `filepath.Base` + `strings.TrimSuffix` extracts UUID |
| PRSR-11 | AgentPi constant added to internal/parser/types.go | Simple addition: `AgentPi AgentType = "pi"` |
| TAXO-01 | find tool name maps to "Read" category | Currently falls to "Other"; add `case "find": return "Read"` |
| TAXO-02 | grep (lowercase) maps to "Grep" category | Already handled: taxonomy.go line 40 `"search_files", "grep": return "Grep"` — NO-OP |
| TEST-01 | Table-driven unit tests in pi_test.go covering all parser behaviors | Fixture at testdata/pi/; use loadFixture + createTestFile pattern |
</phase_requirements>

---

## Summary

Phase 1 adds a single new parser (`internal/parser/pi.go`) that reads pi-agent JSONL session files into the existing `ParsedSession`/`ParsedMessage` model. The pi format is well-documented from prior research (direct source inspection of pi-mono TypeScript source and a real 2.3MB session fixture) and has very high structural similarity to the Claude parser — same JSONL-per-line format, same `newLineReader` infrastructure, same `gjson` parsing library, same output types.

The key differences from Claude: (1) pi uses a dedicated first-line session header (`type=session`) rather than embedding metadata in message entries; (2) message entries all use a common `type=message` wrapper with a `message.role` discriminator; (3) tool calls use `toolCall`/`arguments` instead of `tool_use`/`input`; (4) pi has explicit `compaction` and `model_change` entry types that need synthetic messages; (5) pi uses a linear `id`/`parentId` chain rather than Claude's DAG with fork detection.

The taxonomy change (TAXO-01) is a one-line addition. TAXO-02 is confirmed as a no-op — `grep` already maps to "Grep" in the existing taxonomy. The test approach mirrors the Gemini and Claude parsers exactly: a committed fixture in `testdata/pi/`, a `ParsePiSession` helper, and table-driven subtests.

**Primary recommendation:** Implement `ParsePiSession` following the Gemini parser structure (single-pass, linear, no DAG logic) using `newLineReader`, `gjson`, and `parseTimestamp` from the existing codebase. Do not reuse `ExtractTextContent` — pi uses different block type names (`toolCall` vs `tool_use`, `arguments` vs `input`).

---

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `github.com/tidwall/gjson` | already in go.mod | JSONL field extraction | Used by all existing parsers; zero-allocation path queries |
| Standard `testing` package | Go stdlib | Test framework | Project convention; no test framework added |
| `github.com/stretchr/testify` | already in go.mod | Test assertions (`assert`, `require`) | Used in claude_parser_test.go and gemini_parser_test.go |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `path/filepath` | stdlib | `filepath.Base`, `filepath.Ext` for branchedFrom extraction | Extracting session ID from file path |
| `strings` | stdlib | `strings.TrimSuffix` for removing `.jsonl` extension | V1 session ID and branchedFrom basename |
| `fmt` | stdlib | Model change message formatting | Constructing meta message content |

**Installation:** No new dependencies. All required packages are already in go.mod.

---

## Architecture Patterns

### Recommended File Structure

```
internal/parser/
├── pi.go               # ParsePiSession function — NEW
├── pi_test.go          # Table-driven tests — NEW
└── testdata/
    └── pi/
        └── session.jsonl  # Fixture covering all entry types — NEW
```

### Pattern 1: Single-Pass Linear Parser (follow Gemini, not Claude)

**What:** One file open, one pass through lines, accumulate messages, build session at end.
**When to use:** Pi has no DAG or fork detection (linear id/parentId chain). No need for two-pass like Claude.

```go
// Source: internal/parser/gemini.go structure adapted for JSONL
func ParsePiSession(path, project, machine string) (*ParsedSession, []ParsedMessage, error) {
    info, err := os.Stat(path)
    if err != nil {
        return nil, nil, fmt.Errorf("stat %s: %w", path, err)
    }
    f, err := os.Open(path)
    if err != nil {
        return nil, nil, fmt.Errorf("open %s: %w", path, err)
    }
    defer f.Close()

    lr := newLineReader(f, maxLineSize)
    // ... single pass: parse header, accumulate messages
}
```

### Pattern 2: Session Header Extraction (first line only)

**What:** Pi JSONL line 1 is always the session header with `type=session`.
**When to use:** Always — parse before entering the message loop.

```go
// Source: .planning/research/FEATURES.md — pi format spec
// Line 1 structure:
// {"type":"session","version":3,"id":"...","timestamp":"...","cwd":"...","branchedFrom":"..."}

// V2 detection: header has an "id" field
sessionID := gjson.Get(line, "id").Str  // V2: use this
if sessionID == "" {
    // V1 fallback: filename basename
    sessionID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
}
```

### Pattern 3: Message Role Dispatch

**What:** All substantive entries have `type=message`; role is inside the `message` object.
**When to use:** The main message processing loop.

```go
// Source: .planning/research/FEATURES.md — pi format spec
entryType := gjson.Get(line, "type").Str
switch entryType {
case "message":
    role := gjson.Get(line, "message.role").Str
    switch role {
    case "user":     // handle user message
    case "assistant": // handle assistant message
    case "toolResult": // handle tool result
    }
case "model_change": // synthetic meta message
case "compaction":   // synthetic user message with summary
default:             // skip silently (thinking_level_change, custom, etc.)
}
```

### Pattern 4: Pi Tool Call Extraction (NOT ExtractTextContent)

**What:** Pi assistant content blocks use `toolCall` (not `tool_use`) and `arguments` (not `input`).
**When to use:** Assistant message content block processing only.

```go
// Source: .planning/research/FEATURES.md — pi format spec
// Pi block: {"type":"toolCall","id":"toolu_...","name":"bash","arguments":{"command":"..."}}
// Claude block: {"type":"tool_use","id":"...","name":"Bash","input":{"command":"..."}}
content.ForEach(func(_, block gjson.Result) bool {
    switch block.Get("type").Str {
    case "text":
        // same as Claude
    case "thinking":
        // same as Claude — check redacted: if thinking is empty but signature present, still HasThinking=true
    case "toolCall":  // NOTE: not "tool_use"
        hasToolUse = true
        name := block.Get("name").Str
        tc := ParsedToolCall{
            ToolUseID: block.Get("id").Str,   // NOTE: "id" not "id"
            ToolName:  name,
            Category:  NormalizeToolCategory(name),
            InputJSON: block.Get("arguments").Raw, // NOTE: "arguments" not "input"
        }
        toolCalls = append(toolCalls, tc)
    }
    return true
})
```

### Pattern 5: Tool Result Entry Parsing

**What:** Pi tool results are standalone top-level entries (not nested inside user messages as in Claude).
**When to use:** When `entryType == "message"` and `role == "toolResult"`.

```go
// Source: .planning/research/FEATURES.md — pi format spec
// Pi: {"type":"message","message":{"role":"toolResult","toolCallId":"toolu_...","toolName":"bash","content":[...],"isError":false}}
toolUseID := gjson.Get(line, "message.toolCallId").Str
contentLen := 0
gjson.Get(line, "message.content").ForEach(func(_, block gjson.Result) bool {
    contentLen += len(block.Get("text").Str)
    return true
})
msg := ParsedMessage{
    Role:        RoleUser, // matches Claude pattern for tool results
    ToolResults: []ParsedToolResult{{ToolUseID: toolUseID, ContentLength: contentLen}},
    Timestamp:   ts,
}
```

### Pattern 6: Timestamp Handling (Two Sources)

**What:** Pi entries have two timestamp fields — entry-level `timestamp` (ISO 8601 string) and `message.timestamp` (Unix milliseconds integer).
**When to use:** Use entry-level `timestamp` as primary; fall back to `message.timestamp` for Unix ms.

```go
// Source: .planning/research/FEATURES.md
// Entry-level: "timestamp": "2025-12-08T22:41:05.306Z"  (ISO 8601 — handled by parseTimestamp)
// Message-level: "timestamp": 1765233665292             (Unix ms integer — NOT handled by parseTimestamp)
ts := parseTimestamp(gjson.Get(line, "timestamp").Str)
if ts.IsZero() {
    // Fall back to message.timestamp Unix ms
    msRaw := gjson.Get(line, "message.timestamp").Int()
    if msRaw > 0 {
        ts = time.UnixMilli(msRaw).UTC()
    }
}
```

### Pattern 7: branchedFrom → ParentSessionID

**What:** Pi `branchedFrom` is a full absolute path like `/Users/user/.pi/agent/sessions/--path--/2025-12-09T00-52-54-397Z_uuid.jsonl`. Extract the UUID (filename basename without extension).

```go
// Source: .planning/research/FEATURES.md
branchedFrom := gjson.Get(headerLine, "branchedFrom").Str
if branchedFrom != "" {
    base := filepath.Base(branchedFrom)
    parentSessionID = strings.TrimSuffix(base, ".jsonl")
}
// Also check header's parentSession field as secondary source
```

### Pattern 8: Test Fixture + loadFixture (existing helper)

**What:** Committed JSONL fixture file at `testdata/pi/session.jsonl`, loaded via the existing `loadFixture` helper in `claude_parser_test.go`.
**When to use:** All pi_test.go tests that need comprehensive coverage.

```go
// Source: internal/parser/claude_parser_test.go lines 201-207
// loadFixture reads testdata/{name} relative to the test package directory.
// Available in pi_test.go because same package.

func TestParsePiSession_Full(t *testing.T) {
    content := loadFixture(t, "pi/session.jsonl")
    path := createTestFile(t, "pi-session.jsonl", content)
    sess, msgs, err := ParsePiSession(path, "my_project", "local")
    require.NoError(t, err)
    // ...
}
```

### Anti-Patterns to Avoid

- **Reusing `ExtractTextContent` for pi:** Pi uses different block type names (`toolCall` not `tool_use`, `arguments` not `input`). Calling `ExtractTextContent` would silently miss all tool calls. Implement a dedicated `extractPiAssistantContent` helper.
- **Using `gjson.Get(line, "message.content")` as a string for text messages:** Pi user message content is an array `[{"type":"text","text":"..."}]` not a plain string. Handle both string and array (same as Claude handles it in `ExtractTextContent`).
- **Attempting to parse Unix ms with `parseTimestamp`:** The existing `parseTimestamp` only handles ISO 8601 strings. Use `time.UnixMilli(n).UTC()` for the `message.timestamp` fallback.
- **V1 detection by checking the header version field only:** Per locked decision, V1 detection is based on whether ANY entry has an `id` field — not just the header's `version` field. Check both.
- **Storing `branchedFrom` as raw absolute path:** Only store the basename (without extension) as `ParentSessionID` — consistent with how session IDs work throughout the codebase.

---

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Buffered line reading for large JSONL files | Custom scanner | `newLineReader(f, maxLineSize)` (linereader.go) | Already handles oversized line skipping, 64KB initial buffer, 64MB max |
| JSON field extraction | `encoding/json` + structs | `github.com/tidwall/gjson` | Zero-alloc path queries; all existing parsers use it; no struct definitions needed |
| Timestamp ISO 8601 parsing | `time.Parse` with custom layouts | `parseTimestamp(ts)` (timestamp.go) | Already handles RFC3339, RFC3339Nano, offset normalization, and truncated log output |
| Test file creation | `os.WriteFile` directly | `createTestFile(t, name, content)` (parser_test.go) | Helper registers cleanup and uses `t.TempDir()` |
| Fixture loading | Manual path construction | `loadFixture(t, "pi/session.jsonl")` (claude_parser_test.go) | Already in-package; reads from `testdata/` relative to package |
| Tool name normalization | Custom switch | `NormalizeToolCategory(name)` (taxonomy.go) | Pi lowercase tool names already handled for most; only `find` is missing |
| Project name extraction from cwd | Custom string manipulation | `ExtractProjectFromCwd(cwd)` (project.go) | Handles common project folder conventions |

**Key insight:** The entire infrastructure (line reader, timestamp parser, tool taxonomy, project extraction, test helpers, gjson) is already present. The pi parser is ~80% reuse of patterns, not new code.

---

## Common Pitfalls

### Pitfall 1: Calling ExtractTextContent for Pi Tool Calls
**What goes wrong:** Tool calls are silently dropped from all assistant messages because `ExtractTextContent` looks for `type=tool_use` blocks, not `type=toolCall`.
**Why it happens:** The block type name is different between Claude (`tool_use`) and pi (`toolCall`).
**How to avoid:** Write a dedicated `extractPiContent` function that handles the pi block type names.
**Warning signs:** `HasToolUse=false` on all assistant messages; `ToolCalls` slice always empty.

### Pitfall 2: Missing Redacted Thinking → HasThinking
**What goes wrong:** `HasThinking=false` on messages where the model was thinking but content was redacted.
**Why it happens:** Redacted thinking blocks have an empty `thinking` field — checking `thinking != ""` misses them.
**How to avoid:** Set `HasThinking=true` when block type is `thinking`, regardless of whether `thinking` field is empty.
**Warning signs:** Sessions with `thinkingSignature` present but `HasThinking=false`.

### Pitfall 3: User Message Content as Plain String
**What goes wrong:** User message text is empty because the code checks for a string but pi user messages use a content array.
**Why it happens:** Pi user `message.content` is `[{"type":"text","text":"..."}]`, not a plain string.
**How to avoid:** Handle both string and array content for user messages (same pattern as `ExtractTextContent` does for its string/array check).
**Warning signs:** `FirstMessage` is empty; user message `Content` is always empty.

### Pitfall 4: Unix ms Timestamp Ignored
**What goes wrong:** Message-level timestamps are all zero, causing sessions to have zero `StartedAt`/`EndedAt` if the entry-level timestamp is also absent.
**Why it happens:** `parseTimestamp` handles only ISO 8601 strings; `message.timestamp` is an integer (Unix ms).
**How to avoid:** After `parseTimestamp` returns zero, check `message.timestamp` as integer and use `time.UnixMilli(n).UTC()`.
**Warning signs:** All message timestamps are zero; session StartedAt/EndedAt are zero.

### Pitfall 5: TAXO-02 "grep" is Already Handled
**What goes wrong:** Adding a duplicate case for `grep` causes a compile error (`duplicate case "grep" in expression switch`).
**Why it happens:** `grep` is already in the taxonomy switch at line 40 of taxonomy.go under the Gemini section (`case "search_files", "grep": return "Grep"`).
**How to avoid:** Only add `case "find": return "Read"` — do NOT add `grep`. Verify first.
**Warning signs:** Compilation fails with duplicate case error.

### Pitfall 6: V1 Detection Logic
**What goes wrong:** V1 sessions are incorrectly treated as V2 or vice versa, producing wrong session IDs.
**Why it happens:** Checking only the header's `version` field misses the case where V2 session files start without a version.
**How to avoid:** Per locked decision — if ANY line in the file has an `id` field, treat as V2. For pure V1 files (no `id` anywhere), use filename basename as session ID.
**Warning signs:** V1 session has a blank session ID; or V1 session ID contains a UUID from the filename.

---

## Code Examples

### Pi Session Header Parsing
```go
// Source: .planning/research/FEATURES.md — direct inspection of pi-mono session-manager.ts
// Header is always line 1; subsequent lines are entries
// {"type":"session","version":3,"id":"ffae836b-...","timestamp":"...","cwd":"...","branchedFrom":"..."}

// After reading first valid line:
if gjson.Get(line, "type").Str != "session" {
    // Not a pi session file; return error or empty result
    return nil, nil, fmt.Errorf("not a pi session: missing session header in %s", path)
}
sessionID := gjson.Get(line, "id").Str
cwd := gjson.Get(line, "cwd").Str
headerTS := parseTimestamp(gjson.Get(line, "timestamp").Str)

branchedFrom := gjson.Get(line, "branchedFrom").Str
var parentSessionID string
if branchedFrom != "" {
    base := filepath.Base(branchedFrom)
    parentSessionID = strings.TrimSuffix(base, filepath.Ext(base))
}
```

### Compaction → Synthetic User Message
```go
// Source: .planning/phases/01-parser-foundation/01-CONTEXT.md (locked decision)
// + .planning/research/FEATURES.md
case "compaction":
    summary := gjson.Get(line, "summary").Str
    if summary == "" {
        summary = "[session compacted]" // fallback wording at Claude's discretion
    }
    ts := parseTimestamp(gjson.Get(line, "timestamp").Str)
    messages = append(messages, ParsedMessage{
        Ordinal:       ordinal,
        Role:          RoleUser,
        Content:       summary,
        Timestamp:     ts,
        ContentLength: len(summary),
    })
    ordinal++
```

### model_change → Meta Message
```go
// Source: .planning/phases/01-parser-foundation/01-CONTEXT.md (locked decision)
case "model_change":
    provider := gjson.Get(line, "provider").Str
    modelID := gjson.Get(line, "modelId").Str
    content := fmt.Sprintf("Model changed to %s/%s", provider, modelID)
    ts := parseTimestamp(gjson.Get(line, "timestamp").Str)
    messages = append(messages, ParsedMessage{
        Ordinal:       ordinal,
        Role:          RoleUser,
        Content:       content,
        Timestamp:     ts,
        ContentLength: len(content),
    })
    ordinal++
```

### AgentPi Addition to types.go
```go
// Source: internal/parser/types.go — follow existing pattern
const (
    AgentClaude   AgentType = "claude"
    AgentCodex    AgentType = "codex"
    AgentCopilot  AgentType = "copilot"
    AgentGemini   AgentType = "gemini"
    AgentOpenCode AgentType = "opencode"
    AgentCursor   AgentType = "cursor"
    AgentPi       AgentType = "pi"  // ADD THIS
)
```

### TAXO-01 Addition to taxonomy.go
```go
// Source: internal/parser/taxonomy.go — add to Pi-specific section
// Place after the existing OpenCode/Copilot/Cursor tool sections
// "find" is a filesystem search tool; maps to "Read" category
case "find":
    return "Read"
```

### Fixture File Structure (testdata/pi/session.jsonl)

The fixture should be a valid synthesized JSONL that exercises all parser behaviors in one file:

```jsonl
{"type":"session","version":3,"id":"pi-session-uuid","timestamp":"2025-01-01T10:00:00Z","cwd":"/Users/alice/code/my-project","branchedFrom":"/Users/alice/.pi/agent/sessions/--path--/2025-01-01T09-00-00-000Z_parent-uuid.jsonl"}
{"type":"message","id":"entry-1","parentId":null,"timestamp":"2025-01-01T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"Fix the login bug"}],"timestamp":1735725601000}}
{"type":"message","id":"entry-2","parentId":"entry-1","timestamp":"2025-01-01T10:00:02Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"Let me analyze this.","thinkingSignature":"sig","redacted":false},{"type":"text","text":"Looking at the auth module."},{"type":"toolCall","id":"toolu_01","name":"read","arguments":{"file_path":"auth.go"}}],"model":"claude-opus-4-5","provider":"anthropic","usage":{"input":100,"output":50,"totalTokens":150,"cost":{"total":0.01}},"stopReason":"toolUse","timestamp":1735725602000}}
{"type":"message","id":"entry-3","parentId":"entry-2","timestamp":"2025-01-01T10:00:03Z","message":{"role":"toolResult","toolCallId":"toolu_01","toolName":"read","content":[{"type":"text","text":"package auth\nfunc Login() {}"}],"isError":false,"timestamp":1735725603000}}
{"type":"model_change","id":"entry-4","parentId":"entry-3","timestamp":"2025-01-01T10:00:04Z","provider":"anthropic","modelId":"claude-opus-4-5"}
{"type":"compaction","id":"entry-5","parentId":"entry-4","timestamp":"2025-01-01T10:00:05Z","summary":"# Context Checkpoint\nThe user is fixing a login bug.","firstKeptEntryIndex":0,"tokensBefore":5000}
{"type":"message","id":"entry-6","parentId":"entry-5","timestamp":"2025-01-01T10:00:06Z","message":{"role":"user","content":[{"type":"text","text":"Look good to you?"}],"timestamp":1735725606000}}
{"type":"message","id":"entry-7","parentId":"entry-6","timestamp":"2025-01-01T10:00:07Z","message":{"role":"assistant","content":[{"type":"text","text":"Looks good!"}],"model":"claude-opus-4-5","provider":"anthropic","usage":{"input":200,"output":10,"totalTokens":210,"cost":{"total":0.005}},"stopReason":"stop","timestamp":1735725607000}}
{"type":"thinking_level_change","id":"entry-8","parentId":"entry-7","timestamp":"2025-01-01T10:00:08Z","thinkingLevel":"high"}
not valid json -- malformed line that should be skipped
{"type":"unknown_future_entry_type","id":"entry-9","parentId":"entry-8","timestamp":"2025-01-01T10:00:09Z"}
```

---

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Only Claude/Codex parsers | Multi-agent parsers (Gemini, OpenCode, Copilot, Cursor added) | 2024-2025 | Pi follows same addition pattern |
| Claude-specific DAG processing | Agent-specific linear vs DAG split | At Gemini addition | Pi is linear — use Gemini pattern |

**Confirmed working in existing code:**
- `newLineReader` handles oversized lines, 64MB max
- `parseTimestamp` handles ISO 8601 with offset normalization
- `NormalizeToolCategory("grep")` → `"Grep"` (TAXO-02 is a confirmed no-op)
- `NormalizeToolCategory("find")` → `"Other"` currently (TAXO-01 needs the addition)
- `loadFixture` helper reads from `testdata/` relative to parser package
- `createTestFile` helper creates temp files and registers cleanup

---

## Open Questions

1. **Empty user message content string (plain string form)**
   - What we know: Pi user `message.content` should be an array of content blocks per the spec
   - What's unclear: Whether any real pi sessions use a plain string for user content (Claude supports both)
   - Recommendation: Handle both string and array (defensive; cost is negligible)

2. **model_change content format**
   - What we know: Locked decision says `"Model changed to {model_name}"` but the entry has both `provider` and `modelId` fields
   - What's unclear: Which fields to use — `modelId` alone, or `provider/modelId`
   - Recommendation: Use `provider/modelId` format (e.g., `"Model changed to anthropic/claude-opus-4-5"`) for maximum context; Claude's discretion on exact wording

3. **tool_use_id field name in fixture vs spec**
   - What we know: Real fixture uses `toolCallId` for tool result entries (confirmed from FEATURES.md)
   - What's unclear: Whether any old pi sessions use `toolUseId` (camelCase variant)
   - Recommendation: Use `toolCallId` exclusively; skip result if empty (consistent with Claude pattern)

---

## Sources

### Primary (HIGH confidence)
- `.planning/research/FEATURES.md` — Direct inspection of pi-mono TypeScript source (session-manager.ts, types.ts, messages.ts) and a 2.3MB real session fixture; complete entry type reference with exact field names
- `internal/parser/claude.go` — Claude parser implementation patterns (newLineReader, parseTimestamp reuse)
- `internal/parser/gemini.go` — Gemini parser structure (single-pass, return `*ParsedSession` + `[]ParsedMessage`)
- `internal/parser/content.go` — `ExtractTextContent` reference for what NOT to reuse (different block type names)
- `internal/parser/taxonomy.go` — Confirmed: `grep` already maps to "Grep"; `find` maps to "Other" (needs fix)
- `internal/parser/types.go` — `AgentType` constant pattern; `ParsedSession`, `ParsedMessage` fields
- `internal/parser/timestamp.go` — `parseTimestamp` handles ISO 8601 only; no Unix ms support
- `internal/parser/linereader.go` — `newLineReader` available; `initialScanBufSize` and `maxLineSize` constants in `claude.go`
- `internal/parser/claude_parser_test.go` — `loadFixture` helper (line 201); `createTestFile` (parser_test.go line 622)
- `internal/parser/test_helpers_test.go` — Full set of available test assertions

### Secondary (MEDIUM confidence)
- `.planning/phases/01-parser-foundation/01-CONTEXT.md` — Locked decisions from user discussion
- `.planning/codebase/TESTING.md` — Project testing conventions

---

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH — all libraries already in the codebase; no new dependencies
- Architecture: HIGH — pi format fully documented from source inspection; existing patterns clearly established
- Pitfalls: HIGH — confirmed from code reading (taxonomy grep, ExtractTextContent block types, timestamp integer handling)

**Research date:** 2026-02-27
**Valid until:** 2026-03-29 (pi format is stable; existing parser patterns are stable)
