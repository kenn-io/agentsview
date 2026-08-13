# VS Code Copilot Response Item Parsing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans
> to implement this plan directly in the current agent, task-by-task. Never use
> subagent-driven development. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Restore structured tool calls, nested terminal commands, and visible
file references from VS Code 1.132 Copilot sessions.

**Approved spec/design:**
`docs/superpowers/specs/2026-08-12-vscode-copilot-response-items-design.md`

**Architecture:** Keep `request.response` as the authoritative ordered source.
Decode unused status fields without fixed scalar types, convert inline file
references to visible inline-code text, and support both flat and nested
terminal command fields in the shared VS Code/Positron parser.

**Tech Stack:** Go, `encoding/json`, Testify, VS Code JSONL session logs.

## Global Constraints

- Do not parse `result.metadata.toolCallRounds`; duplicate calls already exist
  in `request.response`.
- Preserve old flat `toolSpecificData.command` support.
- Render references as visible inline code, not local `file://` links.
- Derive the test fixture by hand from issue #1351 and assert parsed behavior.
- Update `docs/internal/session-format-sources.md` with the VS Code 1.132
  evidence.

______________________________________________________________________

### Task 1: Add the VS Code 1.132 Regression Test

**Files:**

- Modify: `internal/parser/vscode_copilot_test.go`

**Interfaces:**

- Consumes: `parseVSCodeCopilotTestSession(t, path, project, machine)`.

- Produces: A regression test for the observable `[]ParsedMessage` contract.

- [x] **Step 1: Write the failing JSONL parser test**

    Add `TestParseVSCodeCopilotSession_VSCode132ResponseItems`. Write a temporary
    JSONL session with one initial operation and one request push. Its response
    must contain two `copilot_readFile` calls and one `run_in_terminal` call,
    each with `"isConfirmed":{"type":1}`. Put `uname -a` in
    `toolSpecificData.commandLine.original`, and put `inlineReference` items for
    `/workspace/test.txt` between the text fragments `"I'll read "`,
    `" first. "`, and `" contains the command."`.

    Assert these literal outcomes:

    ```go
    require.Len(t, msgs, 2, "user and assistant messages")
    assistant := msgs[1]
    assert.True(t, assistant.HasToolUse)
    require.Len(t, assistant.ToolCalls, 3)
    assert.Equal(t, []string{"copilot_readFile", "copilot_readFile", "run_in_terminal"}, []string{
        assistant.ToolCalls[0].ToolName,
        assistant.ToolCalls[1].ToolName,
        assistant.ToolCalls[2].ToolName,
    })
    assert.JSONEq(t, `{"command":"uname -a","message":"Running uname"}`, assistant.ToolCalls[2].InputJSON)
    assert.Contains(t, assistant.Content, "I'll read `/workspace/test.txt` first.")
    assert.Contains(t, assistant.Content, "`/workspace/test.txt` contains the command.")
    ```

- [x] **Step 2: Run the focused test and verify RED**

    Run:

    ```bash
    go test ./internal/parser -run TestParseVSCodeCopilotSession_VSCode132ResponseItems -count=1
    ```

    Expected: FAIL because `isConfirmed` cannot unmarshal into `bool`; the
    assistant has no tool calls, its nested command is absent, and inline
    references are missing.

### Task 2: Parse Flexible Status, References, and Nested Commands

**Files:**

- Modify: `internal/parser/vscode_copilot.go`
- Test: `internal/parser/vscode_copilot_test.go`

**Interfaces:**

- Consumes: `vscodeCopilotResponseItem` and raw `inlineReference` /
  `toolSpecificData` JSON.

- Produces: `extractVSCodeInlineReference(json.RawMessage) string` and extended
  `extractVSCopilotInputJSON(...) string` behavior.

- [x] **Step 1: Make status fields schema-tolerant**

    Remove the unused boolean `IsConfirmed` and `IsComplete` fields from
    `vscodeCopilotResponseItem`. `encoding/json` ignores those producer fields,
    so their scalar or object representation cannot invalidate the stable fields
    that Agentsview consumes.

- [x] **Step 2: Add inline-reference extraction**

    Add a small struct containing `FSPath`, `Path`, and `External`. Implement
    `extractVSCodeInlineReference` to prefer `fsPath`, then `path`, then strip
    the `file://` prefix from `external`. Return "`" + path + "`" for a
    non-empty path and `""` for malformed or empty input. In the
    `inlineReference` switch case, append that string to `textParts` when
    non-empty.

- [x] **Step 3: Support nested terminal command data**

    Extend `vscodeCopilotToolData` with:

    ```go
    CommandLine struct {
        Original string `json:"original"`
    } `json:"commandLine,omitempty"`
    ```

    In `extractVSCopilotInputJSON`, prefer the existing non-empty `Command`, then
    use `CommandLine.Original`. Store the selected value under the existing
    `command` key.

- [x] **Step 4: Run focused VS Code and Positron parser tests and verify GREEN**

    Run:

    ```bash
    go test ./internal/parser -run 'TestParseVSCodeCopilotSession|TestExtractVSCopilotInputJSON|TestPositron' -count=1
    ```

    Expected: PASS. The new test proves the three issue behaviors, while existing
    tests prove the older boolean/flat-command shapes still parse.

### Task 3: Record Format Provenance and Verify the Change

**Files:**

- Modify: `docs/internal/session-format-sources.md`
- Modify: `internal/db/db.go`
- Test: `internal/db/db_test.go`
- Modify: `docs/superpowers/plans/2026-08-12-vscode-copilot-response-items.md`

**Interfaces:**

- Consumes: The observed issue #1351 session artifact and focused test result.

- Produces: An updated VS Code Copilot evidence entry and completed plan record.

- [x] **Step 1: Update the VS Code Copilot evidence entry**

    Add a concise note to the `VS Code Copilot (vscode-copilot)` section. State
    that issue #1351's VS Code 1.132 JSONL artifact was checked on 2026-08-12
    and records object-valued `isConfirmed`, nested
    `toolSpecificData.commandLine.original`, and ordered `inlineReference`
    items. State that Agentsview consumes the final response array rather than
    duplicate metadata tool-call rounds.

- [x] **Step 2: Format and verify**

    Set `dataVersion` to 86 and update its contract test so unchanged archived
    sessions are rebuilt through the existing non-destructive full-resync path.
    Document that version 86 reparses VS Code Copilot and Positron sessions to
    restore response items.

    Run:

    ```bash
    gofmt -w internal/parser/vscode_copilot.go internal/parser/vscode_copilot_test.go
    go test ./internal/parser -count=1
    go vet ./...
    git diff --check
    ```

    Expected: all commands pass with no warnings or formatting errors.

- [x] **Step 3: Review the complete diff**

    Run:

    ```bash
    git status --short
    git diff HEAD
    ```

    Confirm the diff contains only the approved parser behavior, regression test,
    provenance note, and implementation plan. Confirm no private paths from the
    supplied session appear in tracked files.

- [x] **Step 4: Mark this plan complete and commit**

    Change every task checkbox in this plan to `[x]`. Re-run `git diff --check`,
    stage only the parser, data-version, provenance, and plan files, and commit
    with:

    ```bash
    git commit -m "fix(parser): restore VS Code Copilot response items" \
      -m "VS Code 1.132 changed persisted status, terminal command, and inline-reference shapes. Accept those shapes so archived sessions retain structured tool calls and readable file references without duplicating metadata tool-call rounds."
    ```

    Do not bypass hooks. If a hook changes files, inspect and restage the intended
    changes before retrying.

## Verification Result

The affected parser and data-version packages pass with the project's `fts5` tag
in an isolated scratch HOME. Repository-wide tagged `go vet` also passes in
isolated state. `make test-short` reaches and passes `internal/parser`, but the
whole target remains red because native macOS file events do not arrive in the
test environment. The same timeouts reproduce when `internal/fsevents` and the
real-watcher tests in `internal/sync` run independently; neither package is
modified by this change.
