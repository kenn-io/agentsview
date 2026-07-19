# Grok Build Parser Compatibility Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the Grok provider parse every local transcript shape supported by
the pinned Grok Build source and verify the boundary with upstream-generated
golden sessions.

**Architecture:** Keep discovery unchanged and normalize each `chat_history` row
independently into the existing `ParsedMessage` model, selecting current `type`
rows or legacy `role` rows per line. Generate checked-in input fixtures with
Grok Build's Rust serializers and downgrade binary, but keep Go-side parse
expectations hand-authored so the test oracle is independent.

**Tech Stack:** Go 1.26, `gjson`, `testify`, Rust/Cargo for one-time upstream
fixture generation, Grok Build commit
`7cfcb20d2b50b0d18801a6c0af2e401c0e060894`.

## Global Constraints

- Do not change branches; the Codex checkout is detached and branch changes
  require explicit user permission.
- Do not push, pull, rebase, amend, merge, or open a pull request unless the
  user explicitly requests it.
- Never use `--no-verify`; allow repository hooks to run on every commit.
- Commit every tracked-file change with a focused conventional commit.
- Use `apply_patch` for hand-authored file changes and preserve unrelated work.
- Keep private names, hostnames, identities, infrastructure details, and
  absolute user paths out of fixtures, docs, and commit messages.
- Use `testify`: `require` for setup/length checks and `assert` for independent
  observable results.
- Run `go fmt ./...` and `go vet ./...` after Go changes.
- Do not discover nested `subagents/{id}` directories; Grok Build excludes those
  session kinds from its normal user-visible listing.
- Do not parse `updates.jsonl`, invent per-message timestamps, expose encrypted
  reasoning, or infer output-token totals absent from persisted files.

## File Structure

- Create `internal/parser/testdata/grok-build/README.md` for fixture provenance
  and reproducible generation commands.
- Create `internal/parser/testdata/grok-build/generate.rs` as the exact
  source-local example compiled against the pinned Grok Build checkout.
- Create `internal/parser/testdata/grok-build/current/...` for serializer-made
  format 1 `summary.json`, `signals.json`, and `chat_history.jsonl`.
- Create `internal/parser/testdata/grok-build/legacy/...` for serializer-made
  metadata and the format 0 history emitted by Grok Build's downgrade binary.
- Modify `internal/parser/grok_test.go` with a shared golden-copy/parse helper
  and focused public-provider behavior tests.
- Modify `internal/parser/grok.go` with per-line format dispatch, reasoning and
  backend-tool normalization, current summary semantics, fork metadata, and
  source-backed signal handling.

______________________________________________________________________

### Task 1: Generate source-backed fixtures and correct title precedence

**Files:**

- Create: `internal/parser/testdata/grok-build/README.md`
- Create: `internal/parser/testdata/grok-build/generate.rs`
- Create:
  `internal/parser/testdata/grok-build/current/%2Fworkspace%2Fgrok-worktrees%2Fparser-audit/019f6000-0000-7000-8000-000000000001/summary.json`
- Create:
  `internal/parser/testdata/grok-build/current/%2Fworkspace%2Fgrok-worktrees%2Fparser-audit/019f6000-0000-7000-8000-000000000001/signals.json`
- Create:
  `internal/parser/testdata/grok-build/current/%2Fworkspace%2Fgrok-worktrees%2Fparser-audit/019f6000-0000-7000-8000-000000000001/chat_history.jsonl`
- Create:
  `internal/parser/testdata/grok-build/legacy/%2Fworkspace%2Fagentsview/019f6000-0000-7000-8000-000000000002/summary.json`
- Create:
  `internal/parser/testdata/grok-build/legacy/%2Fworkspace%2Fagentsview/019f6000-0000-7000-8000-000000000002/signals.json`
- Create:
  `internal/parser/testdata/grok-build/legacy/%2Fworkspace%2Fagentsview/019f6000-0000-7000-8000-000000000002/source-v1.jsonl`
- Create:
  `internal/parser/testdata/grok-build/legacy/%2Fworkspace%2Fagentsview/019f6000-0000-7000-8000-000000000002/chat_history.jsonl`
- Modify: `internal/parser/grok_test.go:15-29`
- Modify: `internal/parser/grok_test.go:81-145`
- Modify: `internal/parser/grok.go:15-28`
- Modify: `internal/parser/grok.go:471-520`

**Interfaces:**

- Consumes: `xai_grok_shell::session::persistence::Summary`,
  `xai_grok_shell::session::signals::SessionSignals`, and
  `xai_grok_shell::sampling::ConversationItem` from the pinned checkout.

- Produces: `parseGrokGolden(t *testing.T, generation string) ParseResult`, used
  by later tasks, plus stable current/legacy source fixtures.

- [ ] **Step 1: Add the reproducible upstream generator source**

Add `generate.rs` with concrete fixed data and real upstream serde types:

```rust
use serde::{de::DeserializeOwned, Serialize};
use std::{env, fs, path::Path};
use xai_grok_shell::{
    sampling::ConversationItem,
    session::{persistence::Summary, signals::SessionSignals},
};

const CURRENT_ID: &str = "019f6000-0000-7000-8000-000000000001";
const LEGACY_ID: &str = "019f6000-0000-7000-8000-000000000002";

fn normalize_json<T: DeserializeOwned + Serialize>(raw: &str) -> Vec<u8> {
    let value: T = serde_json::from_str(raw).expect("fixture must match upstream type");
    let mut bytes = serde_json::to_vec_pretty(&value).expect("fixture must serialize");
    bytes.push(b'\n');
    bytes
}

fn write_summary(path: &Path, id: &str, cwd: &str, version: u8) {
    let raw = format!(r#"{{
      "info": {{"id": "{id}", "cwd": "{cwd}"}},
      "session_summary": "Inspect parser compatibility",
      "generated_title": "Audit Grok compatibility",
      "created_at": "2026-07-18T10:00:00Z",
      "updated_at": "2026-07-18T10:30:00Z",
      "last_active_at": "2026-07-18T10:29:00Z",
      "num_messages": 12,
      "num_chat_messages": 8,
      "current_model_id": "grok-4.5",
      "chat_format_version": {version},
      "parent_session_id": "019f5000-0000-7000-8000-000000000000",
      "source_workspace_dir": "/workspace/agentsview",
      "git_root_dir": "/workspace/agentsview",
      "head_branch": "feature/parser-audit",
      "agent_name": "grok-build"
    }}"#);
    fs::write(path.join("summary.json"), normalize_json::<Summary>(&raw)).unwrap();
}

fn write_signals(path: &Path) {
    let raw = r#"{
      "turnCount": 2,
      "userMessageCount": 2,
      "assistantMessageCount": 2,
      "contextTokensUsed": 12000,
      "contextWindowTokens": 200000,
      "primaryModelId": "grok-4.5"
    }"#;
    fs::write(
        path.join("signals.json"),
        normalize_json::<SessionSignals>(raw),
    )
    .unwrap();
}

fn write_v1(path: &Path) {
    let rows = [
        r#"{"type":"system","content":"You are Grok"}"#,
        r#"{"type":"user","content":[{"type":"text","text":"project policy"}],"synthetic_reason":"project_instructions"}"#,
        r#"{"type":"user","content":[{"type":"text","text":"Review parser compatibility"}],"prompt_index":0}"#,
        r#"{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"Inspect both formats"}]}"#,
        r#"{"type":"backend_tool_call","kind":{"tool_type":"web_search","id":"ws_1","status":"completed","action":{"type":"search","query":"Grok Build persistence","sources":[]}}}"#,
        r#"{"type":"assistant","content":"Reading the implementation.","tool_calls":[{"id":"call_1","name":"read_file","arguments":"{\"target_file\":\"src/session/persistence.rs\"}"}],"model_id":"grok-4.5"}"#,
        r#"{"type":"tool_result","tool_call_id":"call_1","content":"source body"}"#,
        r#"{"type":"user","content":[{"type":"text","text":"also keep interjections"}],"synthetic_reason":"interjection"}"#,
        r#"{"type":"assistant","content":"Compatibility checked.","model_id":"grok-4.5"}"#,
    ];
    let mut out = Vec::new();
    for row in rows {
        let value: ConversationItem = serde_json::from_str(row).unwrap();
        serde_json::to_writer(&mut out, &value).unwrap();
        out.push(b'\n');
    }
    fs::write(path.join("chat_history.jsonl"), out).unwrap();
}

fn write_legacy_source(path: &Path) {
    let rows = [
        r#"{"type":"system","content":"You are Grok"}"#,
        r#"{"type":"user","content":[{"type":"text","text":"Review parser compatibility"}],"prompt_index":0}"#,
        r#"{"type":"reasoning","id":"rs_v0","summary":[{"type":"summary_text","text":"Check the old format"}]}"#,
        r#"{"type":"assistant","content":"Reading the implementation.","tool_calls":[{"id":"call_1","name":"read_file","arguments":"{\"target_file\":\"src/session/persistence.rs\"}"}],"model_id":"grok-4.5"}"#,
        r#"{"type":"tool_result","tool_call_id":"call_1","content":"source body"}"#,
        r#"{"type":"assistant","content":"Compatibility checked.","model_id":"grok-4.5"}"#,
    ];
    let mut out = Vec::new();
    for row in rows {
        let value: ConversationItem = serde_json::from_str(row).unwrap();
        serde_json::to_writer(&mut out, &value).unwrap();
        out.push(b'\n');
    }
    fs::write(path.join("source-v1.jsonl"), out).unwrap();
}

fn main() {
    let output = env::args().nth(1).expect("usage: agentsview_golden OUTPUT");
    let root = Path::new(&output);
    let current = root
        .join("current")
        .join("%2Fworkspace%2Fgrok-worktrees%2Fparser-audit")
        .join(CURRENT_ID);
    let legacy = root
        .join("legacy")
        .join("%2Fworkspace%2Fagentsview")
        .join(LEGACY_ID);
    fs::create_dir_all(&current).unwrap();
    fs::create_dir_all(&legacy).unwrap();
    write_summary(&current, CURRENT_ID, "/workspace/grok-worktrees/parser-audit", 1);
    write_summary(&legacy, LEGACY_ID, "/workspace/agentsview", 0);
    write_signals(&current);
    write_signals(&legacy);
    write_v1(&current);
    write_legacy_source(&legacy);
}
```

- [ ] **Step 2: Generate and record the golden bytes**

Place the generator at
`crates/codegen/xai-grok-shell/examples/agentsview_golden.rs` in a disposable
checkout of the pinned commit, then run:

```bash
GROK_BUILD_CHECKOUT=/tmp/grok-build-upstream-20260719
cargo run --manifest-path "$GROK_BUILD_CHECKOUT/Cargo.toml" \
  -p xai-grok-shell --example agentsview_golden -- \
  internal/parser/testdata/grok-build
cargo run --manifest-path "$GROK_BUILD_CHECKOUT/Cargo.toml" \
  -p xai-grok-shell --bin chat-history-downgrade -- \
  internal/parser/testdata/grok-build/legacy/%2Fworkspace%2Fagentsview/019f6000-0000-7000-8000-000000000002/source-v1.jsonl \
  internal/parser/testdata/grok-build/legacy/%2Fworkspace%2Fagentsview/019f6000-0000-7000-8000-000000000002/chat_history.jsonl
```

Record the pinned commit, the example destination, and those commands in
`README.md`. Confirm no generated file contains a home directory, hostname,
email, token, or real repository path:

```bash
rg -n '/Users/|/home/|@|token|localhost' internal/parser/testdata/grok-build
```

Expected: only the intentional explanatory use of “token” in `README.md`, if
present; no private path or identity matches in JSON/JSONL.

- [ ] **Step 3: Add the golden provider helper and failing title test**

Add this reusable test helper and a focused test:

```go
func parseGrokGolden(t *testing.T, generation string) ParseResult {
    t.Helper()
    root := t.TempDir()
    fixtureRoot := filepath.Join("testdata", "grok-build", generation)
    require.NoError(t, os.CopyFS(root, os.DirFS(fixtureRoot)))
    provider := newGrokTestProvider(t, root)
    sources, err := provider.Discover(context.Background())
    require.NoError(t, err)
    require.Len(t, sources, 1)
    outcome, err := provider.Parse(
        context.Background(),
        ParseRequest{Source: sources[0]},
    )
    require.NoError(t, err)
    require.Len(t, outcome.Results, 1)
    return outcome.Results[0].Result
}

func TestGrokProviderGoldenCurrentPrefersGeneratedTitle(t *testing.T) {
    result := parseGrokGolden(t, "current")
    assert.Equal(t, "Audit Grok compatibility", result.Session.SessionName)
}
```

- [ ] **Step 4: Run the title test and verify red**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/parser \
  -run '^TestGrokProviderGoldenCurrentPrefersGeneratedTitle$' -count=1
```

Expected: FAIL because the parser returns `Inspect parser compatibility`.

- [ ] **Step 5: Correct title precedence minimally**

Change `decodeGrokSummary` to choose the upstream display order:

```go
Summary: firstNonEmptyJSONLString(
    strings.TrimSpace(root.Get("generated_title").String()),
    strings.TrimSpace(root.Get("session_summary").String()),
    strings.TrimSpace(root.Get("summary").String()),
),
```

- [ ] **Step 6: Run focused and existing Grok tests**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/parser \
  -run '^TestGrokProvider' -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit Task 1**

Load the mandatory commit skill, review the staged fixture bytes, allow hooks,
and commit only Task 1 files:

```bash
git add internal/parser/grok.go internal/parser/grok_test.go \
  internal/parser/testdata/grok-build
git commit -m "test(parser): pin Grok Build golden sessions" \
  -m "Source-generated fixtures make the parser contract auditable against the persistence code while independent Go expectations prevent the golden producer from becoming the test oracle."
```

### Task 2: Normalize current rows, synthetic prompts, and backend tools

**Files:**

- Modify: `internal/parser/grok_test.go`
- Modify: `internal/parser/grok.go:171-452`

**Interfaces:**

- Consumes: Task 1's `parseGrokGolden` and format 1 golden history.

- Produces: `grokChatRowKind(root gjson.Result) string`,
  `grokMessageText(content gjson.Result) string`, and
  `grokBackendToolMessage(root gjson.Result, ordinal int) (ParsedMessage, bool)`.

- [ ] **Step 1: Add focused failing current-format assertions**

Add a test that checks only current row semantics:

```go
func TestGrokProviderGoldenCurrentTranscriptSemantics(t *testing.T) {
    result := parseGrokGolden(t, "current")
    assert.Equal(t, "Review parser compatibility", result.Session.FirstMessage)
    assert.Equal(t, 2, result.Session.UserMessageCount)
    require.Len(t, result.Messages, 6)
    assert.Equal(t, RoleUser, result.Messages[0].Role)
    assert.Equal(t, "Review parser compatibility", result.Messages[0].Content)
    assert.Equal(t, RoleAssistant, result.Messages[1].Role)
    assert.Contains(t, result.Messages[1].Content, "Grok Build persistence")
    require.Len(t, result.Messages[1].ToolCalls, 1)
    assert.Equal(t, "ws_1", result.Messages[1].ToolCalls[0].ToolUseID)
    assert.Equal(t, "web_search", result.Messages[1].ToolCalls[0].ToolName)
    assert.Equal(t, RoleUser, result.Messages[4].Role)
    assert.Equal(t, "also keep interjections", result.Messages[4].Content)
}
```

The six normalized messages are: real user, backend tool context, assistant,
tool-result carrier, interjection, assistant. The tagged project-instructions
row and system row are omitted.

- [ ] **Step 2: Run the current transcript test and verify red**

Run the single test. Expected: FAIL because project instructions are counted and
the backend tool row is absent.

- [ ] **Step 3: Filter tagged synthetic users except interjections**

Before extracting a format 1 user row, apply:

```go
reason := strings.TrimSpace(root.Get("synthetic_reason").Str)
if reason != "" && reason != "interjection" {
    pendingThink = ""
    hasPending = false
    continue
}
```

Use the same clear-without-emission behavior for an intervening user row; Grok
Build drops orphan reasoning instead of surfacing it as a standalone message.

- [ ] **Step 4: Normalize backend tool-call rows**

Add a `backend_tool_call` switch arm. Build an assistant message whose content
is derived from `kind.tool_type` and `kind.action`, whose `ToolUseID` is
`kind.id`, whose tool name is `web_search`, `x_search`, or `code_interpreter`,
and whose `InputJSON` is `kind.action.Raw` when present or `kind.Raw` otherwise.
For the golden web search, emit:

```text
[backend web_search] search: Grok Build persistence
```

Keep pending reasoning buffered across this row so it still attaches to the
following ordinary assistant, matching Grok Build's conversation conversion. Use
this normalization shape so Task 4 can reuse it for legacy `raw_output` entries:

```go
func grokBackendToolMessage(
    root gjson.Result, ordinal int,
) (ParsedMessage, bool) {
    payload := root.Get("kind")
    rowType := strings.TrimSpace(root.Get("type").Str)
    if !payload.Exists() {
        payload = root
    }
    toolName := strings.TrimSpace(payload.Get("tool_type").Str)
    if toolName == "" {
        switch rowType {
        case "web_search_call":
            toolName = "web_search"
        case "custom_tool_call":
            toolName = "x_search"
        case "code_interpreter_call":
            toolName = "code_interpreter"
        }
    }
    if toolName == "" {
        return ParsedMessage{}, false
    }
    id := strings.TrimSpace(payload.Get("id").Str)
    action := payload.Get("action")
    inputJSON := action.Raw
    if inputJSON == "" {
        if input := payload.Get("input"); input.Type == gjson.String {
            inputJSON = input.Str
        } else {
            inputJSON = input.Raw
        }
    }
    content := grokBackendToolSummary(toolName, payload)
    call := ParsedToolCall{
        ToolUseID: id,
        ToolName:  toolName,
        Category:  NormalizeToolCategory(toolName),
        InputJSON: inputJSON,
    }
    return ParsedMessage{
        Ordinal:       ordinal,
        Role:          RoleAssistant,
        Content:       content,
        ContentLength: len(content),
        HasToolUse:    true,
        ToolCalls:     []ParsedToolCall{call},
    }, true
}
```

Use this bounded summary helper; it returns the exact golden text for the
web-search `search` action and never includes encrypted or unbounded binary
payloads:

```go
func grokBackendToolSummary(toolName string, payload gjson.Result) string {
    action := payload.Get("action")
    switch toolName {
    case "web_search":
        switch action.Get("type").Str {
        case "search":
            return "[backend web_search] search: " +
                truncate(strings.TrimSpace(action.Get("query").Str), 300)
        case "open", "open_page":
            return "[backend web_search] open: " +
                truncate(strings.TrimSpace(action.Get("url").Str), 300)
        case "find", "find_in_page":
            return "[backend web_search] find: " +
                truncate(strings.TrimSpace(action.Get("pattern").Str), 300)
        default:
            return "[backend web_search]"
        }
    case "x_search":
        return "[backend x_search] " +
            truncate(strings.TrimSpace(payload.Get("input").Str), 300)
    case "code_interpreter":
        return "[backend code_interpreter] " +
            truncate(strings.TrimSpace(payload.Get("code").Str), 300)
    default:
        return "[backend " + toolName + "]"
    }
}
```

- [ ] **Step 5: Run current and all Grok tests**

Run the focused test, then `-run '^TestGrokProvider'`. Expected: PASS.

- [ ] **Step 6: Commit Task 2**

```bash
git add internal/parser/grok.go internal/parser/grok_test.go
git commit -m "fix(parser): preserve current Grok transcript semantics" \
  -m "Grok tags runtime-injected user items explicitly and persists backend searches as assistant context. Honoring both signals keeps prompt analytics human-focused without discarding server-side tool activity."
```

### Task 3: Parse legacy role rows and mixed histories

**Files:**

- Modify: `internal/parser/grok_test.go`
- Modify: `internal/parser/grok.go:171-317`

**Interfaces:**

- Consumes: `grokChatRowKind` and existing tool normalization.

- Produces: per-line fallback for `role=system|user|assistant|tool` with no
  dependency on summary `chat_format_version`.

- [ ] **Step 1: Add failing legacy and mixed-file tests**

```go
func TestGrokProviderGoldenLegacyTranscript(t *testing.T) {
    result := parseGrokGolden(t, "legacy")
    assert.Equal(t, TranscriptFidelityFull, result.Session.TranscriptFidelity)
    assert.Equal(t, "Review parser compatibility", result.Session.FirstMessage)
    require.NotEmpty(t, result.Messages)
    assert.Equal(t, RoleUser, result.Messages[0].Role)
    assert.Equal(t, "Review parser compatibility", result.Messages[0].Content)
    require.Len(t, result.Messages[1].ToolCalls, 1)
    assert.Equal(t, "call_1", result.Messages[1].ToolCalls[0].ToolUseID)
    assert.Equal(t, "grok-4.5", result.Messages[1].Model)
    require.Len(t, result.Messages[2].ToolResults, 1)
    assert.Equal(t, "call_1", result.Messages[2].ToolResults[0].ToolUseID)
}
```

Also add a temp-dir fixture with a format 0 user row followed by a format 1
assistant row, parse through the provider, and assert both messages in order:

```go
func TestGrokProviderParsesMixedTranscriptFormats(t *testing.T) {
    root := t.TempDir()
    sessionID := "mixed-formats"
    writeGrokFixtureFile(t, grokSummaryPath(root, "cwd-key", sessionID), `{
        "info":{"id":"mixed-formats","cwd":"/workspace/agentsview"},
        "session_summary":"mixed",
        "created_at":"2026-07-18T10:00:00Z",
        "updated_at":"2026-07-18T10:01:00Z"
    }`)
    writeGrokFixtureFile(
        t,
        filepath.Join(root, "cwd-key", sessionID, "chat_history.jsonl"),
        "{\"role\":\"user\",\"content\":\"legacy question\"}\n"+
            "{\"type\":\"assistant\",\"content\":\"current answer\"}\n",
    )
    provider := newGrokTestProvider(t, root)
    sources, err := provider.Discover(context.Background())
    require.NoError(t, err)
    require.Len(t, sources, 1)
    outcome, err := provider.Parse(
        context.Background(),
        ParseRequest{Source: sources[0]},
    )
    require.NoError(t, err)
    require.Len(t, outcome.Results, 1)
    messages := outcome.Results[0].Result.Messages
    require.Len(t, messages, 2)
    assert.Equal(t, "legacy question", messages[0].Content)
    assert.Equal(t, "current answer", messages[1].Content)
}
```

- [ ] **Step 2: Run both tests and verify red**

Expected: both FAIL because format 0 rows lack `type` and are ignored.

- [ ] **Step 3: Add per-line type/role dispatch**

Implement:

```go
func grokChatRowKind(root gjson.Result) string {
    if kind := strings.TrimSpace(root.Get("type").Str); kind != "" {
        return kind
    }
    switch strings.TrimSpace(root.Get("role").Str) {
    case "system":
        return "system"
    case "user":
        return "user"
    case "assistant":
        return "assistant"
    case "tool":
        return "tool_result"
    default:
        return ""
    }
}
```

Switch on this result. Reuse `grokUserContent`, `grokToolCalls`, `model_id`, and
`tool_call_id`; both upstream schemas intentionally share those fields after
dispatch.

- [ ] **Step 4: Run legacy, mixed, and all Grok tests**

Expected: PASS with correct tool-result carriers and no system messages.

- [ ] **Step 5: Commit Task 3**

```bash
git add internal/parser/grok.go internal/parser/grok_test.go
git commit -m "fix(parser): read legacy Grok chat histories" \
  -m "Current Grok Build deliberately falls back between role-based and type-based rows per line, so AgentsView must do the same to retain old and mixed sessions that Grok itself still resumes."
```

### Task 4: Preserve all plaintext reasoning compatibility shapes

**Files:**

- Modify: `internal/parser/grok_test.go`
- Modify: `internal/parser/grok.go:181-317`
- Modify: `internal/parser/grok.go:391-406`

**Interfaces:**

- Consumes: per-line row dispatch from Task 3.

- Produces: `grokReasoningText(root gjson.Result) string`,
  `grokAssistantReasoning(root gjson.Result) string`,
  `grokRawOutputBackendTools(root gjson.Result, seen map[string]struct{}) []ParsedMessage`,
  and a single pending reasoning association rule shared across both
  formats.

- [ ] **Step 1: Add table-driven failing reasoning tests**

For each case, write literal summary/history files and assert the following
assistant's `ThinkingText`:

```go
func TestParseGrokChatHistoryReasoningShapes(t *testing.T) {
    tests := []struct {
        name      string
        history   []string
        wantThink string
        wantCount int
    }{
        {
            name: "standalone content array",
            history: []string{
                `{"type":"reasoning","id":"r1","content":[{"type":"reasoning_text","text":"content thought"}]}`,
                `{"type":"assistant","content":"answer"}`,
            },
            wantThink: "content thought",
            wantCount: 1,
        },
        {
            name: "legacy inline reasoning",
            history: []string{
                `{"type":"assistant","content":"answer","reasoning":{"text":"inline thought"}}`,
            },
            wantThink: "inline thought",
            wantCount: 1,
        },
        {
            name: "legacy raw output reasoning",
            history: []string{
                `{"type":"assistant","content":"answer","raw_output":[{"type":"reasoning","id":"r1","summary":[{"type":"summary_text","text":"raw thought"}]},{"type":"web_search_call","id":"ws_raw","status":"completed","action":{"type":"search","query":"raw query","sources":[]}}]}`,
            },
            wantThink: "raw thought",
            wantCount: 2,
        },
        {
            name: "format zero reasoning content",
            history: []string{
                `{"role":"assistant","content":"answer","reasoning_content":"v0 thought"}`,
            },
            wantThink: "v0 thought",
            wantCount: 1,
        },
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            path := filepath.Join(t.TempDir(), "chat_history.jsonl")
            writeGrokFixtureFile(t, path, strings.Join(tt.history, "\n")+"\n")
            messages, malformed, err := parseGrokChatHistory(path)
            require.NoError(t, err)
            assert.Zero(t, malformed)
            require.Len(t, messages, tt.wantCount)
            assistant := messages[len(messages)-1]
            assert.Equal(t, RoleAssistant, assistant.Role)
            assert.Equal(t, tt.wantThink, assistant.ThinkingText)
            assert.True(t, assistant.HasThinking)
            if tt.name == "legacy raw output reasoning" {
                require.Len(t, messages[0].ToolCalls, 1)
                assert.Equal(t, "ws_raw", messages[0].ToolCalls[0].ToolUseID)
                assert.Contains(t, messages[0].Content, "raw query")
            }
        })
    }
}

func TestParseGrokChatHistoryInlineReasoningOverridesPending(t *testing.T) {
    path := filepath.Join(t.TempDir(), "chat_history.jsonl")
    writeGrokFixtureFile(t, path, strings.Join([]string{
        `{"type":"reasoning","summary":[{"type":"summary_text","text":"pending"}]}`,
        `{"type":"assistant","content":"answer","reasoning":{"text":"inline"}}`,
    }, "\n")+"\n")
    messages, malformed, err := parseGrokChatHistory(path)
    require.NoError(t, err)
    assert.Zero(t, malformed)
    require.Len(t, messages, 1)
    assert.Equal(t, "inline", messages[0].ThinkingText)
}

func TestParseGrokChatHistoryDropsOrphanReasoning(t *testing.T) {
    tests := []struct {
        name    string
        trailing []string
        wantCount int
    }{
        {name: "at eof", wantCount: 0},
        {
            name: "before user",
            trailing: []string{
                `{"type":"user","content":"new question"}`,
                `{"type":"assistant","content":"answer"}`,
            },
            wantCount: 2,
        },
        {
            name: "before tool result",
            trailing: []string{
                `{"type":"tool_result","tool_call_id":"call_1","content":"result"}`,
                `{"type":"assistant","content":"answer"}`,
            },
            wantCount: 2,
        },
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            rows := append([]string{
                `{"type":"reasoning","summary":[{"type":"summary_text","text":"orphan"}]}`,
            }, tt.trailing...)
            path := filepath.Join(t.TempDir(), "chat_history.jsonl")
            writeGrokFixtureFile(t, path, strings.Join(rows, "\n")+"\n")
            messages, malformed, err := parseGrokChatHistory(path)
            require.NoError(t, err)
            assert.Zero(t, malformed)
            require.Len(t, messages, tt.wantCount)
            for _, message := range messages {
                assert.False(t, message.HasThinking)
                assert.NotContains(t, message.Content, "orphan")
            }
        })
    }
}
```

- [ ] **Step 2: Run the reasoning tests and verify red**

Expected: each unsupported shape fails with empty thinking; the orphan test
fails because the current parser flushes a standalone thinking message.

- [ ] **Step 3: Expand plaintext extraction**

Make `grokReasoningText` collect trimmed `text` values from both `summary` and
`content` arrays, falling back to a string `content`. Add
`grokAssistantReasoning` with precedence:

1. `reasoning.text`;
1. plaintext reasoning entries in `raw_output`;
1. `reasoning_content`.

Do not surface `encrypted`, `encrypted_content`, or signatures.

Inspect assistant `raw_output` entries for `web_search_call`,
`custom_tool_call`, and `code_interpreter_call`, normalize them through the
backend-tool helper from Task 2, and append their assistant-visible context
before the ordinary assistant. Track backend IDs already seen as standalone
`backend_tool_call` rows so a legacy `raw_output` copy with the same ID is not
emitted twice, matching Grok Build's load-time deduplication.

```go
func grokRawOutputBackendTools(
    root gjson.Result, seen map[string]struct{},
) []ParsedMessage {
    var messages []ParsedMessage
    rawOutput := root.Get("raw_output")
    if !rawOutput.IsArray() {
        return nil
    }
    rawOutput.ForEach(func(_, item gjson.Result) bool {
        switch item.Get("type").Str {
        case "web_search_call", "custom_tool_call", "code_interpreter_call":
        default:
            return true
        }
        id := strings.TrimSpace(item.Get("id").Str)
        if _, exists := seen[id]; id != "" && exists {
            return true
        }
        message, ok := grokBackendToolMessage(item, 0)
        if !ok {
            return true
        }
        messages = append(messages, message)
        if id != "" {
            seen[id] = struct{}{}
        }
        return true
    })
    return messages
}
```

The assistant switch arm assigns sequential ordinals to these returned messages
before appending the ordinary assistant.

- [ ] **Step 4: Apply Grok's association rules**

On assistant rows, choose inline reasoning when nonblank, otherwise pending
reasoning. Clear pending reasoning after every assistant. Clear it without
emitting on user/tool rows and at EOF. Keep it across backend tool-call rows.

- [ ] **Step 5: Run reasoning and all Grok tests**

Expected: PASS.

- [ ] **Step 6: Commit Task 4**

```bash
git add internal/parser/grok.go internal/parser/grok_test.go
git commit -m "fix(parser): recover Grok reasoning variants" \
  -m "Grok's loader preserves multiple historical plaintext reasoning placements and associates sibling reasoning only with the next assistant. Mirroring that rule prevents both silent loss and orphaned thinking rows."
```

### Task 5: Normalize fork, worktree, and signal metadata

**Files:**

- Modify: `internal/parser/grok_test.go:81-145`
- Modify: `internal/parser/grok.go:15-28`
- Modify: `internal/parser/grok.go:127-168`
- Modify: `internal/parser/grok.go:471-520`
- Modify: `internal/parser/grok.go:522-592`

**Interfaces:**

- Consumes: golden summary/signals from Task 1.

- Produces: `grokSummaryFields.ParentSessionID` and
  `grokSummaryFields.SourceWorkspaceDir`; populated `ParsedSession` fork
  fields; project grouping separate from actual `Cwd`; no false
  current-as-peak metric.

- [ ] **Step 1: Add failing golden metadata assertions**

```go
func TestGrokProviderGoldenCurrentMetadata(t *testing.T) {
    result := parseGrokGolden(t, "current")
    session := result.Session
    assert.Equal(t, "/workspace/grok-worktrees/parser-audit", session.Cwd)
    assert.Equal(t, "agentsview", session.Project)
    assert.Equal(
        t,
        "grok:019f5000-0000-7000-8000-000000000000",
        session.ParentSessionID,
    )
    assert.Equal(t, RelFork, session.RelationshipType)
    assert.False(t, session.HasPeakContextTokens)
    assert.Zero(t, session.PeakContextTokens)
}
```

Update `TestGrokProviderCurrentBuildSummarySchema` to assert that
`contextTokensUsed` does not set peak context. Keep the legacy explicit
`tokenUsage.peakContextTokens` assertion in `TestGrokProviderSummarySource`.

- [ ] **Step 2: Run metadata tests and verify red**

Expected: FAIL because project derives from worktree CWD, fork fields are empty,
and current context is reported as a peak.

- [ ] **Step 3: Decode and apply upstream summary metadata**

Add `ParentSessionID` and `SourceWorkspaceDir` to `grokSummaryFields`, decode
`parent_session_id` and `source_workspace_dir`, and populate:

```go
parentSessionID := strings.TrimSpace(summary.ParentSessionID)
relationshipType := RelNone
if parentSessionID != "" {
    parentSessionID = "grok:" + parentSessionID
    relationshipType = RelFork
}
```

Assign both values in `ParsedSession`.

- [ ] **Step 4: Separate project grouping from actual CWD**

Keep `cwd` derived from `summary.Cwd` then `summary.GitRootDir`. Derive project
first from `summary.SourceWorkspaceDir` when nonblank, then from `cwd`, then the
worktree label and decoded project hint. Preserve existing branch-aware project
normalization.

- [ ] **Step 5: Stop treating current context as peak context**

Remove `contextTokens` and `contextTokensUsed` from the `PeakContextTokens`
fallback list. Retain only explicit legacy `tokenUsage.peakContextTokens`,
`usage.peakContextTokens`, and `peakContextTokens` paths.

- [ ] **Step 6: Run metadata and all Grok tests**

Expected: PASS, including the legacy explicit peak fixture.

- [ ] **Step 7: Commit Task 5**

```bash
git add internal/parser/grok.go internal/parser/grok_test.go
git commit -m "fix(parser): align Grok session metadata" \
  -m "The public summary type distinguishes worktree grouping from execution CWD, records fork ancestry, and exposes current context rather than a peak. Using those meanings avoids broken session trees and overstated token aggregates."
```

### Task 6: Verify the complete parser correction

**Files:**

- Verify only; modify files only if a command exposes a real defect.

**Interfaces:**

- Consumes: Tasks 1-5.

- Produces: clean formatting, vet, parser tests, and short repository suite.

- [ ] **Step 1: Run formatting**

```bash
go fmt ./...
git diff --check
```

Expected: both commands exit 0. If formatting changes tracked Go files, review
and include them in the final focused commit rather than creating an empty
commit.

- [ ] **Step 2: Run focused parser verification**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/parser \
  -run '^TestGrokProvider' -count=1
```

Expected: PASS.

- [ ] **Step 3: Run all parser tests**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/parser -count=1
```

Expected: PASS.

- [ ] **Step 4: Run vet and the short repository suite**

```bash
go vet ./...
make test-short
```

Expected: PASS. Investigate any failure before changing code or expectations.

- [ ] **Step 5: Audit the final diff and fixture hygiene**

```bash
git status --short
git diff HEAD --stat
git diff HEAD
rg -n '/Users/|/home/|@|localhost' internal/parser/testdata/grok-build
```

Expected: only requested Grok parser/tests/goldens and plan/design commits are
present; no private path or identity appears in golden data.

- [ ] **Step 6: Commit verification-only formatting if needed**

If Step 1 changed tracked files after the last task commit, load the mandatory
commit skill and commit those relevant formatting changes. If the tree is clean,
do not create an empty commit.
