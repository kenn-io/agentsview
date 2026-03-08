---
status: awaiting_human_verify
trigger: "Some Edit tool blocks show rich content while others show only the file name or nothing at all"
created: 2026-02-28T00:00:00Z
updated: 2026-02-28T00:00:00Z
---

## Current Focus

hypothesis: CONFIRMED - generateFallbackContent checks edit.content (array) for Pi edits but real Pi format uses edit.lines (array) for replacement content
test: Verified by querying DB and reading actual Pi input_json: {op:"replace",pos:"846#VH",lines:[...]}
expecting: Fix edit.lines handling in generateFallbackContent resolves the display issue
next_action: Fix generateFallbackContent to handle edit.lines in Pi edits branch

## Symptoms

expected: All Edit tool blocks should show what was changed (old/new text or structured edit content) when expanded
actual: Some Edit blocks show diff content when expanded; others show only "file: src/styles.css" (meta tag) with no content body, or are completely empty
errors: No JS errors reported
reproduction: Compare khipu_genomics_site session from 10h ago vs Feb 19 in the agentsview UI — one has content in Edit blocks, the other does not
started: Noticed today; recent commits to feat/pi-support branch have been fixing related rendering bugs

## Eliminated

- hypothesis: Both sessions are same agent type with same input_json format
  evidence: DB shows pi session (147fbc2eb97974b0) uses {path, edits:[{op,pos,end,lines}]} while claude sessions use {file_path, old_string, new_string}
  timestamp: 2026-02-28T00:10:00Z

- hypothesis: ToolBlock.svelte passes wrong argument to generateFallbackContent
  evidence: ToolBlock.svelte line 113 correctly uses toolCall.category || toolCall.tool_name, which resolves to "Edit" for pi edit tool
  timestamp: 2026-02-28T00:10:00Z

## Evidence

- timestamp: 2026-02-28T00:05:00Z
  checked: DB sessions table for khipu_genomics_site sessions
  found: Pi session 147fbc2eb97974b0 (2026-02-28T00:52:20Z), Claude sessions from Feb 21, OpenCode sessions from today
  implication: Multiple agent types affect khipu_genomics_site; disparity is agent-type-dependent

- timestamp: 2026-02-28T00:06:00Z
  checked: DB tool_calls for pi session 147fbc2eb97974b0 Edit calls
  found: input_json = {path:"src/styles.css", edits:[{op:"replace",pos:"846#VH",end:"851#WT",lines:[...array of CSS strings...]}], agent__intent:"..."}
  implication: Pi uses "lines" field (NOT "content") for replacement text array

- timestamp: 2026-02-28T00:08:00Z
  checked: generateFallbackContent in tool-params.ts Pi branch (lines 133-148)
  found: Code checks edit.set_line (for {anchor,new_text} format) or edit.tag + edit.content (array). Real Pi format has edit.op + edit.pos + edit.end + edit.lines — none of these fields are handled
  implication: Root cause confirmed. The "content" field check never matches real Pi data; "lines" is the actual array field

## Resolution

root_cause: generateFallbackContent in tool-params.ts checked edit.content (array) for Pi edits[] entries, but the real Pi agent format uses edit.lines (array of replacement strings) along with edit.op and edit.pos. The content field was never present in real Pi data, so Pi Edit blocks always returned null from generateFallbackContent and showed no content when expanded.

fix: Added a new branch in the Pi edits[] loop: when Array.isArray(edit.lines) is true, render `op @ pos` as header + join edit.lines as the replacement content. Preserved existing set_line and op/tag/content paths as fallbacks.

verification: All 48 tool-params tests pass including 2 new tests for the real Pi format. Frontend rebuilt and deployed to internal/web/dist. Pre-existing sessions.test.ts failure confirmed unrelated (existed before this change).

files_changed:
  - frontend/src/lib/utils/tool-params.ts (lines 132-154: restructured Pi edits branch to handle edit.lines)
  - frontend/src/lib/utils/tool-params.test.ts (lines 278-308: 2 new tests for op/pos/lines format)
commit: 2a7f807
