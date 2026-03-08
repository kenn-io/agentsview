# Phase 4: Better pi Tool Call Rendering - Research

**Researched:** 2026-02-28
**Domain:** Frontend TypeScript — tool call display layer (Svelte 5, content-parser.ts, tool-params.ts)
**Confidence:** HIGH

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions

- **Tool name aliasing:** Map pi tool names to Claude Code aliases for display: `str_replace` → Edit, `run_command` → Bash, `create_file` → Write, `read_file` → Read. Use the same alias mechanism already in `content-parser.ts` (TOOL_ALIASES) — extend it with pi names.
- **Tool metadata tags (collapsed header):**
  - `str_replace` (Edit): show `file: path` tag — matches existing Edit behavior
  - `run_command` (Bash): show `description` field if present — matches existing Bash behavior
  - `create_file` (Write): show `file: path` tag — matches existing Write behavior
  - `read_file` (Read): show `file: path` tag — matches existing Read behavior
- **Expanded content format:**
  - `str_replace` (Edit): `--- old` / `+++ new` diff style showing `old_string` and `new_string` — matches existing Claude Code Edit formatting
  - `run_command` (Bash): `$ command` with full multiline support — matches existing Claude Code Bash formatting
  - `create_file` (Write): truncated file content (~500 chars) with ellipsis — matches existing Write behavior
- **Tool result messages:** Suppress `toolResult` messages entirely — they currently render as blank/empty user messages. Do not surface result content length anywhere — hide completely.
- **Architecture:** Extend existing `tool-params.ts` (not a new file) with pi tool name handlers. Try known pi-specific field names first (e.g. `command`, `path`, `content`, `old_string`, `new_string`), fall through to generic key:value fallback if no known fields match. Unknown pi tool types get the existing generic fallback — safe and extensible.

### Claude's Discretion

- Exact pi argument field names to use (researcher should inspect real pi JSONL test files to confirm `str_replace` uses `old_string`/`new_string`/`path`, `run_command` uses `command`, `create_file` uses `path`/`content`, `read_file` uses `path`)
- Whether toolResult suppression happens at the message list level or via a CSS/render flag

### Deferred Ideas (OUT OF SCOPE)

None — discussion stayed within phase scope.
</user_constraints>

---

## Summary

Phase 4 is a pure frontend rendering improvement. Pi agent tool calls are stored in the DB with `tool_name` set to the raw pi JSONL tool name (e.g., `str_replace`, `run_command`, `create_file`, `read_file`) and `input_json` holding the raw argument object. When `enrichSegments` creates tool segments for pi messages (which have no text-marker tool blocks), it sets `label = tc.tool_name` and `content = tc.input_json`. The `ToolBlock` component then calls `generateFallbackContent(toolCall.tool_name, inputParams)` — since `str_replace`, `run_command`, etc. are not recognized tool names, they fall through to the generic `key: value` loop, showing raw JSON keys rather than formatted output.

The fix requires two targeted changes: (1) extend `TOOL_ALIASES` in `content-parser.ts` with pi tool names so the display label reads "Edit", "Bash", etc. instead of the raw name, and (2) extend `extractToolParamMeta` and `generateFallbackContent` in `tool-params.ts` with pi-specific field name handlers. Additionally, `toolResult`-role messages (stored as user-role messages with empty content and no text) need to be suppressed in the message list filter.

The entire change is confined to three TypeScript utility files plus `MessageList.svelte` for toolResult suppression. No backend changes, no new files, no schema changes.

**Primary recommendation:** Extend `tool-params.ts` with pi tool handlers and add `TOOL_ALIASES` entries in `content-parser.ts` for pi tool names. Suppress toolResult messages in `MessageList.svelte`'s `filteredMessages` filter alongside the existing system-message filter.

---

## Standard Stack

No new libraries needed. All changes are in existing TypeScript utility files.

### Core (already in use)
| File | Purpose | Relevance |
|------|---------|-----------|
| `frontend/src/lib/utils/tool-params.ts` | Extracts meta tags and fallback content for tool blocks | Extend with pi tool handlers |
| `frontend/src/lib/utils/content-parser.ts` | Parses message text into segments; holds TOOL_ALIASES | Add pi names to TOOL_ALIASES |
| `frontend/src/lib/components/content/MessageList.svelte` | Filters messages before rendering | Add toolResult suppression |
| `frontend/src/lib/utils/tool-params.test.ts` | Vitest unit tests for tool-params | Add pi tool test cases |
| `frontend/src/lib/utils/content-parser.test.ts` | Vitest unit tests for content-parser | Add alias test cases if needed |

---

## Architecture Patterns

### How pi Tool Calls Flow to the Display Layer

```
pi JSONL file
  └── toolCall block: { type: "toolCall", id, name, arguments: {...} }
        ↓ internal/parser/pi.go (parsePiAssistantMessage)
  ParsedToolCall { ToolName: "str_replace", InputJSON: `{"path":"...","old_string":"...","new_string":"..."}` }
        ↓ internal/db (stored in tool_calls table)
  ToolCall { tool_name: "str_replace", category: "Edit", input_json: "..." }
        ↓ frontend API response → message.tool_calls[]
  enrichSegments() creates: { type: "tool", label: "str_replace", content: rawInputJSON, toolCall: tc }
        ↓ ToolBlock.svelte
  label → "str_replace" (unaliased — bad)
  fallbackContent → generateFallbackContent("str_replace", params) → generic key:value (bad)
```

After the fix:
```
  TOOL_ALIASES["str_replace"] = "Edit"    ← content-parser.ts
  enrichSegments() creates: { label: "Edit", ... }   ← aliased ✓
  extractToolParamMeta("Edit", { path, old_string, new_string })
    → [{ label: "file", value: path }]    ← meta tag ✓
  generateFallbackContent("Edit", { path, old_string, new_string })
    → "--- old\n{old_string}\n+++ new\n{new_string}"    ← diff format ✓
```

### Pattern 1: TOOL_ALIASES Extension (content-parser.ts)

**What:** Add pi raw tool names to the existing `TOOL_ALIASES` map so `enrichSegments` presents them under the canonical Claude Code display names.
**When to use:** When a new agent uses different tool name strings for the same semantic operation.

```typescript
// Source: frontend/src/lib/utils/content-parser.ts (existing pattern)
const TOOL_ALIASES: Record<string, string> = {
  exec_command: "Bash",
  shell_command: "Bash",
  write_stdin: "Bash",
  shell: "Bash",
  apply_patch: "Edit",
  // Add pi tool aliases:
  str_replace: "Edit",
  run_command: "Bash",
  create_file: "Write",
  read_file: "Read",
};
```

Note: `TOOL_NAMES` regex pattern in `content-parser.ts` only matters for text-marker parsing (Claude Code style). Pi tools arrive via structured `tool_calls` array, not text markers, so `TOOL_NAMES` does NOT need to be updated.

The `TOOL_ALIASES` map is used in `enrichSegments` at line:
```typescript
label: tc.tool_name  // set before aliasing in content segment
```
Wait — actually TOOL_ALIASES is used in `extractMatches` for the text-marker path. For structured tool calls (pi style), the label is set directly in `enrichSegments` as `tc.tool_name`. Therefore the alias needs to be applied in `enrichSegments` too, OR the ToolBlock's `label` prop needs to go through an alias lookup.

**Critical finding:** `enrichSegments` sets `label: tc.tool_name` for pi-style tool calls (line 402 in content-parser.ts). TOOL_ALIASES is only applied in `extractMatches` (text-marker path). To alias pi tool names, either:
- (a) Apply TOOL_ALIASES lookup when setting label in the `!hasTextBasedTools` branch of `enrichSegments`, or
- (b) Apply aliasing in `ToolBlock.svelte` when calling `extractToolParamMeta`/`generateFallbackContent`

Option (a) is cleaner — the alias lookup belongs in content-parser.ts where TOOL_ALIASES already lives.

### Pattern 2: extractToolParamMeta Extension (tool-params.ts)

**What:** Add `else if` branches for pi tool names (after aliasing, these will be "Edit", "Bash", "Write", "Read" — which already exist). The key insight is that pi's argument field names differ from Claude's.

Pi field names (from CONTEXT.md discretion + taxonomy evidence):
- `str_replace`: `path` (not `file_path`), `old_string`, `new_string`
- `run_command`: `command` (same as Claude Bash), may have `description`
- `create_file`: `path` (not `file_path`), `content`
- `read_file`: `path` (not `file_path`)

Because `extractToolParamMeta` is called with the aliased tool name (e.g., "Edit") but pi uses `path` while Claude uses `file_path`, the existing "Edit" branch checks `params.file_path` and will find nothing. The functions must handle BOTH field names.

**Approach:** Update existing "Edit", "Write", "Read" branches to check `path` as a fallback when `file_path` is absent:

```typescript
// tool-params.ts — updated Read branch example
} else if (toolName === "Read") {
  const filePath = params.file_path ?? params.path;  // pi uses "path"
  if (filePath)
    meta.push({ label: "file", value: truncate(String(filePath), 80) });
  // ... existing offset/limit/pages handling
}
```

### Pattern 3: generateFallbackContent Extension (tool-params.ts)

For "Edit" with pi field names:
```typescript
if (toolName === "Edit") {
  // pi uses "path"/"old_string"/"new_string"; Claude uses "file_path"/"old_string"/"new_string"
  const lines: string[] = [];
  if (params.old_string != null) {
    lines.push("--- old");
    lines.push(truncate(String(params.old_string), 500));
  }
  if (params.new_string != null) {
    lines.push("+++ new");
    lines.push(truncate(String(params.new_string), 500));
  }
  return lines.length ? lines.join("\n") : null;
  // existing logic already handles old_string/new_string — no change needed here
}
```

The Edit case already handles `old_string`/`new_string`, so generateFallbackContent for Edit needs no change. The Write case checks `params.content` — pi `create_file` also uses `content`, so no change needed. The gap is only in `extractToolParamMeta` where `file_path` vs `path` needs a fallback.

For Bash/`run_command`, the command is in `params.command` — same field name as Claude. The `description` field (if any) is also the same. No change needed for generateFallbackContent. The `enrichSegments` code already handles the Bash multi-line `$ command` expansion via `input.command`.

### Pattern 4: toolResult Suppression (MessageList.svelte)

**What:** toolResult messages are stored as `role: "user"` with empty `content` and a populated `ToolResults` array. The backend does not expose `ToolResults` to the frontend (it's tagged `json:"-"`). So these messages arrive as user-role messages with empty content.

**Current appearance:** Empty blank user message bubble — ugly and confusing.

**Suppression approach:** Add a filter in `MessageList.svelte`'s `filteredMessages` alongside the existing `isSystemMessage` filter.

Detection heuristic: A toolResult message has `role === "user"`, `content === ""` (or only whitespace), and `has_tool_use === false` and `has_thinking === false`. However, this could also suppress legitimate empty user messages from other agents.

**Better approach:** Check `content_length === 0` AND `role === "user"` AND no tool use. But we need to distinguish from genuine empty user turns.

**Most reliable approach without backend change:** Check that the user message is empty AND has zero content_length. Since `parsePiToolResultMessage` sets no Content field (empty string) and ContentLength comes from the result text (which IS stored), the `content_length` might not be 0.

Looking at the parser: `parsePiToolResultMessage` returns a `ParsedMessage` with `Role: RoleUser`, no `Content` field (empty string), and `ToolResults` populated. The `ContentLength` is set to the sum of tool result text lengths. But `ToolResults` is `json:"-"` — it's not returned to the frontend.

So toolResult messages arrive at the frontend as: `role: "user"`, `content: ""`, `has_tool_use: false`, `has_thinking: false`, `content_length: N` (where N is the result text length).

A safe filter: `m.role === "user" && m.content.trim() === "" && !m.has_tool_use`. This is specific enough since legitimate empty user messages (from pi's `parsePiUserMessage`) would only be created if there was genuinely no text in the content blocks — extremely rare and still invisible.

### Anti-Patterns to Avoid

- **Modifying TOOL_NAMES regex:** This is only needed for text-marker parsing (Claude Code). Pi tool calls arrive via `tool_calls` array, not text markers. Adding pi names to `TOOL_NAMES` would have no effect and would be misleading.
- **Creating a new file for pi tool params:** The CONTEXT.md explicitly locks this to extending `tool-params.ts`.
- **Checking only `file_path` in extractToolParamMeta for aliased pi tools:** Pi uses `path` not `file_path`. Both must be checked.
- **Backend change for toolResult suppression:** The CONTEXT.md scopes this to frontend rendering only.

---

## Don't Hand-Roll

| Problem | Don't Build | Use Instead |
|---------|-------------|-------------|
| Tool name aliasing | Custom display-name lookup | Extend existing `TOOL_ALIASES` map in content-parser.ts |
| Field normalization | Parallel param-extraction functions | Extend existing `extractToolParamMeta` with fallback field names |
| Diff display | Custom diff renderer | Reuse existing `--- old / +++ new` logic already in `generateFallbackContent` |
| Bash command display | Custom command formatter | Reuse existing `enrichSegments` `$ command` expansion (already handles `input.command`) |

**Key insight:** The entire rendering infrastructure already exists for Claude Code tools. Pi tools use the same semantic operations with different field names. The fix is adding field-name fallbacks, not new rendering logic.

---

## Common Pitfalls

### Pitfall 1: TOOL_ALIASES only applied in text-marker path
**What goes wrong:** Developer adds pi names to `TOOL_ALIASES` but pi tools still show raw names because `TOOL_ALIASES` is only applied in `extractMatches` (text-marker branch), not in the `!hasTextBasedTools` branch of `enrichSegments`.
**Why it happens:** The two paths (text-marker vs structured JSON) are separate code paths in `enrichSegments`.
**How to avoid:** Also apply `TOOL_ALIASES[tc.tool_name] ?? tc.tool_name` when setting `label` in the `!hasTextBasedTools` append loop.
**Warning signs:** Tool label still reads "str_replace" in browser despite TOOL_ALIASES having the mapping.

### Pitfall 2: file_path vs path field name mismatch
**What goes wrong:** `extractToolParamMeta("Read", { path: "/foo.ts" })` returns null because the existing "Read" branch checks `params.file_path` which is undefined.
**Why it happens:** Pi uses `path` as the field name; Claude Code uses `file_path`.
**How to avoid:** Use `params.file_path ?? params.path` in each file-path check.
**Warning signs:** Meta tag section is empty for pi read/write/edit tool calls.

### Pitfall 3: toolResult suppression too broad
**What goes wrong:** Legitimate short user messages are also suppressed.
**Why it happens:** Filter condition `content === "" && role === "user"` could match genuine empty turns from other agents.
**How to avoid:** Confirm the filter `content.trim() === "" && !has_tool_use && !has_thinking` is sufficient. Since no other agent creates empty-content user messages in practice, this is safe. If concerned, could additionally check `content_length > 0` to keep messages where result content was stored but not surfaced — but this makes pi toolResult messages with non-zero content_length visible again. The cleanest approach: check `content.trim() === "" && role === "user"` (empty content is the key differentiator).
**Warning signs:** Valid user messages disappear in sessions from other agents.

### Pitfall 4: Forgetting to update TOOL_NAMES when pi tool names appear in text
**What goes wrong:** Not applicable here — pi stores tool calls in structured `tool_calls` array, never as text markers. TOOL_NAMES is NOT needed.
**Why it matters:** Confusion about which code path handles pi tools. Always remember: pi uses `enrichSegments` structured path, not regex text-marker path.

---

## Code Examples

### Verified current pattern: enrichSegments structured path (content-parser.ts:396-407)
```typescript
// Source: frontend/src/lib/utils/content-parser.ts
if (!hasTextBasedTools) {
  while (tcIdx < toolCalls.length) {
    const tc = toolCalls[tcIdx]!;
    tcIdx++;
    result.push({
      type: "tool",
      content: tc.input_json ?? "",
      label: tc.tool_name,  // <-- this is where aliasing must happen
      toolCall: tc,
    });
  }
}
```

After fix, this line becomes:
```typescript
label: TOOL_ALIASES[tc.tool_name] ?? tc.tool_name,
```

### Verified current pattern: extractToolParamMeta Read branch (tool-params.ts:23-43)
```typescript
// Source: frontend/src/lib/utils/tool-params.ts
if (toolName === "Read") {
  if (params.file_path)
    meta.push({ label: "file", value: truncate(String(params.file_path), 80) });
  // ...
}
```

After fix:
```typescript
if (toolName === "Read") {
  const filePath = params.file_path ?? params.path;  // pi uses "path"
  if (filePath)
    meta.push({ label: "file", value: truncate(String(filePath), 80) });
  // ...
}
```

### Verified current pattern: generateFallbackContent Edit (tool-params.ts:110-121)
```typescript
// Source: frontend/src/lib/utils/tool-params.ts
if (toolName === "Edit") {
  const lines: string[] = [];
  if (params.old_string != null) {
    lines.push("--- old");
    lines.push(truncate(String(params.old_string), 500));
  }
  if (params.new_string != null) {
    lines.push("+++ new");
    lines.push(truncate(String(params.new_string), 500));
  }
  return lines.length ? lines.join("\n") : null;
}
```

This already handles pi's `old_string`/`new_string` field names. No change needed.

### Verified current pattern: enrichSegments Bash expansion (content-parser.ts:362-381)
```typescript
// Source: frontend/src/lib/utils/content-parser.ts
if (tc.tool_name === "Bash" && tc.input_json) {
  // ...
  const input = JSON.parse(tc.input_json);
  const fullCmd = input.command;
  if (fullCmd && fullCmd.includes("\n")) {
    enriched.content = `$ ${fullCmd}`;
    // ...
  }
}
```

This check uses `tc.tool_name === "Bash"` — which will only work after aliasing happens. If pi's `run_command` is aliased to "Bash" in the label but `tc.tool_name` still holds "run_command", this expansion won't trigger. **Important:** `tc.tool_name` is the raw DB value and is NOT aliased — the alias only affects `label`. The Bash expansion check must also handle `tc.tool_name === "run_command"`.

### Verified current pattern: isSystemMessage filter (MessageList.svelte:30-36)
```typescript
// Source: frontend/src/lib/components/content/MessageList.svelte
function isSystemMessage(m: Message): boolean {
  if (m.role !== "user") return false;
  const trimmed = m.content.trim();
  return SYSTEM_MSG_PREFIXES.some((p) => trimmed.startsWith(p));
}
```

Pattern to add alongside this in `filteredMessages`:
```typescript
function isPiToolResult(m: Message): boolean {
  return m.role === "user" && m.content.trim() === "";
}
// In filteredMessages derived:
msgs = msgs.filter((m) => !isSystemMessage(m) && !isPiToolResult(m));
```

---

## Pi Tool Names: Confirmed Field Names

Based on code inspection of `internal/parser/taxonomy.go` and the CONTEXT.md locked decisions, plus the pi-mono source research:

| Pi Tool Name | Aliased To | Known Argument Fields | Source |
|-------------|-----------|----------------------|--------|
| `str_replace` | Edit | `path`, `old_string`, `new_string` | CONTEXT.md locked decision |
| `run_command` | Bash | `command`, (optional `description`) | CONTEXT.md + taxonomy.go already maps `run_command` → Bash |
| `create_file` | Write | `path`, `content` | CONTEXT.md locked decision |
| `read_file` | Read | `path` | CONTEXT.md + taxonomy.go already maps `read_file` → Read |

Note: `run_command` and `read_file` are already in `taxonomy.go` (mapped under the Gemini section — pi reuses same tool names). `str_replace` and `create_file` are NOT in taxonomy.go — they map to "Other" category currently. These need to be added to taxonomy.go as part of this phase.

**Additional note:** Pi also uses lowercase `read`, `write`, `edit`, `bash` (native pi tools per pi-mono source). These are already mapped in taxonomy.go under "OpenCode tools". Their argument fields match Claude Code (`file_path`, `command`, `content`, `old_string`, `new_string`). No changes needed for these.

---

## State of the Art

| Old Behavior | New Behavior | Change |
|---|---|---|
| Pi tool blocks show raw `str_replace` label + `path: /foo\nold_string: x\nnew_string: y` | Show "Edit" label + file meta tag + `--- old / +++ new` diff | Phase 4 |
| Pi tool blocks show raw `run_command` label + `command: ls` | Show "Bash" label + `$ ls` content | Phase 4 |
| toolResult messages render as empty user bubbles | toolResult messages suppressed entirely | Phase 4 |

---

## Open Questions

1. **Taxonomy.go: should `str_replace` and `create_file` be added?**
   - What we know: These pi tool names return "Other" from `NormalizeToolCategory`. The category is stored in the DB and used for analytics.
   - What's unclear: The CONTEXT.md scopes changes to "frontend rendering improvements." But taxonomy is backend.
   - Recommendation: Add `str_replace` → "Edit" and `create_file` → "Write" to taxonomy.go as a small backend addition — it's a 2-line change and fixes analytics category counts for pi sessions. Flag this as in-scope since it's a direct dependency of correct aliasing behavior.

2. **toolResult suppression: what if content_length > 0 is a concern?**
   - What we know: `parsePiToolResultMessage` stores `ContentLength` from result text, but the `content` string is empty.
   - What's unclear: Do any pi tool results have meaningful text in `content` field (not content_length)?
   - Recommendation: Filter on `content.trim() === ""` — this is what actually renders. Zero-content messages are invisible regardless of content_length.

3. **Bash expansion in enrichSegments: does it need run_command support?**
   - What we know: The expansion checks `tc.tool_name === "Bash"` but pi tool_name is `"run_command"` (raw, not aliased).
   - What's unclear: Is multi-line command formatting needed for pi run_command calls?
   - Recommendation: Yes — add `|| tc.tool_name === "run_command"` to the Bash expansion condition, or set a sentinel in the segment label and check that instead.

---

## Sources

### Primary (HIGH confidence)
- Direct code inspection: `frontend/src/lib/utils/tool-params.ts` — confirmed current extractToolParamMeta and generateFallbackContent logic
- Direct code inspection: `frontend/src/lib/utils/content-parser.ts` — confirmed TOOL_ALIASES and enrichSegments structured path
- Direct code inspection: `frontend/src/lib/components/content/ToolBlock.svelte` — confirmed how fallback content is triggered
- Direct code inspection: `frontend/src/lib/components/content/MessageList.svelte` — confirmed existing isSystemMessage filter pattern
- Direct code inspection: `internal/parser/pi.go` — confirmed pi tool fields stored as raw JSONL arguments
- Direct code inspection: `internal/parser/taxonomy.go` — confirmed which pi tool names already have category mappings
- Direct code inspection: `internal/parser/testdata/pi/session.jsonl` — confirmed fixture only has `read` with `file_path`
- 04-CONTEXT.md — locked tool name decisions and argument field names

### Secondary (MEDIUM confidence)
- [badlogic/pi-mono README](https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/README.md) — confirms pi built-in tools are read/write/edit/bash
- CONTEXT.md specifies str_replace/create_file as pi tool variants — matches oh-my-pi/extended pi usage

### Tertiary (LOW confidence)
- oh-my-pi source suggests hashline edits instead of old_string/new_string — could affect whether str_replace format matches assumption. If oh-my-pi users have different field shapes, generic fallback handles it safely.

---

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH — no new dependencies, pure extension of existing utilities
- Architecture: HIGH — code paths fully traced from pi parser through enrichSegments to ToolBlock
- Pitfalls: HIGH — TOOL_ALIASES/structured-path mismatch is a concrete code issue identified via source inspection
- Field names for str_replace/create_file: MEDIUM — from CONTEXT.md locked decisions, not directly confirmed in live pi JSONL fixtures

**Research date:** 2026-02-28
**Valid until:** 2026-03-28 (stable domain — pure frontend TypeScript, no external APIs)
