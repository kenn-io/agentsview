# AGE column in `session search` results Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Render the per-match message timestamp as an `AGE` column in `agentsview session search` human output (table format and `--context` record format), leaving JSON and `session list` output unchanged.

**Architecture:** Extract the relative age buckets (`now`/seconds/minutes/hours/days) shared with `session list` into a reusable core in `session_list_render.go`, add a search-side helper in `session_search.go` that layers a year-disambiguating absolute branch on top, then thread a single captured `now` clock through the two human renderers so tests can inject a fixed time. Presentation-only: no DB, service, MCP, or JSON changes.

**Tech Stack:** Go 1.x, `github.com/mattn/go-runewidth` (display-width column sizing), `github.com/stretchr/testify` (assertions), Cobra CLI.

## Global Constraints

- Build/test tags: tests run with `CGO_ENABLED=1` and `-tags "fts5"` (the sqlite3 driver needs CGO; FTS5 is a build tag). Scope test runs to `./cmd/agentsview/`.
- After Go code changes, run `go fmt ./...` and `go vet -tags fts5 ./...` before every commit (repo Validation rule; `go vet` needs the `fts5` tag to compile).
- Test style: table-driven where practical; testify only — `require.X` for checks that must abort the test (setup, nil receivers, length checks before indexing), `assert.X` for independent checks. Never write `if got != want { t.Fatalf(...) }`.
- Commit every turn that changes tracked files; use conventional commit messages; no generated-with lines, test-plan sections, or command transcripts in commit messages.
- Do NOT change branches, push, rebase, amend, or open/merge PRs as part of this plan. This plan only writes and commits code on the current branch.
- Absolute age formats (verbatim from spec): relative under a week matches `session list` — `now` (future/skew), `42s`, `7m`, `3h`, `5d`. Beyond a week in search: `Jan 02` when the timestamp's year equals the current year, `Jan 2025` for prior years (Go layout `Jan 2006`). Em dash (`emDash`, the `—` constant) when the timestamp is empty or unparseable.
- `session list` output stays byte-identical: `humanizeSessionAge` keeps its existing year-less `Jan 02` beyond a week.

---

### Task 1: Extract shared relative-age core (`humanizeAgeRelative`)

Split the relative buckets out of `humanizeSessionAge` into a reusable core so the search side can share them. `humanizeSessionAge` must keep byte-identical behavior.

**Files:**
- Modify: `cmd/agentsview/session_list_render.go:70-93` (`humanizeSessionAge`)
- Test: `cmd/agentsview/session_list_render_test.go` (new `TestHumanizeAgeRelative`; existing `TestHumanizeSessionAge` must keep passing unmodified)

**Interfaces:**
- Produces: `humanizeAgeRelative(t, now time.Time) (string, bool)` — returns the relative bucket string (`"now"`, `"<n>s"`, `"<n>m"`, `"<n>h"`, `"<n>d"`) and `true` when the age is under a week; returns `("", false)` at a week or more. Callers supply their own absolute branch when it returns `false`.

- [ ] **Step 1: Write the failing test**

Add to `cmd/agentsview/session_list_render_test.go` (the file already imports `time`, `testify/assert`, and defines `renderNow = time.Date(2026, 6, 19, 23, 18, 0, 0, time.UTC)`):

```go
func TestHumanizeAgeRelative(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		t       time.Time
		wantStr string
		wantOK  bool
	}{
		{"future skew reads as now", renderNow.Add(5 * time.Second), "now", true},
		{"seconds", renderNow.Add(-30 * time.Second), "30s", true},
		{"minutes", renderNow.Add(-5 * time.Minute), "5m", true},
		{"hours", renderNow.Add(-3 * time.Hour), "3h", true},
		{"days", renderNow.Add(-2 * 24 * time.Hour), "2d", true},
		{"a week or more returns not-ok", renderNow.Add(-7 * 24 * time.Hour), "", false},
		{"long past returns not-ok", renderNow.Add(-90 * 24 * time.Hour), "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := humanizeAgeRelative(tc.t, renderNow)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantStr, got)
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `CGO_ENABLED=1 go test -tags "fts5" ./cmd/agentsview/ -run TestHumanizeAgeRelative -count=1`
Expected: FAIL — compile error `undefined: humanizeAgeRelative`.

- [ ] **Step 3: Write minimal implementation**

In `cmd/agentsview/session_list_render.go`, add the new function immediately above `humanizeSessionAge` (before the `// humanizeSessionAge renders...` comment at line 70), and rewrite `humanizeSessionAge` to delegate to it. Replace the whole existing `humanizeSessionAge` block (lines 70-93):

```go
// humanizeAgeRelative renders the age of t relative to now using the shared
// relative buckets: "now" for a future/clock-skewed timestamp, then
// seconds/minutes/hours/days. It returns ("", false) once the age reaches a
// week, leaving the absolute-format choice to each caller — session list keeps
// a year-less "Jan 02"; search disambiguates the year.
func humanizeAgeRelative(t, now time.Time) (string, bool) {
	d := now.Sub(t)
	switch {
	case d < 0:
		return "now", true
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s", true
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m", true
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h", true
	case d < 7*24*time.Hour:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d", true
	default:
		return "", false
	}
}

// humanizeSessionAge renders a session's last-activity time relative to
// now: seconds/minutes/hours/days for the recent past, an absolute year-less
// month and day beyond a week, and an em dash when no timestamp is available.
func humanizeSessionAge(s db.Session, now time.Time) string {
	t := sessionActivityTime(s)
	if t.IsZero() {
		return emDash
	}
	if rel, ok := humanizeAgeRelative(t, now); ok {
		return rel
	}
	return t.Format("Jan 02")
}
```

(No import changes: `strconv`, `time`, and `db` are already imported.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `CGO_ENABLED=1 go test -tags "fts5" ./cmd/agentsview/ -run 'TestHumanizeAgeRelative|TestHumanizeSessionAge|TestPrintSessionListHuman' -count=1`
Expected: PASS. `TestHumanizeSessionAge` and `TestPrintSessionListHuman` confirm `session list` behavior is byte-identical (regression assertion for the "session list unchanged" requirement).

- [ ] **Step 5: Format, vet, and commit**

```bash
go fmt ./...
go vet -tags fts5 ./...
git add cmd/agentsview/session_list_render.go cmd/agentsview/session_list_render_test.go
git commit -m "refactor(cli): extract shared humanizeAgeRelative core"
```

---

### Task 2: Search-side age helper (`humanizeMatchAge`)

Add the search-specific formatter: parse the match timestamp, reuse the shared relative core, and add the year-disambiguating absolute branch.

**Files:**
- Modify: `cmd/agentsview/session_search.go` (add `humanizeMatchAge` helper; add `"time"` import)
- Test: `cmd/agentsview/session_search_test.go` (new `TestHumanizeMatchAge`)

**Interfaces:**
- Consumes: `humanizeAgeRelative(t, now time.Time) (string, bool)` from Task 1; `parseSessionTime(s string) (time.Time, bool)` and the `emDash` constant, both already in `session_list_render.go`.
- Produces: `humanizeMatchAge(ts string, now time.Time) string` — parses `ts` (RFC3339/RFC3339Nano) via `parseSessionTime`, returns `emDash` when empty or unparseable, returns the shared relative bucket under a week, and beyond a week returns `t.Format("Jan 02")` when `t.Year() == now.Year()` else `t.Format("Jan 2006")`.

- [ ] **Step 1: Write the failing test**

Add to `cmd/agentsview/session_search_test.go` (the file already imports `time`, `testify/assert`, `testify/require`; `renderNow` is defined in the same package's `session_list_render_test.go`):

```go
func TestHumanizeMatchAge(t *testing.T) {
	t.Parallel()
	rfc := func(d time.Duration) string {
		return renderNow.Add(-d).Format(time.RFC3339)
	}
	tests := []struct {
		name string
		ts   string
		want string
	}{
		{"seconds", rfc(30 * time.Second), "30s"},
		{"minutes", rfc(5 * time.Minute), "5m"},
		{"hours", rfc(3 * time.Hour), "3h"},
		{"days", rfc(2 * 24 * time.Hour), "2d"},
		{"future skew reads as now", renderNow.Add(5 * time.Second).Format(time.RFC3339), "now"},
		{"current-year absolute", "2026-01-02T08:00:00Z", "Jan 02"},
		{"prior-year absolute", "2025-01-02T08:00:00Z", "Jan 2025"},
		{"RFC3339Nano parses", renderNow.Add(-time.Hour).Format(time.RFC3339Nano), "1h"},
		{"empty is em dash", "", emDash},
		{"unparseable is em dash", "not-a-time", emDash},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, humanizeMatchAge(tc.ts, renderNow))
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `CGO_ENABLED=1 go test -tags "fts5" ./cmd/agentsview/ -run TestHumanizeMatchAge -count=1`
Expected: FAIL — compile error `undefined: humanizeMatchAge`.

- [ ] **Step 3: Write minimal implementation**

In `cmd/agentsview/session_search.go`, add `"time"` to the import block (between `"strings"` and the third-party imports so the stdlib group stays grouped):

```go
import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
	"golang.org/x/term"
)
```

Then add the helper. Place it immediately below `printContentSearchResult` (after its closing brace at line 205, before the `contentTerminalWidth` comment):

```go
// humanizeMatchAge renders a content match's timestamp for the search AGE
// column. It parses the RFC3339/RFC3339Nano string via parseSessionTime
// (returning emDash when empty or unparseable), uses the shared relative
// buckets under a week, and beyond a week formats an absolute date that
// disambiguates the year: "Jan 02" when the timestamp falls in now's year,
// "Jan 2006" (e.g. "Jan 2025") for prior years. Search spans the whole
// multi-year archive, so the year matters here even though session list omits
// it.
func humanizeMatchAge(ts string, now time.Time) string {
	t, ok := parseSessionTime(ts)
	if !ok {
		return emDash
	}
	if rel, ok := humanizeAgeRelative(t, now); ok {
		return rel
	}
	if t.Year() == now.Year() {
		return t.Format("Jan 02")
	}
	return t.Format("Jan 2006")
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `CGO_ENABLED=1 go test -tags "fts5" ./cmd/agentsview/ -run TestHumanizeMatchAge -count=1`
Expected: PASS.

- [ ] **Step 5: Format, vet, and commit**

```bash
go fmt ./...
go vet -tags fts5 ./...
git add cmd/agentsview/session_search.go cmd/agentsview/session_search_test.go
git commit -m "feat(cli): add search-side humanizeMatchAge helper"
```

---

### Task 3: Render the AGE column and thread the clock

Wire `humanizeMatchAge` into both human renderers: a new `AGE` table column (immediately after `MATCH`, before the optional `SCORE`) and an age token on the `--context` match line. Thread a captured `now time.Time` into `printContentMatchesTable` and `printContentMatchesHuman`; `printContentSearchResult` captures `time.Now()` once. Update all existing call sites and the one width-sensitive test.

**Files:**
- Modify: `cmd/agentsview/session_search.go:198-205` (`printContentSearchResult`), `:277-372` (`printContentMatchesTable`), `:381-413` (`printContentMatchesHuman`)
- Test: `cmd/agentsview/session_search_test.go` (new `TestPrintContentMatchesTableAgeColumn`, `TestPrintContentMatchesHumanAgeToken`; thread `renderNow` through every existing `printContentMatchesTable`/`printContentMatchesHuman` call; recompute `TestPrintContentMatchesTableSnippetExactFit`)

**Interfaces:**
- Consumes: `humanizeMatchAge(ts string, now time.Time) string` from Task 2.
- Produces (new signatures — every caller in this package must pass `now`):
  - `printContentMatchesTable(w io.Writer, res *service.ContentSearchResult, termWidth int, now time.Time) error`
  - `printContentMatchesHuman(w io.Writer, res *service.ContentSearchResult, now time.Time) error`
  - `printContentSearchResult(w io.Writer, res *service.ContentSearchResult, contextN int) error` — signature unchanged; it now captures `now := time.Now()` and forwards it.

- [ ] **Step 1: Write the failing tests**

Add to `cmd/agentsview/session_search_test.go`:

```go
// TestPrintContentMatchesTableAgeColumn pins the AGE column: present in the
// header immediately after MATCH and before the optional SCORE column, with a
// relative bucket for recent matches and a year-disambiguated absolute date
// for older ones. Matches with no timestamp render an em dash.
func TestPrintContentMatchesTableAgeColumn(t *testing.T) {
	score := 0.83
	res := &service.ContentSearchResult{
		Matches: []db.ContentMatch{
			{
				SessionID: "s1", Project: "p", Location: "message",
				Ordinal: 1, Snippet: "recent hit", Score: &score,
				Timestamp: renderNow.Add(-3 * time.Hour).Format(time.RFC3339),
			},
			{
				SessionID: "s2", Project: "p", Location: "message",
				Ordinal: 2, Snippet: "old hit", Score: &score,
				Timestamp: "2025-01-02T08:00:00Z",
			},
			{
				SessionID: "s3", Project: "p", Location: "message",
				Ordinal: 3, Snippet: "no timestamp", Score: &score,
			},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printContentMatchesTable(&buf, res, 0, renderNow))
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 4)

	header := lines[0]
	assert.Contains(t, header, "AGE")
	matchIdx := strings.Index(header, "MATCH")
	ageIdx := strings.Index(header, "AGE")
	scoreIdx := strings.Index(header, "SCORE")
	require.GreaterOrEqual(t, matchIdx, 0)
	require.GreaterOrEqual(t, ageIdx, 0)
	require.GreaterOrEqual(t, scoreIdx, 0)
	assert.Greater(t, ageIdx, matchIdx, "AGE comes after MATCH")
	assert.Less(t, ageIdx, scoreIdx, "AGE comes before SCORE")

	assert.Contains(t, lines[1], "3h")
	assert.Contains(t, lines[2], "Jan 2025")
	assert.Contains(t, lines[3], emDash, "missing timestamp renders an em dash")
}

// TestPrintContentMatchesHumanAgeToken pins the --context record format: the
// age token sits on the match line between the ordinal/score markers and the
// project, e.g. "s1  #14 score=0.83  3h  proj  message".
func TestPrintContentMatchesHumanAgeToken(t *testing.T) {
	score := 0.83
	res := &service.ContentSearchResult{
		Matches: []db.ContentMatch{
			{
				SessionID: "s1", Project: "proj", Location: "message",
				Ordinal: 14, Snippet: "hit", Score: &score,
				Timestamp: renderNow.Add(-3 * time.Hour).Format(time.RFC3339),
			},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printContentMatchesHuman(&buf, res, renderNow))
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 2)
	line := lines[0]
	scoreIdx := strings.Index(line, "score=0.83")
	ageIdx := strings.Index(line, "3h")
	projIdx := strings.Index(line, "proj")
	require.GreaterOrEqual(t, scoreIdx, 0)
	require.GreaterOrEqual(t, ageIdx, 0)
	require.GreaterOrEqual(t, projIdx, 0)
	assert.Greater(t, ageIdx, scoreIdx, "age token comes after the score marker")
	assert.Less(t, ageIdx, projIdx, "age token comes before the project")
}
```

Also update the existing tests that call the changed signatures. Thread `renderNow` as the new final argument into every `printContentMatchesHuman(&buf, res)` call (there are 4: in `TestPrintContentMatchesHumanShowsScoreForScoredMatches`, `TestPrintContentMatchesHumanShowsContext`, `TestPrintContentMatchesHumanTruncatesContextLine`, `TestPrintContentMatchesHumanRendersUnitRangeAndSubMarker`) so each reads `printContentMatchesHuman(&buf, res, renderNow)`, and into every `printContentMatchesTable(&buf, res, <width>)` call (in `TestPrintContentMatchesTableBasic`, `TestPrintContentMatchesTableScoreColumn`, `TestPrintContentMatchesTableRangeAndSub`, `TestPrintContentMatchesTableSnippetFillsWidth`, `TestPrintContentMatchesTableLocationCap` (both calls), `TestPrintContentMatchesTableEmptyAndCursor` (both calls), `TestPrintContentMatchesTableSnippetExactFit`, `TestPrintContentMatchesTableProjectCap` (both calls), `TestPrintContentMatchesTableWideRunesAlign`, `TestPrintContentMatchesTableWideSnippetBudget`) so each reads `printContentMatchesTable(&buf, res, <width>, renderNow)`. `printContentSearchResult` keeps its signature, so its two calls in `TestPrintContentSearchResultPicksRenderer` are unchanged. None of these fixtures set `Timestamp`, so their AGE cell is the em dash — no existing content assertion depends on the AGE cell except the exact-fit width test below.

Then recompute `TestPrintContentMatchesTableSnippetExactFit`. The new AGE column (header `AGE` width 3, cell `—` width 1 → column width 3, plus a 2-space gap) consumes 5 more cells, so the exact-fit snippet budget at width 100 drops from 70 to 65. Replace the comment and snippet literal:

```go
	// Fixed columns for this row: ID "s1" (header "ID" wins, 2) + MATCH
	// "#1"/"MATCH" (5) + AGE "—"/"AGE" (3) + PROJECT "p"/"PROJECT" (7) +
	// LOCATION "message"/"LOCATION" (8), each followed by a 2-space gap =
	// 35 used, leaving a 65-rune snippet budget.
	snippet := strings.Repeat("x", 65)
```

(The `assert.Equal(t, width, len([]rune(lines[1])))` line below it still holds: 35 fixed + 65 snippet = 100.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `CGO_ENABLED=1 go test -tags "fts5" ./cmd/agentsview/ -run 'TestPrintContentMatches' -count=1`
Expected: FAIL — compile error `not enough arguments in call to printContentMatchesTable` / `printContentMatchesHuman` (the production functions still have the old signatures).

- [ ] **Step 3: Write the implementation**

In `cmd/agentsview/session_search.go`, update `printContentSearchResult` to capture the clock once (replace lines 198-205):

```go
// printContentSearchResult renders a content search result for humans.
// Flat results (no --context) render as an aligned table sized to the
// terminal; --context requests keep the record-style output because
// per-match context lines cannot live inside table rows. The clock is
// captured once here and threaded into the row loop so tests can inject a
// fixed time.
func printContentSearchResult(
	w io.Writer, res *service.ContentSearchResult, contextN int,
) error {
	now := time.Now()
	if contextN > 0 {
		return printContentMatchesHuman(w, res, now)
	}
	return printContentMatchesTable(w, res, contentTerminalWidth(w), now)
}
```

Update `printContentMatchesTable` (the `AGE` column is added to the header list right after `MATCH`, and to each row's cells right after the `match` cell; because it is a non-snippet column it is auto-sized by the existing `widths` loop and absorbed by `contentSnippetBudget` with no other change). Replace the signature line and the header/cells construction. New signature:

```go
func printContentMatchesTable(
	w io.Writer, res *service.ContentSearchResult, termWidth int, now time.Time,
) error {
```

Change the header list (currently at line 291-295) from:

```go
	headers := []string{"ID", "MATCH"}
	if hasScore {
		headers = append(headers, "SCORE")
	}
	headers = append(headers, "PROJECT", "LOCATION", "SNIPPET")
```

to:

```go
	headers := []string{"ID", "MATCH", "AGE"}
	if hasScore {
		headers = append(headers, "SCORE")
	}
	headers = append(headers, "PROJECT", "LOCATION", "SNIPPET")
```

Change the per-row cells construction (currently at line 315) from:

```go
		cells := []string{sanitizeTerminal(m.SessionID), match}
```

to:

```go
		cells := []string{
			sanitizeTerminal(m.SessionID), match, humanizeMatchAge(m.Timestamp, now),
		}
```

(No other change to `printContentMatchesTable`: `hasScore` still appends `SCORE` after `AGE`, and PROJECT/LOCATION/SNIPPET follow unchanged. The width/budget/alignment logic is untouched.)

Update `printContentMatchesHuman`. New signature:

```go
func printContentMatchesHuman(w io.Writer, res *service.ContentSearchResult, now time.Time) error {
```

Change the trailing match-line print (currently lines 401-402) from:

```go
		fmt.Fprintf(w, "  %s  %s\n",
			sanitizeTerminal(m.Project), sanitizeTerminal(loc))
```

to insert the age token between the ordinal/score markers and the project:

```go
		fmt.Fprintf(w, "  %s  %s  %s\n",
			humanizeMatchAge(m.Timestamp, now),
			sanitizeTerminal(m.Project), sanitizeTerminal(loc))
```

- [ ] **Step 4: Run the full package tests to verify they pass**

Run: `CGO_ENABLED=1 go test -tags "fts5" ./cmd/agentsview/ -count=1`
Expected: PASS — the new AGE tests pass, every existing table/human/record test passes with `renderNow` threaded through, and `TestPrintSessionListHuman`/`TestHumanizeSessionAge` still confirm `session list` is unchanged. (Run the whole package, not just `-run TestPrintContentMatches`, to catch any missed call site.)

- [ ] **Step 5: Format, vet, and commit**

```bash
go fmt ./...
go vet -tags fts5 ./...
git add cmd/agentsview/session_search.go cmd/agentsview/session_search_test.go
git commit -m "feat(cli): render AGE column in session search results"
```
