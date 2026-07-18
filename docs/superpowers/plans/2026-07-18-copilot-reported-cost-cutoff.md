# Copilot Reported Cost Cutoff Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Preserve `main`'s schemas and existing AI Credits surfaces while
using Copilot CLI `totalNanoAiu` as authoritative USD cost only for sessions
starting on or after June 1, 2026.

**Architecture:** Gate reported-cost attachment in the Copilot parser using the
builder's first valid session timestamp. Restore every existing UI, API, CLI,
and credit-derivation surface removed by this branch, while retaining the
generic cross-backend handling required for `cost_usd` and `cost_source`.

**Tech Stack:** Go 1.24, SQLite, PostgreSQL, DuckDB, Svelte 5, TypeScript,
Paraglide JS, testify.

## Global Constraints

- Keep database and response schemas equal to `main` wherever possible.
- Do not add a Copilot-specific cost or credit column, schema migration,
  compatibility probe, dual-read path, or usage-daily schema-version change.
- Preserve the existing Copilot AI Credits card, `copilotAICredits` totals,
  session `ai_credits`, CLI AI Credits line, translations, and generated type.
- Interpret `totalNanoAiu` as USD only for sessions whose first valid timestamp
  is on or after `2026-06-01T00:00:00Z`.
- Use the session start timestamp, not the shutdown timestamp.
- Preserve SQLite, PostgreSQL, and DuckDB behavior parity.
- Retain data version 69 so already-indexed eligible sessions are reparsed.
- Do not push, rebase, amend, squash, or change branches.

---

### Task 1: Add the session-start pricing cutoff

**Files:**

- Modify: `internal/parser/copilot.go`
- Test: `internal/parser/copilot_test.go`

**Interfaces:**

- Consumes: `copilotSessionBuilder.startedAt time.Time`, populated from the
  first valid event timestamp.
- Produces: usage events with `CostUSD`, `CostStatus`, and `CostSource` only for
  sessions eligible for usage-based AI Credits billing.

- [ ] **Step 1: Add a failing cutoff regression test**

Add a table-driven test beside the shutdown reported-cost tests:

```go
func TestParseCopilotSession_ReportedCostPricingCutoff(t *testing.T) {
	tests := []struct {
		name         string
		startedAt    string
		shutdownAt   string
		wantReported bool
	}{
		{
			name:         "before cutoff",
			startedAt:    "2026-05-31T23:59:59Z",
			shutdownAt:   "2026-06-01T00:01:00Z",
			wantReported: false,
		},
		{
			name:         "exactly at cutoff",
			startedAt:    "2026-06-01T00:00:00Z",
			shutdownAt:   "2026-06-01T00:01:00Z",
			wantReported: true,
		},
		{
			name:         "after cutoff",
			startedAt:    "2026-06-01T00:00:01Z",
			shutdownAt:   "2026-06-01T00:01:00Z",
			wantReported: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeCopilotJSONL(t,
				fmt.Sprintf(`{"type":"session.start","data":{"sessionId":"cutoff"},"timestamp":%q}`, tt.startedAt),
				fmt.Sprintf(`{"type":"user.message","data":{"content":"Hello"},"timestamp":%q}`, tt.startedAt),
				fmt.Sprintf(`{"type":"session.shutdown","data":{"totalNanoAiu":2500000000,"modelMetrics":{"claude-sonnet-4.6":{"usage":{"inputTokens":100,"outputTokens":50}}}},"timestamp":%q}`, tt.shutdownAt),
			)
			_, _, usage := parseCopilotFull(t, path, "m")
			require.Len(t, usage, 1)
			if tt.wantReported {
				require.NotNil(t, usage[0].CostUSD)
				assert.InDelta(t, 0.025, *usage[0].CostUSD, 1e-12)
				assert.Equal(t, "copilot-reported", usage[0].CostSource)
			} else {
				assert.Nil(t, usage[0].CostUSD)
				assert.Empty(t, usage[0].CostSource)
			}
		})
	}
}
```

Add `fmt` to the test imports.

- [ ] **Step 2: Run the regression test and confirm the old-session case fails**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/parser -run TestParseCopilotSession_ReportedCostPricingCutoff -count=1
```

Expected: FAIL because the pre-cutoff session currently receives a reported
cost.

- [ ] **Step 3: Implement the UTC session-start gate**

Add the fixed cutoff near the Copilot event constants:

```go
var copilotUsageBasedPricingStartedAt = time.Date(
	2026, time.June, 1, 0, 0, 0, 0, time.UTC,
)
```

At the start of `handleShutdown`, calculate eligibility and clear prior
cumulative reported cost only for eligible sessions:

```go
useReportedCost := !b.startedAt.IsZero() &&
	!b.startedAt.Before(copilotUsageBasedPricingStartedAt)
if useReportedCost {
	for i := range b.usageEvents {
		if b.usageEvents[i].CostSource == copilotReportedCostSource {
			b.usageEvents[i].CostUSD = nil
			b.usageEvents[i].CostStatus = ""
			b.usageEvents[i].CostSource = ""
		}
	}
}
```

Gate the existing `totalNanoAiu` conversion:

```go
totalNanoAiu := data.Get("totalNanoAiu")
if useReportedCost && totalNanoAiu.Exists() {
	if len(events) == 0 {
		events = append(events, ParsedUsageEvent{
			Source:     "shutdown",
			Model:      "copilot",
			OccurredAt: occurredAt,
		})
	}
	costUSD := float64(totalNanoAiu.Int()) / 1e11
	events[0].CostUSD = &costUSD
	events[0].CostStatus = "exact"
	events[0].CostSource = copilotReportedCostSource
}
```

Update the existing reported-cost parser fixtures from January 2025 to dates
after the cutoff. Leave the new before-cutoff case unchanged.

- [ ] **Step 4: Run all Copilot parser tests**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/parser -run Copilot -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit the parser cutoff**

```bash
git add internal/parser/copilot.go internal/parser/copilot_test.go
git commit -m "fix(copilot): gate reported cost by pricing era"
```

The commit body must explain that GitHub switched new sessions to AI Credits
on June 1, 2026 and that pre-cutoff `totalNanoAiu` values are not USD billing.
Do not add attribution or validation footers.

### Task 2: Restore `main`'s existing credit and response surfaces

**Files:**

- Modify: `cmd/agentsview/session_usage.go`
- Modify: `cmd/agentsview/session_usage_test.go`
- Modify: `internal/db/usage.go`
- Modify: `internal/db/usage_test.go`
- Modify: `internal/postgres/usage.go`
- Modify: `internal/postgres/usage_unit_test.go`
- Modify: `internal/duckdb/analytics_usage.go`
- Modify: `internal/server/huma_routes_sessions.go`
- Modify: `frontend/src/lib/api/generated/models/SessionUsageResponse.ts`
- Modify: `frontend/src/lib/components/usage/UsageSummaryCards.svelte`
- Modify: `frontend/messages/en.json`
- Modify: `frontend/messages/fr.json`
- Modify: `frontend/messages/ko.json`
- Modify: `frontend/messages/zh-CN.json`
- Modify: `frontend/messages/zh-TW.json`
- Modify: `docs/session-api.md`
- Modify: `docs/token-usage.md`

**Interfaces:**

- Consumes: the selected session `CostUSD`, whether catalog-computed or
  `copilot-reported`.
- Produces: the same `ai_credits` and `copilotAICredits` surfaces as `main`,
  derived from the selected USD cost at 100 credits per dollar.

- [ ] **Step 1: Restore failing contract assertions before implementation**

Restore the existing `main` assertions that require:

```go
assert.InDelta(t, 1.75, u.AICredits, 1e-9, "AICredits")
assert.Contains(t, s, "AI Credits")
assert.Contains(t, s, "1000")
```

Add an assertion to each SQLite, PostgreSQL, and DuckDB reported-cost test that
the session-level credits derive from the selected authoritative USD total:

```go
assert.InDelta(t, reportedCost/0.01, usage.AICredits, 1e-9)
```

- [ ] **Step 2: Run the focused tests and confirm the restored assertions fail**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/duckdb ./cmd/agentsview -run 'AICredits|CopilotReported' -count=1
```

Expected: FAIL because the session field and render line are currently absent.

- [ ] **Step 3: Restore session credit derivation after cost selection**

Restore the field in `db.SessionUsage`:

```go
AICredits float64 `json:"ai_credits,omitempty"`
```

After the authoritative/catalog cost selection in all three backends, derive
credits from the final selected value:

```go
if out.HasCost {
	out.AICredits = AICreditsFromCost(sess.Agent, out.CostUSD)
}
```

Use `db.AICreditsFromCost` in PostgreSQL and DuckDB. Do not add an
`ai_credits` storage column.

- [ ] **Step 4: Restore the API, CLI, frontend, and localization surfaces**

Restore the Huma response field and mapping:

```go
AICredits float64 `json:"ai_credits,omitempty"`
```

```go
AICredits: usage.AICredits,
```

Restore the CLI output:

```go
if out.AICredits > 0 {
	fmt.Fprintf(w, "%s %.0f\n", label("AI Credits"), out.AICredits)
}
```

Restore the generated TypeScript field:

```ts
ai_credits?: number;
```

Restore `usage_summary_copilot_ai_credits` in every locale:

```json
// en.json
"usage_summary_copilot_ai_credits": "Copilot AI Credits"
// fr.json
"usage_summary_copilot_ai_credits": "Crédits Copilot AI"
// ko.json
"usage_summary_copilot_ai_credits": "Copilot AI 크레딧"
// zh-CN.json and zh-TW.json
"usage_summary_copilot_ai_credits": "Copilot AI Credits"
```

Restore `fmtCredits` and the conditional Copilot AI Credits card in
`UsageSummaryCards.svelte` exactly as on `origin/main`.

- [ ] **Step 5: Align documentation with the unchanged contract**

Keep the reported-billing section but add the explicit cutoff:

```markdown
Copilot sessions starting on or after June 1, 2026 can report an authoritative
`totalNanoAiu` billing total. Older sessions remain catalog-priced because they
were created under the premium-request pricing model.
```

Restore the dashboard-card and CLI descriptions from `origin/main`. Remove the
claim that usage-daily v2 removes or changes `copilotAICredits`; the schema and
field set remain unchanged.

- [ ] **Step 6: Regenerate localization and API artifacts**

From `frontend/`, run:

```bash
npm run i18n:compile
npm run generate:api
```

Only retain generated changes that restore the `main` contract.

- [ ] **Step 7: Run focused contract tests**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/postgres ./internal/duckdb ./internal/server ./cmd/agentsview -run 'AICredits|CopilotReported|SessionUsage' -count=1
```

Run from `frontend/`:

```bash
npm run check
```

Expected: all commands PASS with zero frontend errors.

- [ ] **Step 8: Commit the restored surfaces**

Stage only the files listed in this task and commit:

```bash
git add cmd/agentsview/session_usage.go cmd/agentsview/session_usage_test.go docs/session-api.md docs/token-usage.md frontend/messages/en.json frontend/messages/fr.json frontend/messages/ko.json frontend/messages/zh-CN.json frontend/messages/zh-TW.json frontend/src/lib/api/generated/models/SessionUsageResponse.ts frontend/src/lib/components/usage/UsageSummaryCards.svelte internal/db/usage.go internal/db/usage_test.go internal/duckdb/analytics_usage.go internal/postgres/usage.go internal/postgres/usage_unit_test.go internal/server/huma_routes_sessions.go
git commit -m "fix(usage): preserve existing Copilot credit surfaces"
```

The commit body must explain that this PR changes Copilot cost selection, not
the existing schema or presentation contract. Do not add attribution or
validation footers.

### Task 3: Remove unrelated resync and draft-schema work

**Files:**

- Modify: `internal/db/orphaned.go`
- Modify: `internal/db/orphaned_test.go`
- Modify: `internal/sync/engine_integration_test.go`
- Modify: `internal/duckdb/schema.go`
- Verify: `internal/db/db.go`
- Verify: `internal/db/db_test.go`

**Interfaces:**

- Consumes: the normal parser data-version resync path.
- Produces: no schema or orphan-copy behavior beyond `main`; data version 69
  remains the only resync trigger.

- [ ] **Step 1: Remove branch-only orphan usage-event copying**

Delete the `copyUsageEventsForIDs` call and function from
`internal/db/orphaned.go`. The existing message-copy statement must be followed
directly by the tool-call copy block:

```go
); err != nil {
	return fmt.Errorf("copying messages: %w", err)
}

// Copy tool_calls. Map old message_id to new message_id via the
// (session_id, ordinal) natural key.
```

Delete `TestCopyOrphanedDataPreservesUsageEvents` and the added orphan usage
seeding/assertions in `TestResyncAllPreservesTrashedSessionData`.

- [ ] **Step 2: Restore incidental schema-file formatting**

Restore the blank line in `internal/duckdb/schema.go` so the file has no diff
from `origin/main`.

- [ ] **Step 3: Verify the only database-version diff is data version 69**

Run:

```bash
git diff origin/main...HEAD -- internal/db/db.go internal/db/db_test.go internal/db/schema.sql internal/postgres/schema.go internal/duckdb/schema.go internal/db/orphaned.go
```

Expected: only the documented `dataVersion = 69` change and its test remain;
there are no DDL, migration, compatibility-probe, or orphan-copy changes.

- [ ] **Step 4: Run resync and database tests**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/sync -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit the scope cleanup**

```bash
git add internal/db/orphaned.go internal/db/orphaned_test.go internal/sync/engine_integration_test.go internal/duckdb/schema.go
git commit -m "refactor(usage): drop unrelated resync changes"
```

The commit body must explain that the existing data-version reparse is
sufficient and that the PR does not change orphan archive semantics.

### Task 4: Verify the final branch scope and full behavior

**Files:**

- Verify all files changed by `git diff origin/main...HEAD`
- Verify: `docs/superpowers/specs/2026-07-18-copilot-reported-cost-cutoff-design.md`
- Verify: `docs/superpowers/plans/2026-07-18-copilot-reported-cost-cutoff.md`

**Interfaces:**

- Consumes: all prior tasks.
- Produces: a branch whose observable delta is timestamp-gated Copilot
  reported-cost selection with the existing contract preserved.

- [ ] **Step 1: Format Go and inspect the final diff**

Run:

```bash
gofmt -w internal/parser/copilot.go internal/parser/copilot_test.go internal/db/usage.go internal/db/usage_test.go internal/postgres/usage.go internal/postgres/usage_unit_test.go internal/duckdb/analytics_usage.go internal/server/huma_routes_sessions.go cmd/agentsview/session_usage.go cmd/agentsview/session_usage_test.go internal/db/orphaned.go internal/db/orphaned_test.go internal/sync/engine_integration_test.go
git diff --check
git diff --stat origin/main...HEAD
git diff --name-status origin/main...HEAD
```

Expected: no whitespace errors; no locale, API, UI, CLI, response-schema, DDL,
or orphan-copy deletion relative to `main`.

- [ ] **Step 2: Run Go validation**

Run:

```bash
go fmt ./...
go vet ./...
CGO_ENABLED=1 go test -tags fts5 ./internal/parser ./internal/db ./internal/postgres ./internal/duckdb ./internal/server ./internal/service ./cmd/agentsview ./internal/export -count=1
go build ./...
```

Expected: PASS.

- [ ] **Step 3: Run PostgreSQL parity tests**

Run against the existing dedicated test database:

```bash
TEST_PG_URL="postgres://agentsview_test:agentsview_test_password@localhost:5433/agentsview_test?sslmode=disable" CGO_ENABLED=1 go test -tags "fts5,pgtest" ./internal/postgres/... -count=1
```

Expected: PASS. Do not stop the existing PostgreSQL container without explicit
user approval.

- [ ] **Step 4: Run frontend validation**

From `frontend/`, run:

```bash
npm run i18n:compile
npm run check
```

Expected: PASS with zero errors.

- [ ] **Step 5: Confirm the final contract and cutoff mechanically**

Run:

```bash
rg -n "usage_summary_copilot_ai_credits|ai_credits|copilotAICredits|AI Credits" cmd frontend internal docs
rg -n "copilotUsageBasedPricingStartedAt|2026-06-01|copilot-reported" internal/parser docs/token-usage.md
git diff origin/main...HEAD -- internal/db/schema.sql internal/postgres/schema.go internal/duckdb/schema.go
```

Expected: all existing credit surfaces are present, the cutoff is documented
and tested, and the schema diff is empty.

- [ ] **Step 6: Report without pushing**

Report the local commit SHAs, validation evidence, and that the branch remains
unpushed. Do not change the PR body or post comments unless the user explicitly
requests it.
