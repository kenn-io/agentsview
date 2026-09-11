# Plan 001: Accept `model/selection` event in DeepSeek Harness parser

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md` — unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat 2107855f..HEAD -- internal/parser/deepseek_harness_format.go internal/parser/deepseek_harness_test.go`
> If any in-scope file changed since this plan was written, compare the
> "Current state" excerpts against the live code before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P1
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: bug
- **Planned at**: commit `2107855f`, 2026-09-10

## Why this matters

The running agentsview instance logs `sync error: unsupported required event type "model/selection"` every second, polluting logs and failing to sync DeepSeek Harness sessions. The `model/selection` event is a newer DeepSeek Harness addition that records which model was selected for a request (provider, model name, reasoning effort). It is informational metadata — it does not affect transcript reconstruction or surface ordering — but it lacks the `ignorable: true` flag, so the parser correctly rejects it as a required unknown event per the upstream contract.

## Current state

The relevant files:

- `internal/parser/deepseek_harness_format.go` — event parsing and validation; contains `deepSeekHarnessKnownEvents` (line 75) and the unknown-event guard (line 657)
- `internal/parser/deepseek_harness_test.go` — parser tests; contains the "unknown required" test case (line 757) as a structural pattern

**`deepSeekHarnessKnownEvents` at `internal/parser/deepseek_harness_format.go:75-94`:**

```go
var deepSeekHarnessKnownEvents = map[string]struct{}{
	"agent-preset/selected": {}, "agent/inbox/spliced": {},
	"approval/asked": {}, "approval/decided": {}, "approval/policy": {},
	"assistant/chunk": {}, "assistant/message": {},
	"command/done": {}, "command/run": {},
	"compaction/end": {}, "compaction/prune": {}, "compaction/start": {},
	"compaction/summary": {}, "feedback/record": {}, "goal/change": {},
	"hook/invoked": {}, "hook/result": {}, "llm/retry": {},
	"llm/retry-started": {}, "permission/preset": {}, "plan/mode": {},
	"request/context": {}, "request/header": {}, "sandbox/mode": {},
	"schedule/change": {}, "session/end-seed": {}, "session/title": {},
	"session/title-llm-request": {}, "step/end": {}, "step/start": {},
	"subagent/descriptor": {}, "todo/write": {},
	"tool-workflow/agent-end": {}, "tool-workflow/agent-start": {},
	"tool-workflow/run-end": {}, "tool-workflow/run-start": {},
	"tool/call": {}, "tool/code-dispatch": {},
	"tool/code-dispatch-start": {}, "tool/result": {},
	"turn/end": {}, "turn/start": {}, "user/message": {},
	"web/deepseek-search-llm-request": {},
}
```

**Unknown-event guard at `internal/parser/deepseek_harness_format.go:657-661`:**

```go
if _, known := deepSeekHarnessKnownEvents[typeName]; !known && !ignorable {
    return deepSeekHarnessEvent{}, deepSeekHarnessUnsupportedError{message: fmt.Sprintf(
        "unsupported required event type %q", typeName,
    )}
}
```

**Observed event shape (from a live session file):**

```json
{"type":"model/selection","seq":12543,"time":1788812474660,"data":{"provider":"deepseek-official","model":"deepseek-v4-flash","reasoningEffort":"high"}}
```

This event is not a surface event (no `surfaceOp`), so `deepSeekHarnessSurfaceEvents` does not need updating.

**Convention**: the repo uses conventional commits with a scope prefix — e.g. `fix(parser): ...`, `feat(parser): ...`. See recent `git log` for examples.

## Commands you will need

| Purpose   | Command                                      | Expected on success          |
|-----------|----------------------------------------------|------------------------------|
| Tests     | `go test ./internal/parser/ -run DeepSeekHarness -count=1` | all pass                     |
| Vet       | `go vet ./internal/parser/`                  | exit 0, no output            |
| Fmt       | `go fmt ./internal/parser/deepseek_harness_format.go` | no diff (already formatted)  |

## Scope

**In scope**:
- `internal/parser/deepseek_harness_format.go` — add `model/selection` to `deepSeekHarnessKnownEvents`
- `internal/parser/deepseek_harness_test.go` — add a test case for `model/selection` acceptance

**Out of scope**:
- `docs/internal/session-format-sources.md` — the pinned upstream commit (`47f9438`) does not include `model/selection`; the doc update belongs in a follow-up once a release is tagged
- No changes to `deepSeekHarnessSurfaceEvents` — `model/selection` is not a surface event
- No model/selection data extraction for usage attribution — that is a separate feature

## Git workflow

- Branch: `fix/deepseek-model-selection-event` (or follow the repo's branch convention)
- Single commit: `fix(parser): accept model/selection event in DeepSeek Harness parser`
- Do NOT push or open a PR unless instructed

## Steps

### Step 1: Add `model/selection` to `deepSeekHarnessKnownEvents`

In `internal/parser/deepseek_harness_format.go`, add `"model/selection": {}` to the `deepSeekHarnessKnownEvents` map. Insert it in alphabetical position within the existing list — between `"llm/retry-started"` and `"permission/preset"` on line 83. The resulting block around that area should look like:

```go
	"llm/retry-started": {}, "model/selection": {}, "permission/preset": {}, "plan/mode": {},
```

**Verify**: `go vet ./internal/parser/` → exit 0, no output

### Step 2: Add a test for `model/selection` acceptance

In `internal/parser/deepseek_harness_test.go`, add a new subtest inside the existing `TestDeepSeekHarnessScanEvents` function (or the closest test function that contains the "unknown required" / "unknown ignorable" subtests). Model it after the "unknown ignorable" test at line 768, but use `model/selection` without the `ignorable` flag:

```go
t.Run("model/selection accepted", func(t *testing.T) {
    records := []any{
        deepSeekHarnessFixtureHeader("model-select", deepSeekHarnessFixtureCwd, nil),
        map[string]any{
            "type": "model/selection", "seq": 0, "time": 1700000000001,
            "data": map[string]any{"provider": "deepseek-official", "model": "deepseek-v4-flash", "reasoningEffort": "high"},
        },
        deepSeekHarnessFixtureEvent(1, "turn/start", map[string]any{"turn": 1}, nil),
        deepSeekHarnessFixtureEvent(2, "turn/end", deepSeekHarnessTurnEnd(1, "completed"), nil),
    }
    path := writeDeepSeekHarnessFixture(t, t.TempDir(), "model-select", deepSeekHarnessFixtureCwd, "plain", records)
    result, err := parseDeepSeekHarnessSession(t.Context(), path, "")
    require.NoError(t, err)
    assert.Len(t, result.Messages, 0)
})
```

**Verify**: `go test ./internal/parser/ -run DeepSeekHarness -count=1` → all pass, including the new "model/selection accepted" subtest

### Step 3: Run full parser tests and vet

Run the full DeepSeek Harness test suite plus vet to confirm nothing regressed.

**Verify**:
- `go test ./internal/parser/ -run DeepSeekHarness -count=1` → all pass
- `go vet ./internal/parser/` → exit 0

## Test plan

- New test: "model/selection accepted" — verifies that a session containing a `model/selection` event (without `ignorable: true`) parses successfully without error
- Existing tests: confirm no regressions in the full `DeepSeekHarness` test suite

## Done criteria

- [ ] `go test ./internal/parser/ -run DeepSeekHarness -count=1` exits 0
- [ ] `go vet ./internal/parser/` exits 0
- [ ] New "model/selection accepted" test exists and passes
- [ ] No files outside the in-scope list are modified (`git status`)

## STOP conditions

- The code at `internal/parser/deepseek_harness_format.go:75-94` doesn't match the excerpt in "Current state" (the known events list has changed)
- The "unknown required" / "unknown ignorable" tests at `internal/parser/deepseek_harness_test.go:757-777` no longer exist at those line numbers
- The fix requires touching a file outside the in-scope list

## Maintenance notes

- The pinned upstream commit in `docs/internal/session-format-sources.md` (`47f9438`) predates `model/selection`. Update the pinned commit and evidence entry once a DeepSeek Harness release containing this event is tagged.
- If DeepSeek Harness emits other new required events in the future, this same pattern applies: add them to `deepSeekHarnessKnownEvents` and verify with a test.
- The `model/selection` event carries `provider`, `model`, and `reasoningEffort`. A future enhancement could extract this data for usage attribution when `request/context` is absent.
