# Plan 002: Treat malformed sourceEventSeqs as non-fatal in DeepSeek Harness parser

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md` — unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat 19e741d1..HEAD -- internal/parser/deepseek_harness_format.go internal/parser/deepseek_harness_test.go`
> If any in-scope file changed since this plan was written, compare the
> "Current state" excerpts against the live code before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P1
- **Effort**: S
- **Risk**: MED
- **Depends on**: plans/001-add-model-selection-to-known-events.md
- **Category**: bug
- **Planned at**: commit `19e741d1`, 2026-09-10

## Why this matters

After the `model/selection` fix (plan 001), the parser now reaches deeper into session logs and encounters a pre-existing DeepSeek Harness writer bug: `sourceEventSeqs` is written as a nested array `[[138, 144]]` instead of flat `[138, 144]`. The strict integer-array validation rejects this, the error is stored as an issue, and when a subsequent `turn/end` event is encountered the entire session scan fails with "corrupt committed DeepSeek Harness log". This prevents syncing sessions that are otherwise fully usable — the malformed `sourceEventSeqs` is purely informational metadata and skipping it is safe.

## Current state

The relevant files:

- `internal/parser/deepseek_harness_format.go` — event parsing; contains `deepSeekHarnessSafeIntArray` (line 893) and `sourceEventSeqs` validation (line 676–679)
- `internal/parser/deepseek_harness_test.go` — parser tests

**`sourceEventSeqs` validation at `internal/parser/deepseek_harness_format.go:676–679`:**

```go
if hasSourceEventSeqs {
    if _, err := deepSeekHarnessSafeIntArray(sourceEventSeqs, true); err != nil {
        return deepSeekHarnessEvent{}, errors.New("event sourceEventSeqs is invalid")
    }
}
```

**`deepSeekHarnessSafeIntArray` at `internal/parser/deepseek_harness_format.go:893–909`:**

```go
func deepSeekHarnessSafeIntArray(raw jsontext.Value, nonNegative bool) ([]int64, error) {
    var values []jsontext.Value
    if err := json.Unmarshal(raw, &values); err != nil {
        return nil, err
    }
    out := make([]int64, 0, len(values))
    for _, rawValue := range values {
        value, err := deepSeekHarnessRequiredSafeInt(
            map[string]jsontext.Value{"value": rawValue}, "value", nonNegative,
        )
        if err != nil {
            return nil, err
        }
        out = append(out, value)
    }
    return out, nil
}
```

**Malformed data example (session `baf3c153-...`, row 145):**

```json
{"type":"assistant/message","seq":145,"time":1788812762815,"data":{...},"sourceEventSeqs":[[138,144]],"surfaceOp":"append"}
```

The `sourceEventSeqs` value is `[[138, 144]]` — each "integer" is wrapped in an extra array layer. The expected shape is `[138, 144]`.

**Error flow:** `parseDeepSeekHarnessEvent` returns error → stored in `issue` at scan line 245 → next `turn/end` triggers fatal "corrupt committed DeepSeek Harness log" at scan line 254–258.

**Convention**: the repo uses conventional commits with a scope prefix — e.g. `fix(parser): ...`. See recent `git log` for examples.

## Commands you will need

| Purpose   | Command                                      | Expected on success          |
|-----------|----------------------------------------------|------------------------------|
| Tests     | `go test ./internal/parser/ -run DeepSeekHarness -count=1` | all pass                     |
| Vet       | `go vet ./internal/parser/`                  | exit 0, no output            |

## Scope

**In scope**:
- `internal/parser/deepseek_harness_format.go` — add `deepSeekHarnessSafeSourceEventSeqs` helper, update validation at line 676–679
- `internal/parser/deepseek_harness_test.go` — add tests for nested and valid `sourceEventSeqs`

**Out of scope**:
- No changes to `deepSeekHarnessSafeIntArray` — it remains strict for other callers
- No changes to the scan loop or error flow in `scanDeepSeekHarnessLog`
- No changes to `docs/internal/session-format-sources.md`

## Git workflow

- Branch: `fix/deepseek-sourceEventSeqs-lenient` (or follow repo convention)
- Single commit: `fix(parser): lenient sourceEventSeqs validation for nested arrays`
- Do NOT push or open a PR unless instructed

## Steps

### Step 1: Add `deepSeekHarnessSafeSourceEventSeqs` helper

In `internal/parser/deepseek_harness_format.go`, add a new function after `deepSeekHarnessSafeIntArray` (after line 909). This function attempts to parse `sourceEventSeqs` as a flat integer array; if that fails, it attempts to flatten a nested array; if that also fails, it returns nil (skip the field):

```go
func deepSeekHarnessSafeSourceEventSeqs(raw jsontext.Value) []int64 {
	flat, err := deepSeekHarnessSafeIntArray(raw, true)
	if err == nil {
		return flat
	}
	var nested [][]int64
	if err := json.Unmarshal(raw, &nested); err != nil {
		return nil
	}
	var out []int64
	for _, inner := range nested {
		out = append(out, inner...)
	}
	return out
}
```

**Verify**: `go vet ./internal/parser/` → exit 0, no output

### Step 2: Update `sourceEventSeqs` validation to use lenient helper

In `internal/parser/deepseek_harness_format.go`, replace lines 676–679:

```go
if hasSourceEventSeqs {
    if _, err := deepSeekHarnessSafeIntArray(sourceEventSeqs, true); err != nil {
        return deepSeekHarnessEvent{}, errors.New("event sourceEventSeqs is invalid")
    }
}
```

With:

```go
if hasSourceEventSeqs {
    _ = deepSeekHarnessSafeSourceEventSeqs(sourceEventSeqs)
}
```

The result is intentionally discarded — `sourceEventSeqs` is only used for validation, not for downstream processing. The call validates the field can be interpreted as integers (flat or flattened) and silently accepts malformed data.

**Verify**: `go vet ./internal/parser/` → exit 0, no output

### Step 3: Add tests for sourceEventSeqs validation

In `internal/parser/deepseek_harness_test.go`, add three subtests inside `TestDeepSeekHarnessFormatErrorsAndCrashTails` (near the "model/selection accepted" test added in plan 001):

**3a. Valid flat sourceEventSeqs accepted:**

```go
t.Run("sourceEventSeqs flat accepted", func(t *testing.T) {
    records := []any{
        deepSeekHarnessFixtureHeader("src-seqs-flat", deepSeekHarnessFixtureCwd, nil),
        map[string]any{
            "type": "assistant/message", "seq": 0, "time": 1700000000001,
            "data": map[string]any{
                "turn": 1, "step": 1,
                "message": map[string]any{"role": "assistant", "content": []any{}},
            },
            "sourceEventSeqs": []any{float64(1), float64(2), float64(3)},
            "surfaceOp": "append",
        },
        deepSeekHarnessFixtureEvent(1, "turn/end", deepSeekHarnessTurnEnd(1, "completed"), nil),
    }
    path := writeDeepSeekHarnessFixture(t, t.TempDir(), "src-seqs-flat", deepSeekHarnessFixtureCwd, "plain", records)
    result, err := parseDeepSeekHarnessSession(t.Context(), path, "")
    require.NoError(t, err)
    assert.NotEmpty(t, result.Messages)
})
```

**3b. Nested sourceEventSeqs accepted (the bug this plan fixes):**

```go
t.Run("sourceEventSeqs nested accepted", func(t *testing.T) {
    records := []any{
        deepSeekHarnessFixtureHeader("src-seqs-nested", deepSeekHarnessFixtureCwd, nil),
        map[string]any{
            "type": "assistant/message", "seq": 0, "time": 1700000000001,
            "data": map[string]any{
                "turn": 1, "step": 1,
                "message": map[string]any{"role": "assistant", "content": []any{}},
            },
            "sourceEventSeqs": []any{[]any{float64(1), float64(2)}},
            "surfaceOp": "append",
        },
        deepSeekHarnessFixtureEvent(1, "turn/end", deepSeekHarnessTurnEnd(1, "completed"), nil),
    }
    path := writeDeepSeekHarnessFixture(t, t.TempDir(), "src-seqs-nested", deepSeekHarnessFixtureCwd, "plain", records)
    result, err := parseDeepSeekHarnessSession(t.Context(), path, "")
    require.NoError(t, err)
    assert.NotEmpty(t, result.Messages)
})
```

**3c. Completely invalid sourceEventSeqs accepted (silently skipped):**

```go
t.Run("sourceEventSeqs invalid accepted", func(t *testing.T) {
    records := []any{
        deepSeekHarnessFixtureHeader("src-seqs-bad", deepSeekHarnessFixtureCwd, nil),
        map[string]any{
            "type": "assistant/message", "seq": 0, "time": 1700000000001,
            "data": map[string]any{
                "turn": 1, "step": 1,
                "message": map[string]any{"role": "assistant", "content": []any{}},
            },
            "sourceEventSeqs": []any{"not-a-number"},
            "surfaceOp": "append",
        },
        deepSeekHarnessFixtureEvent(1, "turn/end", deepSeekHarnessTurnEnd(1, "completed"), nil),
    }
    path := writeDeepSeekHarnessFixture(t, t.TempDir(), "src-seqs-bad", deepSeekHarnessFixtureCwd, "plain", records)
    result, err := parseDeepSeekHarnessSession(t.Context(), path, "")
    require.NoError(t, err)
    assert.NotEmpty(t, result.Messages)
})
```

**Verify**: `go test ./internal/parser/ -run DeepSeekHarness -count=1` → all pass, including the three new subtests

### Step 4: Run full parser tests and vet

**Verify**:
- `go test ./internal/parser/ -run DeepSeekHarness -count=1` → all pass
- `go vet ./internal/parser/` → exit 0

## Test plan

- New tests: "sourceEventSeqs flat accepted", "sourceEventSeqs nested accepted", "sourceEventSeqs invalid accepted" — covering the happy path, the nested-array writer bug, and completely invalid data
- Existing tests: confirm no regressions in the full `DeepSeekHarness` test suite

## Done criteria

- [ ] `go test ./internal/parser/ -run DeepSeekHarness -count=1` exits 0
- [ ] `go vet ./internal/parser/` exits 0
- [ ] Three new `sourceEventSeqs` tests exist and pass
- [ ] No files outside the in-scope list are modified (`git status`)

## STOP conditions

- The code at `internal/parser/deepseek_harness_format.go:676–679` doesn't match the excerpt in "Current state"
- The `deepSeekHarnessSafeIntArray` function at line 893–909 has changed signature or behavior
- The fix requires touching a file outside the in-scope list

## Maintenance notes

- `deepSeekHarnessSafeIntArray` remains strict — other callers (if any) are unaffected
- The `sourceEventSeqs` field is informational metadata used for event lineage tracking; the parser does not currently use it for transcript reconstruction, so skipping malformed values is safe
- The nested-array bug is in the DeepSeek Harness writer, not in agentsview; a future upstream fix would make the lenient path dead code but harmless
- Consider reporting the upstream bug to DeepSeek Harness if the repo accepts issues
