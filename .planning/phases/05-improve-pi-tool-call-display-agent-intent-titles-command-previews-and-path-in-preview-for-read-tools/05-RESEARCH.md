# Phase 5: Improve pi Tool Call Display — Research

**Researched:** 2026-02-28
**Domain:** Frontend TypeScript — tool call collapsed header display (Svelte 5, content-parser.ts, ToolBlock.svelte)
**Confidence:** HIGH

---

## Summary

Phase 5 is a pure frontend rendering improvement building directly on Phase 4. Three concrete display gaps exist in pi tool call blocks:

**Gap 1: `agent__intent` titles.** Pi tool call `arguments` objects frequently carry an `agent__intent` field containing a human-readable description of what the agent is doing (e.g., `"Reading package.json to understand project dependencies"`). This field is currently invisible — it gets swallowed into the raw `input_json` and never surfaced. Displaying it in the collapsed tool header would tell the user *why* a tool was called at a glance.

**Gap 2: Command previews.** Pi uses lowercase tool names `bash`, `read`, `write`, `edit` (OpenCode-style). These are NOT in `TOOL_ALIASES`, so the `enrichSegments` label assignment falls back to the raw name (`bash` not `Bash`). More critically, the Bash command expansion check only matches `"Bash"` and `"run_command"` — it misses lowercase `"bash"`. Result: pi `bash` tool calls show no command preview and display "bash" (lowercase) instead of "Bash".

**Gap 3: Path-in-preview for Read tools.** Pi `read` (and `read_file`) tool calls set `content = ""` in `enrichSegments` (only Bash-type tools get content expansion). So the collapsed `previewLine` (which is `content.split("\n")[0]`) is always empty for Read tools. The file path that IS available in `toolCall.input_json.path` never appears in the collapsed header.

All three fixes are confined to frontend TypeScript/Svelte. No backend changes, no schema changes, no new files.

**Primary recommendation:** (1) Add lowercase pi tool aliases (`bash`, `read`, `write`, `edit`, `grep`, `glob`) to `TOOL_ALIASES` and the Bash expansion check. (2) Add read path expansion to `enrichSegments` for Read-type tool calls. (3) Surface `agent__intent` in `ToolBlock.svelte`'s collapsed header, extracting it from `inputParams.agent__intent` when present.

---

## Standard Stack

No new libraries. All changes are in existing TypeScript utility files and one Svelte component.

### Core (already in use)

| File | Purpose | Phase 5 Relevance |
|------|---------|------------------|
| `frontend/src/lib/utils/content-parser.ts` | Holds `TOOL_ALIASES` map and `enrichSegments` | Add lowercase tool aliases; add Read path expansion |
| `frontend/src/lib/components/content/ToolBlock.svelte` | Renders collapsed/expanded tool block | Surface `agent__intent` in header |
| `frontend/src/lib/utils/tool-params.ts` | `extractToolParamMeta` / `generateFallbackContent` | May need Bash description extraction for lowercase `bash` |
| `frontend/src/lib/utils/content-parser.test.ts` | Vitest tests for content-parser | Add alias and expansion tests |
| `frontend/src/lib/utils/tool-params.test.ts` | Vitest tests for tool-params | Add any new test cases |

### Test Infrastructure

| Property | Value |
|----------|-------|
| Framework | Vitest 4.0.18 |
| Config file | `frontend/vite.config.ts` (implicit) |
| Quick run | `cd frontend && npm test -- --run src/lib/utils/content-parser.test.ts` |
| Full suite | `cd frontend && npm test -- --run` |
| Go tests | `make test-short` |

---

## Architecture Patterns

### How Pi Tool Calls Flow (current state after Phase 4)

```
pi JSONL toolCall: { name: "bash", arguments: { command: "ls -la", agent__intent: "Listing files" } }
      ↓ parsePiAssistantMessage (pi.go)
ParsedToolCall { ToolName: "bash", InputJSON: '{"command":"ls -la","agent__intent":"Listing files"}' }
      ↓ DB storage
ToolCall { tool_name: "bash", category: "Bash", input_json: '{"command":"ls -la","agent__intent":"..."}' }
      ↓ enrichSegments (!hasTextBasedTools branch)
segment = { type: "tool", content: "", label: "bash", toolCall: tc }
         ^^^^^^^^^^^^^^^^                   ^^^^^
         BUG: content empty (Bash expansion misses lowercase "bash")
         BUG: label shows "bash" not "Bash" (not in TOOL_ALIASES)
      ↓ ToolBlock.svelte
previewLine = ""   ← no preview shown
label = "bash"     ← lowercase, inconsistent with Claude's "Bash"
agent__intent field never shown
```

After Phase 5 fixes:
```
segment = { type: "tool", content: "$ ls -la", label: "Bash", toolCall: tc }
ToolBlock header (collapsed): ▶ Bash  $ ls -la
                   or with intent: ▶ Bash  Listing files
```

For pi `read` tool:
```
pi JSONL: { name: "read", arguments: { path: "/src/auth.go", agent__intent: "Reading auth module" } }
      ↓ (current Phase 4 state)
segment = { type: "tool", content: "", label: "Read", toolCall: tc }
previewLine = ""  ← nothing shown in collapsed header

After Phase 5:
previewLine = "/src/auth.go"  or  "Reading auth module"
```

### Pattern 1: TOOL_ALIASES Extension for Lowercase Pi Tool Names

**What:** Add lowercase OpenCode-style tool names (used by pi) to `TOOL_ALIASES`.
**Gap:** `bash`, `read`, `write`, `edit`, `grep`, `glob` are not in `TOOL_ALIASES`. They appear in taxonomy.go but not in the frontend alias map.
**Fix:**

```typescript
// Source: frontend/src/lib/utils/content-parser.ts
const TOOL_ALIASES: Record<string, string> = {
  // ... existing entries ...
  // Lowercase pi/OpenCode tool names
  bash: "Bash",
  read: "Read",
  write: "Write",
  edit: "Edit",
  grep: "Grep",
  glob: "Glob",
};
```

**Note:** `find` (pi) already maps to Read via taxonomy but is not in TOOL_ALIASES. Should add `find: "Read"` as well.

**Confidence:** HIGH — confirmed by direct inspection of real pi session JSONL files (tool names are lowercase) and TOOL_ALIASES source code.

### Pattern 2: Bash Expansion for Lowercase `bash`

**What:** The `enrichSegments` Bash expansion check must include lowercase `bash`.
**Gap (two locations):**

Location 1 — text-marker pairing loop (line ~367):
```typescript
// Current:
if ((tc.tool_name === "Bash" || tc.tool_name === "run_command") && tc.input_json) {
// Fix:
if ((tc.tool_name === "Bash" || tc.tool_name === "bash" || tc.tool_name === "run_command") && tc.input_json) {
```

Location 2 — `!hasTextBasedTools` append loop (line ~406):
```typescript
// Current:
if ((tc.tool_name === "Bash" || tc.tool_name === "run_command") && tc.input_json) {
// Fix:
if ((tc.tool_name === "Bash" || tc.tool_name === "bash" || tc.tool_name === "run_command") && tc.input_json) {
```

**Cleaner alternative:** Extract to a helper `isBashTool(name: string)` to avoid duplicating the condition:
```typescript
function isBashTool(name: string): boolean {
  return name === "Bash" || name === "bash" || name === "run_command";
}
```

**Confidence:** HIGH — confirmed by reading enrichSegments source; lowercase "bash" is the pi tool name per real session data.

### Pattern 3: Read Path Expansion in `enrichSegments`

**What:** For Read-type tools, set `content` to the file path so `previewLine` shows a useful value.
**Gap:** No expansion logic for Read tools; `content = ""` always for non-Bash tools.
**Fix:** Add a parallel expansion for Read-type tools in the `!hasTextBasedTools` append loop:

```typescript
// In !hasTextBasedTools append loop:
const tc = toolCalls[tcIdx++]!;
let content = "";
if (isBashTool(tc.tool_name) && tc.input_json) {
  // ... existing Bash expansion ...
  content = `$ ${fullCmd}`;
} else if (isReadTool(tc.tool_name) && tc.input_json) {
  try {
    const input = JSON.parse(tc.input_json);
    const filePath = input.path ?? input.file_path;
    if (filePath) content = String(filePath);
  } catch { /* leave empty */ }
}
```

Where `isReadTool` would be:
```typescript
function isReadTool(name: string): boolean {
  return name === "Read" || name === "read" || name === "read_file";
}
```

**Alternative approach:** Do this in `ToolBlock.svelte` by computing `previewLine` from `inputParams.path ?? inputParams.file_path` when `content` is empty and `label` is "Read". This avoids touching `enrichSegments` for a display concern.

**Recommendation:** The `ToolBlock.svelte` approach is cleaner because:
- `previewLine` is purely a display concern
- `content` in a segment has semantic meaning (the tool's actual content)
- Keeping `content = ""` for Read tools preserves correctness (Read has no "content" to show expanded)

**Confidence:** HIGH — current behavior confirmed by code trace; both approaches are workable.

### Pattern 4: `agent__intent` Display in ToolBlock

**What:** Extract `agent__intent` from `inputParams` and show it in the collapsed header.
**Where:** `ToolBlock.svelte` — `previewLine` computation or a new derived value.

**Current header (collapsed):**
```
▶ Read   [previewLine or nothing]
```

**Desired behavior:**
- If `inputParams.agent__intent` exists AND `previewLine` is empty: show `agent__intent` as preview
- If both exist: prefer `previewLine` (the actual content) over `agent__intent` (the intent description)
- OR: Always show `agent__intent` when present, in a distinct style (italic, muted color)

**Option A: Intent as fallback previewLine**
```typescript
// ToolBlock.svelte
let previewLine = $derived.by(() => {
  const line = content.split("\n")[0]?.slice(0, 100) ?? "";
  if (line) return line;
  // Fallback to agent__intent for pi tools
  if (inputParams?.agent__intent) {
    return String(inputParams.agent__intent).slice(0, 100);
  }
  return "";
});
```

**Option B: Intent as separate element**
```svelte
{#if collapsed && previewLine}
  <span class="tool-preview">{previewLine}</span>
{/if}
{#if collapsed && !previewLine && inputParams?.agent__intent}
  <span class="tool-preview tool-intent">{inputParams.agent__intent}</span>
{/if}
```

**Option C: Intent always shown if present, after the previewLine**
```svelte
{#if collapsed && previewLine}
  <span class="tool-preview">{previewLine}</span>
{:else if collapsed && inputParams?.agent__intent}
  <span class="tool-preview tool-intent">{String(inputParams.agent__intent).slice(0, 100)}</span>
{/if}
```

**Recommendation:** Option A (intent as fallback previewLine) is simplest and least invasive. The previewLine for Bash is already `$ command` (preferred). For Read, the previewLine would be the file path (after Pattern 3 fix). `agent__intent` then serves as a fallback when neither is available (e.g., `find` with pattern but no path).

**Important:** `agent__intent` must be **excluded** from the generic key:value fallback in `generateFallbackContent`. Currently the generic fallback loops over all params:
```typescript
for (const [key, value] of Object.entries(params)) {
  // This would show "agent__intent: Reading auth module" in expanded view
}
```
This should filter out `agent__intent` (and the top-level `intent` field) from the expanded content display.

**Confidence:** HIGH — agent__intent field confirmed in real pi session data with consistent presence across all tool types that use it.

### Pattern 5: Filter `agent__intent` from Generic Fallback

**What:** The generic `key: value` fallback loop in `generateFallbackContent` currently shows ALL params including `agent__intent`. This should be filtered.

**Fix in `tool-params.ts`:**
```typescript
// Skip meta-parameter that describes intent, not tool input
const SKIP_PARAMS = new Set(["agent__intent"]);

for (const [key, value] of Object.entries(params)) {
  if (SKIP_PARAMS.has(key)) continue;
  if (value == null || value === "") continue;
  // ...
}
```

**Confidence:** HIGH — confirmed by inspecting real pi session data where `agent__intent` is present in arguments alongside actual tool params.

---

## Field Name Survey (Confirmed from Real Pi Session Data)

| Pi Tool | Args Keys | `agent__intent` Present | Notes |
|---------|-----------|------------------------|-------|
| `bash` | `command`, `agent__intent`, `timeout?`, `cwd?`, `head?` | ~30% of calls | Most calls use it |
| `read` | `path`, `agent__intent`, `offset?`, `limit?` | ~10% of calls | Directory reads often have it |
| `edit` | `path`, `edits`, `agent__intent?` | ~22% of calls | Uses `edits` array (NOT old_string/new_string) |
| `write` | `path`, `content`, `agent__intent?` | ~10% of calls | |
| `grep` | `pattern`, `path?`, `agent__intent?`, `glob?` | ~13% of calls | |
| `find` | `pattern`, `agent__intent?` | ~8% of calls | |
| `puppeteer` | `action`, `agent__intent?`, `url?`, etc. | ~31% of calls | Browser tool, unlikely to alias |
| `fetch` | `url`, `agent__intent?`, `timeout?` | ~21% of calls | HTTP fetch tool |
| `todo_write` | `ops`, `agent__intent?` | ~6% of calls | |
| `ask` | `questions` | 0% | No intent |
| `task` | `agent`, `context`, `tasks` | 0% | |

**Key observation:** `agent__intent` is in the arguments object (alongside tool params), NOT a top-level `intent` field. The JSONL also has a top-level `intent` field on the `toolCall` block itself, but the parser stores only `block.Get("arguments").Raw` as `InputJSON`. So `agent__intent` IS available in `inputParams` in the frontend.

**Pi `edit` tool format:** Pi's native `edit` tool uses an `edits` array (not `old_string`/`new_string`). The `edits` array contains operations like `{op: "append", lines: [...]}` or `{op: "replace", pos: "hashline", lines: [...]}`. This is very different from Claude Code's Edit format. The current `generateFallbackContent("Edit", params)` will find no `old_string`/`new_string` and fall through to the generic key:value loop — which will dump the large `edits` array. **Phase 5 should consider suppressing or truncating the `edits` content.** However, this may be out of scope for this phase.

---

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Tool alias lookup | Custom display-name map per agent | Extend existing `TOOL_ALIASES` | Already established pattern |
| Content preview for Read | Separate Read preview component | Extend `ToolBlock.svelte` `previewLine` derivation | Keeps display logic colocated |
| intent display | Separate intent component | Inline `agent__intent` as preview fallback | Simpler, consistent with existing preview slot |
| param filtering | Separate filter function per tool | Add to `SKIP_PARAMS` set in generateFallbackContent | Existing loop already handles per-key decisions |

---

## Common Pitfalls

### Pitfall 1: Forgetting lowercase `bash` in both expansion locations
**What goes wrong:** Bash expansion added to one loop but not the other (text-marker pairing vs `!hasTextBasedTools` append).
**Why it happens:** Two separate code paths in `enrichSegments` for two routing strategies.
**How to avoid:** Extract `isBashTool(name)` helper used in both places.
**Warning signs:** Pi `bash` with multi-line commands expands in one context but not the other.

### Pitfall 2: `agent__intent` appearing in expanded fallback content
**What goes wrong:** After fixing aliases so `bash` falls through to the generic fallback, `agent__intent: "..."` appears in the expanded tool content — ugly and redundant with the header.
**Why it happens:** `generateFallbackContent` loops over all params without filtering.
**How to avoid:** Add `agent__intent` to a skip-set in `generateFallbackContent`.
**Warning signs:** Expanded tool block shows "agent__intent: ..." as a content line.

### Pitfall 3: `previewLine` for Read tool showing full path when truncated
**What goes wrong:** Long file paths get sliced at 100 chars in previewLine but the full path is available in the meta tag (file: ...) — this is actually fine, 100 chars is enough for a path preview.
**Why it happens:** Not actually a pitfall — just a design note. truncate(filePath, 100) is consistent with other meta tag truncation.

### Pitfall 4: Pi `edit` tool expanded content dumping large `edits` array
**What goes wrong:** The pi native `edit` tool has `edits: [{op: "replace", pos: "846#VH", lines: [...many lines...]}]`. The generic fallback tries to JSON-stringify this, producing a wall of text.
**Why it happens:** Pi's `edit` uses a hashline-based diff format, not old_string/new_string. The current `generateFallbackContent("Edit", params)` doesn't match old_string/new_string and falls through to the generic loop.
**How to avoid:** In `generateFallbackContent`, handle the `edits` array case: show `N edits to path` or just the op types, rather than the full array.
**Note:** This might be a Phase 5 item or Phase 6 — depends on scope decision.

### Pitfall 5: `read` (lowercase) tool with `path` param — `extractToolParamMeta` already handles it
**What goes wrong:** Developer re-adds path handling for lowercase `read`.
**Why it happens:** Phase 4 already added `params.file_path ?? params.path` to the `"Read"` branch of `extractToolParamMeta`. BUT — the label for lowercase `read` is currently `"read"` (not `"Read"`) because it's not in TOOL_ALIASES. So `extractToolParamMeta("read", params)` doesn't match the `"Read"` branch!
**How to avoid:** Adding lowercase aliases to TOOL_ALIASES (Pattern 1) fixes this automatically — after aliasing, `label = "Read"` and `extractToolParamMeta("Read", params)` hits the correct branch.
**Warning signs:** Meta tag still missing for pi lowercase `read` tool after adding path expansion.

---

## Code Examples

### Current TOOL_ALIASES (content-parser.ts)
```typescript
// Source: frontend/src/lib/utils/content-parser.ts
const TOOL_ALIASES: Record<string, string> = {
  exec_command: "Bash",
  shell_command: "Bash",
  write_stdin: "Bash",
  shell: "Bash",
  apply_patch: "Edit",
  // Pi tool names (added Phase 4)
  str_replace: "Edit",
  run_command: "Bash",
  create_file: "Write",
  read_file: "Read",
  // MISSING: bash, read, write, edit, grep, glob, find (lowercase pi/OpenCode names)
};
```

After Phase 5:
```typescript
const TOOL_ALIASES: Record<string, string> = {
  // ... existing ...
  // Lowercase pi/OpenCode tool names
  bash: "Bash",
  read: "Read",
  write: "Write",
  edit: "Edit",
  grep: "Grep",
  glob: "Glob",
  find: "Read",
};
```

### Current enrichSegments Bash expansion (content-parser.ts ~367)
```typescript
// Source: frontend/src/lib/utils/content-parser.ts
// Text-marker pairing loop:
if ((tc.tool_name === "Bash" || tc.tool_name === "run_command") && tc.input_json) {
// Fix: add "bash" lowercase
if ((tc.tool_name === "Bash" || tc.tool_name === "bash" || tc.tool_name === "run_command") && tc.input_json) {

// !hasTextBasedTools append loop (~406):
if ((tc.tool_name === "Bash" || tc.tool_name === "run_command") && tc.input_json) {
// Fix: same addition
if ((tc.tool_name === "Bash" || tc.tool_name === "bash" || tc.tool_name === "run_command") && tc.input_json) {
```

### Read Path Expansion in `!hasTextBasedTools` loop
```typescript
// Add after Bash expansion, in the !hasTextBasedTools append loop:
} else if ((tc.tool_name === "Read" || tc.tool_name === "read" || tc.tool_name === "read_file") && tc.input_json) {
  try {
    const input = JSON.parse(tc.input_json);
    const filePath = input.path ?? input.file_path;
    if (filePath) {
      content = String(filePath);
    }
  } catch { /* leave empty */ }
}
```

### agent__intent as previewLine fallback in ToolBlock.svelte
```typescript
// Source: frontend/src/lib/components/content/ToolBlock.svelte
// Current:
let previewLine = $derived(
  content.split("\n")[0]?.slice(0, 100) ?? "",
);

// After Phase 5:
let previewLine = $derived.by(() => {
  const line = content.split("\n")[0]?.slice(0, 100) ?? "";
  if (line) return line;
  if (inputParams?.agent__intent) {
    return String(inputParams.agent__intent).slice(0, 100);
  }
  return "";
});
```

### Filter agent__intent from generateFallbackContent
```typescript
// Source: frontend/src/lib/utils/tool-params.ts
// In generateFallbackContent generic fallback loop:
const INTERNAL_PARAMS = new Set(["agent__intent"]);

for (const [key, value] of Object.entries(params)) {
  if (INTERNAL_PARAMS.has(key)) continue;  // ADD THIS
  if (value == null || value === "") continue;
  // ...
}
```

---

## Scope Decision: Pi `edit` Tool Expanded Content

The pi native `edit` tool uses an `edits` array with hashline positions (e.g., `pos: "846#VH"`). This is opaque to humans. Current behavior: the generic fallback dumps all params including the large edits array.

**Options:**
1. **Show op summary:** `"3 replace operations"` (Phase 5 scope)
2. **Show lines content only:** Extract all `lines` arrays from edits and join (Phase 5 scope, more work)
3. **Defer to later phase:** Accept that pi `edit` expanded content is unreadable for now

**Recommendation:** Defer pi `edit` expanded content to a future phase. Focus Phase 5 on the three named goals: agent__intent display, command previews, and path-in-preview. The `edits` format is a separate problem requiring more design work.

---

## Open Questions

1. **Should `agent__intent` display replace or supplement `previewLine`?**
   - What we know: For Bash tools, `previewLine` = `$ command` (useful, specific). For Read tools, `previewLine` = file path (useful, specific). For other tools, `previewLine` may be empty.
   - Recommendation: Show `agent__intent` ONLY as fallback when `previewLine` is empty. Prefer actual content over intent description.

2. **Should `agent__intent` appear in the meta tags section when expanded?**
   - What we know: Currently meta tags show file path, description, etc. `agent__intent` is a different kind of metadata (human-readable purpose statement).
   - Recommendation: No — keep `agent__intent` in the header preview only. The expanded view should show tool params, not repeat the intent.

3. **Are lowercase `grep` and `glob` pi tool names actually used?**
   - What we know: From real session data analysis, `grep` (165 uses) is the pi tool name. `glob` not observed but likely exists.
   - Recommendation: Add both to TOOL_ALIASES defensively.

4. **Does the pi `bash` tool use `description` field for meta tags?**
   - What we know: `extractToolParamMeta("Bash", params)` shows `description` if present. Pi `bash` args don't include a `description` field — just `command`, `agent__intent`, `timeout`, `cwd`.
   - Recommendation: The existing Bash meta tag handler is fine. `agent__intent` is shown in previewLine (not as a meta tag), so no change needed to `extractToolParamMeta`.

---

## Validation Architecture

*(nyquist_validation not enabled in .planning/config.json — section skipped)*

---

## Sources

### Primary (HIGH confidence)
- Direct code inspection: `frontend/src/lib/utils/content-parser.ts` — confirmed TOOL_ALIASES contents and enrichSegments Bash expansion condition
- Direct code inspection: `frontend/src/lib/components/content/ToolBlock.svelte` — confirmed previewLine derivation and how it's displayed
- Direct code inspection: `frontend/src/lib/utils/tool-params.ts` — confirmed generateFallbackContent generic param loop
- Real pi session data: `~/.pi/agent/sessions/**/*.jsonl` — confirmed `agent__intent` field in tool arguments, lowercase tool names, `edits` array format for `edit` tool
- Tool statistics from real sessions: `bash` 885 calls, `read` 777 calls, `edit` 385 calls, all using lowercase names

### Secondary (MEDIUM confidence)
- Phase 4 research and implementation (04-RESEARCH.md, 04-01-PLAN.md) — confirmed Phase 4 scope and what was/wasn't done
- taxonomy.go inspection — confirmed lowercase tool names already mapped to categories (but not in frontend TOOL_ALIASES)

### Tertiary (LOW confidence)
- None

---

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH — no new dependencies, pure TypeScript/Svelte extensions
- Architecture: HIGH — all three gaps confirmed by code trace and real data
- agent__intent field: HIGH — confirmed present in real pi sessions, consistent format
- Lowercase tool alias gap: HIGH — confirmed by TOOL_ALIASES inspection and real session tool names
- Pi `edit` edits array: HIGH — confirmed format from real data, but handling strategy is deferred

**Research date:** 2026-02-28
**Valid until:** 2026-03-28 (stable domain — pure frontend TypeScript/Svelte, no external APIs)
