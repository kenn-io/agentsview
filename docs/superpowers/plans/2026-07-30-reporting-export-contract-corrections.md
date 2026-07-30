# Reporting Export Schema v1 Completion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Complete reporting schema v1 with comprehensive usage collection,
layout-independent canonical output, consistent minute totals, explicit command
version selection, and refreshed synthetic fixtures.

**Architecture:** Collect activity sessions, usage sessions, and standalone
usage independently through one read transaction. Merge raw usage candidates
before one semantic sort, survivor-mask pass, cost-allocation pass, and complete
usage projection; filter that result by the activity-session set for activity
only. Validate and derive split-bearing minute totals at the export boundary,
and validate the CLI schema version before opening storage.

**Tech Stack:** Go, SQLite, Cobra, testify, canonical JSON, SHA-256, Markdown.

## Global Constraints

- Work only from the current branch head and current working tree.
- Do not fetch or inspect removed branch history or deleted fixture material.
- Schema version `1` remains the default and only supported reporting version.
- Merge session-linked and standalone usage before ordering, deduplication, cost
  allocation, and aggregation.
- Feed `allUsage` to usage totals and all usage breakdowns.
- Feed only activity-session rows from `allUsage` to activity and first-seen
  calculations.
- Never use database row IDs as ordering keys.
- Derive every split-bearing `agent_minutes` from its automated and interactive
  components after validation.
- Use only conspicuously synthetic names, labels, paths, repositories, and
  domains in tests, fixtures, documentation, and commit text.
- Do not push, rewrite history, or interact with the pull request.

______________________________________________________________________

### Task 1: Collect and merge every usage input

**Files:**

- Modify: `internal/db/activityreport.go`
- Modify: `internal/db/reporting_export.go`
- Test: `internal/db/reporting_export_test.go`

**Interfaces:**

- Produces:
  `reportingUsageSessionsFrom(context.Context, *sql.Tx, AnalyticsFilter, string, string) ([]activity.SessionMeta, []string, error)`.

- Produces:
  `activityReportUsageCandidatesFrom(context.Context, sessionExportQuerier, []string, string, string) ([]activity.UsageRow, *export.PricingBlock, error)`.

- Produces:
  `reportingStandaloneUsageCandidatesFrom(context.Context, *sql.Tx, activity.Query) ([]activity.UsageRow, error)`.

- Produces:
  `finalizeReportingUsage(activity.Query, []activity.UsageRow) []activity.UsageRow`.

- Produces:
  `reportingActivityUsage([]activity.UsageRow, map[string]struct{}) []activity.UsageRow`.

- Preserves `activityReportUsageFrom` by implementing its existing behavior on
  top of `activityReportUsageCandidatesFrom`.

- [ ] **Step 1: Extend the usage regression with an independently eligible
  session**

In `TestReportingExportIncludesStandaloneRowsOnlyInUsage`, seed a
`fixture-usage-only` session whose `started_at` and `ended_at` are outside the
selected day while its usage event occurs at `2026-07-28T09:10:00Z`. Give it
synthetic agent, project, and model labels and literal token and cost values.

Assert the session appears in the complete usage projection:

```go
assert.Equal(t, int64(31), hour9.Usage.Totals.InputTokens)
assert.Equal(t, []string{
	"model standalone usage",
	"model usage-only",
}, reportingUsageKeys(hour9.Usage.ByModel))
assert.Contains(t, reportingUsageKeys(hour9.Usage.ByAgent), "agent usage-only")
require.Len(t, hour9.Usage.ByProject, 1)
assert.Equal(t, "project usage-only", hour9.Usage.ByProject[0].Project)
```

Assert it creates no activity or first-seen state:

```go
assert.Zero(t, hour9.Activity.Totals.AgentMinutes)
assert.Zero(t, hour9.Activity.Totals.NewSessions)
assert.Zero(t, hour9.Activity.Totals.NewProjects)
assert.Zero(t, hour9.Activity.Totals.NewModels)
```

Keep the standalone row unattributed:

```go
assert.NotContains(
	t,
	reportingUsageProjectNames(hour9.Usage.ByProject),
	"standalone",
)
```

Finally compare summed export totals and all model, agent, and project
breakdowns with `GetDailyUsage` using exact token and money assertions.

- [ ] **Step 2: Add a cross-input deduplication regression**

Create `TestReportingExportDeduplicatesMergedUsageInputs`. Seed one
session-linked usage event and one standalone event with the same stable dedup
identity, timestamp, model, and token facts. Give the two candidates distinct
costs so double counting is visible.

Export the day and assert one literal survivor:

```go
hour := day.Hours[9]
assert.Equal(t, int64(41), hour.Usage.Totals.InputTokens)
assert.Equal(t, int64(7), hour.Usage.Totals.OutputTokens)
require.Len(t, hour.Usage.ByModel, 1)
```

Also assert that only an activity-eligible session row can contribute to
activity and first-seen fields.

- [ ] **Step 3: Run the focused tests and verify RED**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db \
  -run 'TestReportingExportIncludesStandaloneRowsOnlyInUsage|TestReportingExportDeduplicatesMergedUsageInputs' \
  -count=1
```

Expected: the usage-only session is absent, and cross-input deduplication fails
because the standalone loader currently applies its survivor mask separately.

- [ ] **Step 4: Split session usage loading into candidate and finalized
  layers**

Extract the SQL scan, pricing resolution, and semantic row mapping from
`activityReportUsageFrom` into:

```go
func (db *DB) activityReportUsageCandidatesFrom(
	ctx context.Context,
	source sessionExportQuerier,
	ids []string,
	lowerBound, upperBound string,
) ([]activity.UsageRow, *export.PricingBlock, error)
```

This helper returns all padded-range candidates with token amounts and raw cost
attributes populated. It does not call `activity.UsageSurvivorMask` or
`activity.AllocateUsageCosts`.

Keep the existing `activityReportUsageFrom` contract by calling the candidate
helper, sorting the candidates, applying `activity.UsageSurvivorMask` for its
query, and returning the surviving rows. This avoids changing the existing
activity-report caller.

- [ ] **Step 5: Load usage-session IDs independently**

Add `reportingUsageSessionsFrom`. Query the union of eligible message usage and
eligible `usage_events` in the padded UTC bounds, select distinct non-empty
session IDs, apply the existing default session filters, and order IDs by
session ID. Load only these metadata fields from `sessions`:

```text
id, title fallback, project, agent, machine, started_at, ended_at, is_automated
```

Sort the returned metadata and IDs by session ID after all chunks are loaded. Do
not use session activity-window overlap in this query.

- [ ] **Step 6: Return raw standalone candidates**

Rename `reportingStandaloneUsageFrom` to
`reportingStandaloneUsageCandidatesFrom`. Preserve its padded range query and
semantic field mapping, but remove its local sort and survivor-mask pass.
Standalone candidates retain an empty session ID and empty project.

- [ ] **Step 7: Merge, finalize, and route usage once**

In `reportingHoursFromSnapshot`:

1. load activity sessions and activity events;
1. load usage sessions independently;
1. load raw session-linked usage for the usage-session IDs;
1. load raw standalone usage;
1. append both raw slices into `allUsage`;
1. globally sort `allUsage`;
1. apply one survivor mask over the reporting range;
1. allocate costs once; and
1. derive `activityUsage` by membership in the activity-session ID set.

Build `sessionByID` and project identities from the union of activity and
usage-session metadata. Pass `allUsage` to every `reportingUsageForHour` call.
Pass only `activityUsage` to `activity.Aggregate` and `buildReportingFirstSeen`.

Use a membership filter, not `SessionID != ""`:

```go
func reportingActivityUsage(
	rows []activity.UsageRow,
	activityIDs map[string]struct{},
) []activity.UsageRow {
	out := make([]activity.UsageRow, 0, len(rows))
	for _, row := range rows {
		if _, ok := activityIDs[row.SessionID]; ok {
			out = append(out, row)
		}
	}
	return out
}
```

- [ ] **Step 8: Run focused and neighboring tests and verify GREEN**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db \
  -run 'TestReportingExport|TestReportingUsage|TestActivityReport' -count=1
```

Expected: PASS.

- [ ] **Step 9: Inspect and commit the unified usage change**

Run `gofmt` on changed Go files, inspect the complete staged diff, and scan
added strings for environment-derived identities or paths. Use the mandatory
commit skill and commit with:

```text
fix(export): collect complete reporting usage
```

### Task 2: Freeze aggregation order and minute consistency

**Files:**

- Modify: `internal/db/activityreport.go`
- Modify: `internal/db/reporting_export.go`
- Test: `internal/db/reporting_export_test.go`

**Interfaces:**

- Preserves global activity ordering by session ID, ordinal, timestamp, role,
  and model after every SQL chunk is loaded.

- Produces:
  `reportingDerivedAgentMinutes(string, float64, float64, float64) (float64, error)`.

- Changes:
  `reportingHourFromActivity(time.Time, activity.Report) (export.ReportingHour, error)`.

- Changes activity breakdown conversion helpers to return their output and an
  error.

- [ ] **Step 1: Strengthen the oversized-chunk regression**

Keep the two equivalent databases with `maxSQLVars + 1` sessions inserted in
opposite orders. Ensure at least one activity pair crosses the 500-ID boundary
and at least two usage candidates tie on timestamp and ordinal.

Assert exact canonical bytes and digests:

```go
assert.Equal(t, string(firstBytes), string(secondBytes))
assert.Equal(t, firstDay.Digest, secondDay.Digest)
```

Add a literal expected total so two identically wrong exports cannot satisfy the
test:

```go
assert.Equal(t, int64(1), firstDay.Hours[10].Usage.Totals.InputTokens)
```

Add a direct assertion that activity events loaded across chunks are globally
ordered by semantic fields, including timestamp, role, and model ties.

- [ ] **Step 2: Temporarily remove the final activity-event sort and verify the
  regression fails**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db \
  -run TestReportingExportIsIndependentOfArchiveLayout -count=1
```

Expected: FAIL with different bytes, digest, event order, or aggregate total.
Restore the sort immediately after confirming the regression.

- [ ] **Step 3: Write minute-validation tests**

Create table-driven tests around `reportingHourFromActivity` with testify.
Cover:

```go
{
	name:        "consistent",
	original:    0.5,
	automated:   0.25,
	interactive: 0.25,
	want:        0.5,
},
{
	name:        "inclusive tolerance boundary",
	original:    0.500000001,
	automated:   0.25,
	interactive: 0.25,
	want:        0.5,
},
{
	name:        "above tolerance",
	original:    math.Nextafter(0.500000001, math.Inf(1)),
	automated:   0.25,
	interactive: 0.25,
	wantErr:     true,
},
```

Also reject negative original totals, negative components, `NaN`, infinity, and
an infinite derived sum. Construct model, agent, and project breakdown rows with
different valid split values and assert every serialized `AgentMinutes` equals
the direct Go addition of its two serialized components.

- [ ] **Step 4: Run the minute tests and verify RED**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db \
  -run 'TestReportingHourDerivesAgentMinutes|TestReportingHourRejectsInvalidAgentMinutes' \
  -count=1
```

Expected: FAIL because the current conversion copies the original total without
validation.

- [ ] **Step 5: Implement the exact minute invariant**

Add:

```go
const reportingMinuteToleranceFactor = 1e-9

func reportingDerivedAgentMinutes(
	field string,
	original, automated, interactive float64,
) (float64, error) {
	if math.IsNaN(original) || math.IsInf(original, 0) || original < 0 {
		return 0, fmt.Errorf("%s agent minutes total is invalid", field)
	}
	if math.IsNaN(automated) || math.IsInf(automated, 0) || automated < 0 {
		return 0, fmt.Errorf("%s automated agent minutes is invalid", field)
	}
	if math.IsNaN(interactive) || math.IsInf(interactive, 0) || interactive < 0 {
		return 0, fmt.Errorf("%s interactive agent minutes is invalid", field)
	}
	derived := automated + interactive
	if math.IsInf(derived, 0) {
		return 0, fmt.Errorf("%s derived agent minutes is invalid", field)
	}
	scale := math.Max(1, math.Max(math.Abs(original), math.Abs(derived)))
	tolerance := reportingMinuteToleranceFactor * scale
	if math.Abs(original-derived) > tolerance {
		return 0, fmt.Errorf("%s agent minutes do not match components", field)
	}
	return derived, nil
}
```

Change `reportingHourFromActivity` and the activity breakdown converters to
return errors. Validate totals and every model, agent, and project row before
constructing the export value. Propagate errors from
`reportingHoursFromSnapshot` with the reporting period and breakdown key.

- [ ] **Step 6: Run deterministic and minute tests and verify GREEN**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db \
  -run 'TestReportingExportIsIndependentOfArchiveLayout|TestReportingHour' \
  -count=1
```

Expected: PASS.

- [ ] **Step 7: Inspect and commit ordering and minute validation**

Run `gofmt` on changed Go files, inspect the complete staged diff, and scan all
new test strings. Use the mandatory commit skill and commit with:

```text
fix(export): enforce canonical activity totals
```

### Task 3: Add explicit command schema selection

**Files:**

- Modify: `cmd/agentsview/export_reporting.go`
- Test: `cmd/agentsview/export_reporting_test.go`

**Interfaces:**

- Extends `exportReportingDeps` with:
  `openDatabase func(*cobra.Command) (*db.DB, func(), error)`.

- Produces: `bindReportingSchemaVersion(*cobra.Command) *int`.

- Produces: `validateReportingSchemaVersion(int) error`.

- [ ] **Step 1: Write table-driven explicit-version tests**

For `hour`, `day`, and `digest`, execute the command once with no version option
and once with `--schema-version 1`. Assert both invocations succeed and return
identical bytes containing `"schema_version":1`.

Use these argument forms:

```text
export hour --schema-version 1 2026-07-28-10
export day --schema-version 1 2026-07-28
export digest --schema-version 1 --from 2026-07-28 --to 2026-07-28
```

- [ ] **Step 2: Write rejection tests with an opener seam**

Construct each reporting subcommand with:

```go
opened := false
deps := exportReportingDeps{
	now: func() time.Time { return fixedNow },
	openDatabase: func(*cobra.Command) (*db.DB, func(), error) {
		opened = true
		return nil, func() {}, errors.New("unexpected database open")
	},
}
```

Execute valid command arguments with `--schema-version 2`. Assert:

```go
require.EqualError(t, err, "unsupported reporting schema version 2")
assert.False(t, opened)
assert.Empty(t, stdout)
assert.Empty(t, stderr)
```

- [ ] **Step 3: Run schema-version tests and verify RED**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview \
  -run TestExportReportingSchemaVersion -count=1
```

Expected: FAIL because the reporting commands do not define the option.

- [ ] **Step 4: Bind and validate the option before storage access**

Set `defaultExportReportingDeps().openDatabase` to `openReportingExportDB`. Bind
a local integer option on each reporting command:

```go
func bindReportingSchemaVersion(command *cobra.Command) *int {
	version := export.ReportingSchemaVersion
	command.Flags().IntVar(
		&version,
		"schema-version",
		export.ReportingSchemaVersion,
		"Reporting export schema version",
	)
	return &version
}
```

At the start of each `RunE`, before config loading or database opening, call:

```go
if err := validateReportingSchemaVersion(*schemaVersion); err != nil {
	return err
}
```

Use `deps.openDatabase(cmd)` for valid commands. Unsupported versions must not
call config loading, the opener seam, export logic, or the output writer.

- [ ] **Step 5: Run reporting command tests and verify GREEN**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview \
  -run 'TestExportReportingSchemaVersion|TestExportHour|TestExportDay|TestExportDigest' \
  -count=1
```

Expected: PASS.

- [ ] **Step 6: Inspect and commit schema selection**

Run `gofmt` on changed Go files, inspect the complete staged diff, and use the
mandatory commit skill to commit with:

```text
feat(export): select reporting schema v1 explicitly
```

### Task 4: Regenerate and document the completed v1 contract

**Files:**

- Modify: `cmd/agentsview/export_reporting_test.go`
- Modify: `cmd/agentsview/testdata/reporting/hour-v1.json`
- Modify: `cmd/agentsview/testdata/reporting/day-v1.json`
- Modify: `cmd/agentsview/testdata/reporting/digest-v1.json`
- Modify: `cmd/agentsview/testdata/reporting/manifest.sha256`
- Modify: `docs/reporting-export.md`

**Interfaces:**

- Uses the finalized database and command behavior from Tasks 1 through 3.

- Produces the frozen version 1 command bytes and their SHA-256 manifest.

- [ ] **Step 1: Extend the golden archive with a usage-only session**

Add a `fixture-usage-only` session with a session window outside the fixture day
and a usage event inside hour 11. Use only:

```text
machine: fixture-machine
agent: agent usage-only
project: project usage-only 雪
model: model usage-only α
repository example if needed: github.com/acme/example
domain if needed: example.invalid
```

Assert the regenerated hour contains its model, agent, and project usage
breakdowns while activity and `new_*` values remain unchanged. Keep standalone
usage out of project attribution.

- [ ] **Step 2: Update command examples and contract rules**

In `docs/reporting-export.md`:

- show `--schema-version 1` on hour, day, and digest examples;
- state that omission defaults to version 1 and other versions fail before
  opening storage;
- describe activity sessions, usage sessions, and standalone events as
  independent transaction-scoped input sets;
- state that merged `allUsage` feeds totals and every usage breakdown;
- state that only activity-session rows feed activity and `new_*`;
- state that standalone rows have no fabricated project;
- define the exact minute tolerance formula and inclusive boundary; and
- remove the activity-window clock-skew reconciliation exception because
  usage-session selection is now independent.

Keep the canonicalization section generic and preserve all version 1 byte rules.

- [ ] **Step 3: Regenerate the version 1 fixtures**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview \
  -run TestExportReportingGolden -count=1 -update
```

Expected: PASS and rewrite exactly the three JSON files and `manifest.sha256`.

- [ ] **Step 4: Verify fixture determinism and manifest integrity**

Run the golden test twice without `-update`:

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview \
  -run TestExportReportingGolden -count=2
```

Then verify each manifest entry with the platform SHA-256 utility and parse all
three JSON fixtures with `jq`. Expected: every hash matches and every JSON
document parses.

- [ ] **Step 5: Run formatting and complete Go verification**

Run:

```bash
gofmt -w cmd/agentsview/export_reporting_test.go
go fmt ./...
CGO_ENABLED=1 go test -tags fts5 ./...
go vet ./...
```

Expected: every command exits zero.

- [ ] **Step 6: Perform the complete staged-content audit**

Stage only the implementation, tests, fixtures, manifest, and reporting
documentation. Inspect:

```bash
git diff --cached --check
git diff --cached
jq -r '.. | strings' cmd/agentsview/testdata/reporting/*.json | sort -u
```

Verify every fixture string is a synthetic label, UTC timestamp, schema field,
digest, or synthetic project identity. Scan the complete staged content for
credentials, absolute environment paths, email addresses, non-example domains,
unexpected repository names, and unrelated prose. Correct any finding before
committing.

- [ ] **Step 7: Commit the frozen v1 artifacts**

Use the mandatory commit skill and commit with:

```text
docs(export): freeze completed reporting schema v1
```

- [ ] **Step 8: Confirm the final local state**

Run `git status --short` and report the commits and verification results. Do not
push, rewrite history, or post pull-request comments.
