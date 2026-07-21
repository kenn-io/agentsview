package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func TestSessionSearchFlagValidation(t *testing.T) {
	cmd := newSessionSearchCommand()
	cmd.SetArgs([]string{"needle", "--regex", "--fts"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

// TestSessionSearchSinceMutuallyExclusiveWithActiveSince verifies the error
// is returned before any service/DB access is attempted (no data dir is set
// up here), matching --regex/--fts's fail-fast validation style.
func TestSessionSearchSinceMutuallyExclusiveWithActiveSince(t *testing.T) {
	cmd := newSessionSearchCommand()
	cmd.SetArgs([]string{
		"needle", "--since", "14d", "--active-since", "2024-01-01T00:00:00Z",
	})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(),
		"--since and --active-since are mutually exclusive")
}

func TestSessionSearchSinceRejectsInvalidFormat(t *testing.T) {
	cmd := newSessionSearchCommand()
	cmd.SetArgs([]string{"needle", "--since", "3x"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid --since")
}

// seedSearchMessage inserts a single message for sessionID carrying content,
// so `session search <pattern>` has something to match.
func seedSearchMessage(t *testing.T, dataDir, sessionID, content string) {
	t.Helper()
	d, err := db.Open(filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	require.NoError(t, d.InsertMessages([]db.Message{{
		SessionID:     sessionID,
		Ordinal:       1,
		Role:          "user",
		Content:       content,
		ContentLength: len(content),
		Timestamp:     "2026-04-01T00:00:00Z",
	}}))
}

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

// TestSessionSearchSinceFiltersByActivity is the end-to-end regression test
// for the CRITICAL requirement that --since actually narrows results by
// resolving to the same active_since window --active-since already
// threads through to the search filter: a session active within the
// window survives, one outside it does not.
func TestSessionSearchSinceFiltersByActivity(t *testing.T) {
	dataDir := newAgentDataDir(t)
	seedSessionsWithOpts(t, dataDir,
		activitySeed("fresh", 2*time.Hour),
		activitySeed("stale", 20*24*time.Hour),
	)
	seedSearchMessage(t, dataDir, "fresh", "needle in fresh session")
	seedSearchMessage(t, dataDir, "stale", "needle in stale session")

	out, err := executeCommand(newRootCommand(),
		"session", "search", "needle", "--since", "7d", "--format", "json")
	require.NoError(t, err)

	got := decodeCLIJSON[service.ContentSearchResult](t, out)
	require.Len(t, got.Matches, 1)
	assert.Equal(t, "fresh", got.Matches[0].SessionID)
}

func TestSessionSearchFTSWithToolSource(t *testing.T) {
	cmd := newSessionSearchCommand()
	cmd.SetArgs([]string{"needle", "--fts", "--in", "tool_result"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "messages only")
}

func TestResolveContentSearchModeMapping(t *testing.T) {
	tests := []struct {
		name                                     string
		useRegex, useFTS, useSemantic, useHybrid bool
		wantMode                                 string
	}{
		{name: "default substring", wantMode: "substring"},
		{name: "regex", useRegex: true, wantMode: "regex"},
		{name: "fts", useFTS: true, wantMode: "fts"},
		{name: "semantic", useSemantic: true, wantMode: "semantic"},
		{name: "hybrid", useHybrid: true, wantMode: "hybrid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, err := resolveContentSearchMode(
				tt.useRegex, tt.useFTS, tt.useSemantic, tt.useHybrid, nil)
			require.NoError(t, err)
			assert.Equal(t, tt.wantMode, mode)
		})
	}
}

func TestResolveContentSearchModeMutualExclusion(t *testing.T) {
	tests := []struct {
		name                                     string
		useRegex, useFTS, useSemantic, useHybrid bool
	}{
		{name: "regex and fts", useRegex: true, useFTS: true},
		{name: "semantic and hybrid", useSemantic: true, useHybrid: true},
		{name: "regex and semantic", useRegex: true, useSemantic: true},
		{name: "fts and hybrid", useFTS: true, useHybrid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolveContentSearchMode(
				tt.useRegex, tt.useFTS, tt.useSemantic, tt.useHybrid, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "mutually exclusive")
		})
	}
}

func TestSessionSearchSemanticWithToolSource(t *testing.T) {
	cmd := newSessionSearchCommand()
	cmd.SetArgs([]string{"needle", "--semantic", "--in", "tool_input"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "messages only")
}

func TestSessionSearchHybridWithToolSource(t *testing.T) {
	cmd := newSessionSearchCommand()
	cmd.SetArgs([]string{"needle", "--hybrid", "--in", "tool_result"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "messages only")
}

// TestSessionSearchScopeRequiresSemanticOrHybrid verifies --scope fails
// fast (before any service/DB access) when set without --semantic/--hybrid.
func TestSessionSearchScopeRequiresSemanticOrHybrid(t *testing.T) {
	for _, args := range [][]string{
		{"needle", "--scope", "top"},
		{"needle", "--fts", "--scope", "all"},
		{"needle", "--regex", "--scope", "subordinate"},
	} {
		cmd := newSessionSearchCommand()
		cmd.SetArgs(args)
		err := cmd.Execute()
		require.Error(t, err, "args %v", args)
		assert.Contains(t, err.Error(), "--semantic or --hybrid", "args %v", args)
	}
}

// TestSessionSearchScopeRejectsInvalidValue verifies the value gate fires
// at the CLI boundary rather than deep in the store.
func TestSessionSearchScopeRejectsInvalidValue(t *testing.T) {
	cmd := newSessionSearchCommand()
	cmd.SetArgs([]string{"needle", "--semantic", "--scope", "bogus"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "top, all, or subordinate")
}

func TestValidateScopeFlag(t *testing.T) {
	tests := []struct {
		name                   string
		scope                  string
		useSemantic, useHybrid bool
		wantErr                string
	}{
		{name: "empty scope always valid"},
		{name: "top with semantic", scope: "top", useSemantic: true},
		{name: "all with hybrid", scope: "all", useHybrid: true},
		{name: "subordinate with semantic", scope: "subordinate", useSemantic: true},
		{name: "scope without mode flag", scope: "top",
			wantErr: "--semantic or --hybrid"},
		{name: "invalid value", scope: "bogus", useSemantic: true,
			wantErr: "top, all, or subordinate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateScopeFlag(tt.scope, tt.useSemantic, tt.useHybrid)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestPrintContentMatchesHumanShowsScoreForScoredMatches(t *testing.T) {
	score := 0.834
	res := &service.ContentSearchResult{
		Matches: []db.ContentMatch{
			{
				SessionID: "sess1",
				Project:   "proj",
				Location:  "message",
				Ordinal:   3,
				Snippet:   "hello world",
				Score:     &score,
			},
			{
				SessionID: "sess2",
				Project:   "proj",
				Location:  "message",
				Ordinal:   1,
				Snippet:   "no score here",
			},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printContentMatchesHuman(&buf, res, renderNow))
	out := buf.String()
	assert.Contains(t, out, "score=0.83")
	lines := bytes.Split(buf.Bytes(), []byte("\n"))
	require.NotEmpty(t, lines)
	assert.NotContains(t, string(lines[2]), "score=",
		"unscored match should not print a score")
}

func TestPrintContentMatchesHumanShowsContext(t *testing.T) {
	res := &service.ContentSearchResult{
		Matches: []db.ContentMatch{
			{
				SessionID: "sess1", Project: "proj", Location: "message",
				Ordinal: 5, Snippet: "the match line",
				ContextBefore: []db.Message{
					{Role: "user", Content: "earlier question"},
				},
				ContextAfter: []db.Message{
					{Role: "assistant", Content: "later reply"},
				},
			},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printContentMatchesHuman(&buf, res, renderNow))
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 4)
	assert.Equal(t, "  user: earlier question", lines[0])
	assert.Contains(t, lines[1], "sess1")
	assert.Contains(t, lines[2], "the match line")
	assert.Equal(t, "  assistant: later reply", lines[3])
}

func TestPrintContentMatchesHumanTruncatesContextLine(t *testing.T) {
	longContent := strings.Repeat("a", 250)
	res := &service.ContentSearchResult{
		Matches: []db.ContentMatch{
			{
				SessionID: "sess1", Ordinal: 1, Snippet: "match",
				ContextBefore: []db.Message{{Role: "user", Content: longContent}},
			},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printContentMatchesHuman(&buf, res, renderNow))
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.NotEmpty(t, lines)
	require.True(t, strings.HasPrefix(lines[0], "  user: "))
	body := strings.TrimPrefix(lines[0], "  user: ")
	assert.LessOrEqual(t, len([]rune(body)), 201)
	assert.True(t, strings.HasSuffix(body, "…"))
}

func TestContentMatchJSONRoundTripsContext(t *testing.T) {
	res := service.ContentSearchResult{
		Matches: []db.ContentMatch{
			{
				SessionID: "sess1", Ordinal: 5,
				ContextBefore: []db.Message{{Role: "user", Ordinal: 3, Content: "before"}},
				ContextAfter:  []db.Message{{Role: "assistant", Ordinal: 7, Content: "after"}},
			},
			{SessionID: "sess2", Ordinal: 1},
		},
	}
	data, err := json.Marshal(res)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"context_before"`)
	assert.Contains(t, string(data), `"context_after"`)

	var decoded service.ContentSearchResult
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Len(t, decoded.Matches, 2)
	require.Len(t, decoded.Matches[0].ContextBefore, 1)
	assert.Equal(t, "before", decoded.Matches[0].ContextBefore[0].Content)
	assert.Empty(t, decoded.Matches[1].ContextBefore)
}

func TestContentMatchJSONRoundTripsScore(t *testing.T) {
	score := 0.5
	res := service.ContentSearchResult{
		Matches: []db.ContentMatch{
			{SessionID: "sess1", Ordinal: 1, Score: &score},
			{SessionID: "sess2", Ordinal: 2},
		},
	}
	data, err := json.Marshal(res)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"score":0.5`)

	var decoded service.ContentSearchResult
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Len(t, decoded.Matches, 2)
	require.NotNil(t, decoded.Matches[0].Score)
	assert.InDelta(t, score, *decoded.Matches[0].Score, 0.0001)
	assert.Nil(t, decoded.Matches[1].Score)
}

// TestPrintContentMatchesHumanRendersUnitRangeAndSubMarker pins the human
// rendering for run-grouped semantic/hybrid hits: a multi-message unit
// renders "#<start>-<end> @<anchor>", a subordinate hit gains a "sub"
// marker, and a single-ordinal hit keeps today's plain "#<ordinal>" form.
func TestPrintContentMatchesHumanRendersUnitRangeAndSubMarker(t *testing.T) {
	score := 0.91
	res := &service.ContentSearchResult{
		Matches: []db.ContentMatch{
			{
				SessionID: "sess1", Project: "proj", Location: "message",
				Ordinal: 19, OrdinalRange: [2]int{12, 40},
				Subordinate: true, Score: &score, Snippet: "ranged hit",
			},
			{
				SessionID: "sess2", Project: "proj", Location: "message",
				Ordinal: 5, OrdinalRange: [2]int{5, 5},
				Snippet: "single-message unit",
			},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printContentMatchesHuman(&buf, res, renderNow))
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 4)

	assert.Contains(t, lines[0], "#12-40 @19", "range with anchor marker")
	assert.Contains(t, lines[0], " sub", "subordinate marker")
	assert.Contains(t, lines[0], "score=0.91")

	assert.Contains(t, lines[2], "#5", "single-ordinal hit keeps the plain form")
	assert.NotContains(t, lines[2], "@", "no anchor marker for single-ordinal hits")
	assert.NotContains(t, lines[2], " sub", "no subordinate marker for top-level hits")
}

// TestPrintContentMatchesTableBasic pins the flat (no --context) human
// rendering: a header row, one aligned row per match, the full session ID,
// the fused location:tool column, and an untruncated snippet when no
// terminal width is known (termWidth 0 — pipes, files, tests).
func TestPrintContentMatchesTableBasic(t *testing.T) {
	longSnippet := strings.Repeat("s", 300)
	res := &service.ContentSearchResult{
		Matches: []db.ContentMatch{
			{
				SessionID: "fc9367d6-38f7-4d18-863d-118dec238bd0",
				Project:   "yas", Location: "tool_result", ToolName: "Bash",
				Ordinal: 12, Snippet: longSnippet,
			},
			{
				SessionID: "sess2", Project: "proj2", Location: "message",
				Ordinal: 3, Snippet: "line one\nline two",
			},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printContentMatchesTable(&buf, res, 0, renderNow))
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 3)

	header, row1, row2 := lines[0], lines[1], lines[2]
	for _, col := range []string{"ID", "MATCH", "AGE", "PROJECT", "LOCATION", "SNIPPET"} {
		assert.Contains(t, header, col)
	}
	assert.NotContains(t, header, "SCORE",
		"SCORE column omitted when no match is scored")
	assert.Greater(t, strings.Index(header, "AGE"), strings.Index(header, "MATCH"),
		"AGE comes after MATCH in the unscored table")
	assert.Less(t, strings.Index(header, "AGE"), strings.Index(header, "PROJECT"),
		"AGE comes before PROJECT in the unscored table")

	assert.Contains(t, row1, "fc9367d6-38f7-4d18-863d-118dec238bd0")
	assert.Contains(t, row1, "#12")
	assert.Contains(t, row1, "tool_result:Bash")
	assert.Contains(t, row1, longSnippet, "snippet untruncated at width 0")
	assert.Contains(t, row2, "line one line two",
		"newlines collapsed to keep one row per match")

	// Columns align: each header label starts at the same rune offset as
	// the corresponding cell in every row.
	idIdx := strings.Index(header, "ID")
	matchIdx := strings.Index(header, "MATCH")
	assert.Equal(t, idIdx, strings.Index(row1, "fc9367d6"))
	assert.Equal(t, matchIdx, strings.Index(row1, "#12"))
	assert.Equal(t, matchIdx, strings.Index(row2, "#3"))
}

// TestPrintContentMatchesTableScoreColumn pins the conditional SCORE
// column: present when any match carries a score, with an em dash for
// unscored rows.
func TestPrintContentMatchesTableScoreColumn(t *testing.T) {
	score := 0.834
	res := &service.ContentSearchResult{
		Matches: []db.ContentMatch{
			{SessionID: "s1", Project: "p", Location: "message",
				Ordinal: 3, Snippet: "hit", Score: &score},
			{SessionID: "s2", Project: "p", Location: "message",
				Ordinal: 1, Snippet: "hit"},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printContentMatchesTable(&buf, res, 0, renderNow))
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 3)
	assert.Contains(t, lines[0], "SCORE")
	assert.Contains(t, lines[1], "0.83")
	assert.Contains(t, lines[2], emDash, "unscored row shows an em dash")
}

// TestPrintContentMatchesTableRangeAndSub pins the MATCH column for
// run-grouped hits: "#<start>-<end> @<anchor>" plus a "sub" marker for
// subordinate units.
func TestPrintContentMatchesTableRangeAndSub(t *testing.T) {
	res := &service.ContentSearchResult{
		Matches: []db.ContentMatch{
			{
				SessionID: "s1", Project: "p", Location: "message",
				Ordinal: 19, OrdinalRange: [2]int{12, 40},
				Subordinate: true, Snippet: "ranged",
			},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printContentMatchesTable(&buf, res, 0, renderNow))
	assert.Contains(t, buf.String(), "#12-40 @19 sub")
}

// TestPrintContentMatchesTableSnippetFillsWidth pins TTY behavior: the
// snippet expands to the remaining terminal width and is ellipsized there,
// so no row exceeds the terminal width.
func TestPrintContentMatchesTableSnippetFillsWidth(t *testing.T) {
	res := &service.ContentSearchResult{
		Matches: []db.ContentMatch{
			{SessionID: "s1", Project: "p", Location: "message",
				Ordinal: 1, Snippet: strings.Repeat("x", 500)},
		},
	}
	const width = 100
	var buf bytes.Buffer
	require.NoError(t, printContentMatchesTable(&buf, res, width, renderNow))
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 2)
	row := lines[1]
	assert.LessOrEqual(t, len([]rune(row)), width)
	assert.True(t, strings.HasSuffix(row, "…"), "truncated snippet gains an ellipsis")
}

// TestPrintContentMatchesTableLocationCap pins the LOCATION cap: on a
// TTY a huge tool name cannot starve the snippet column, while non-TTY
// output (termWidth 0) keeps the full value, matching the untruncated
// snippet policy for pipes.
func TestPrintContentMatchesTableLocationCap(t *testing.T) {
	res := &service.ContentSearchResult{
		Matches: []db.ContentMatch{
			{SessionID: "s1", Project: "p", Location: "tool_result",
				ToolName: strings.Repeat("t", 200), Ordinal: 1, Snippet: "hit"},
		},
	}
	loc := "tool_result:" + strings.Repeat("t", 200)

	var buf bytes.Buffer
	require.NoError(t, printContentMatchesTable(&buf, res, 200, renderNow))
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 2)
	assert.NotContains(t, lines[1], loc, "location is capped on a TTY")
	assert.Contains(t, lines[1], "…")
	assert.Contains(t, lines[1], "hit", "snippet survives a huge tool name")

	buf.Reset()
	require.NoError(t, printContentMatchesTable(&buf, res, 0, renderNow))
	lines = strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 2)
	assert.Contains(t, lines[1], loc, "piped output keeps the full location")
}

// TestPrintContentMatchesTableEmptyAndCursor pins the unchanged empty
// message and pagination footer around the table.
func TestPrintContentMatchesTableEmptyAndCursor(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, printContentMatchesTable(
		&buf, &service.ContentSearchResult{}, 0, renderNow))
	assert.Equal(t, "(no matches)\n", buf.String())

	buf.Reset()
	res := &service.ContentSearchResult{
		Matches: []db.ContentMatch{
			{SessionID: "s1", Project: "p", Location: "message",
				Ordinal: 1, Snippet: "hit"},
		},
		NextCursor: 7,
	}
	require.NoError(t, printContentMatchesTable(&buf, res, 0, renderNow))
	assert.Contains(t, buf.String(), "More results: --cursor 7")
}

// TestContentSnippetBudget pins the snippet width computation: 0 means
// unknown terminal (untruncated), otherwise the remaining width after the
// fixed columns with a readability floor on narrow terminals.
func TestContentSnippetBudget(t *testing.T) {
	tests := []struct {
		name        string
		termWidth   int
		otherWidths []int
		want        int
	}{
		{"unknown terminal", 0, []int{10, 5}, 0},
		{"wide terminal", 200, []int{36, 5, 4, 10}, 200 - (36 + 2) - (5 + 2) - (4 + 2) - (10 + 2)},
		{"narrow terminal floors", 60, []int{36, 5, 4, 10}, contentSnippetMinWidth},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, contentSnippetBudget(tt.termWidth, tt.otherWidths))
		})
	}
}

// TestPrintContentSearchResultPicksRenderer pins the dispatch: --context
// requests keep the record-style output (context lines cannot live in
// table rows), while flat results render as a table.
func TestPrintContentSearchResultPicksRenderer(t *testing.T) {
	res := &service.ContentSearchResult{
		Matches: []db.ContentMatch{
			{
				SessionID: "s1", Project: "p", Location: "message",
				Ordinal: 5, Snippet: "the match",
				ContextBefore: []db.Message{{Role: "user", Content: "before"}},
			},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printContentSearchResult(&buf, res, 1))
	assert.Contains(t, buf.String(), "  user: before",
		"context mode keeps record-style output")
	assert.NotContains(t, buf.String(), "SNIPPET")

	buf.Reset()
	require.NoError(t, printContentSearchResult(&buf, res, 0))
	assert.Contains(t, buf.String(), "SNIPPET", "flat mode renders the table")
}

// TestPrintContentMatchesTableSnippetExactFit pins the truncation boundary:
// a snippet that exactly fits the remaining width prints unmodified, with
// no rune dropped and no ellipsis.
func TestPrintContentMatchesTableSnippetExactFit(t *testing.T) {
	const width = 100
	// Fixed columns for this row: ID "s1" (header "ID" wins, 2) + MATCH
	// "#1"/"MATCH" (5) + AGE "—"/"AGE" (3) + PROJECT "p"/"PROJECT" (7) +
	// LOCATION "message"/"LOCATION" (8), each followed by a 2-space gap =
	// 35 used, leaving a 65-rune snippet budget.
	snippet := strings.Repeat("x", 65)
	res := &service.ContentSearchResult{
		Matches: []db.ContentMatch{
			{SessionID: "s1", Project: "p", Location: "message",
				Ordinal: 1, Snippet: snippet},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printContentMatchesTable(&buf, res, width, renderNow))
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 2)
	assert.True(t, strings.HasSuffix(lines[1], snippet),
		"exact-fit snippet prints unmodified")
	assert.Equal(t, width, len([]rune(lines[1])))
}

// TestPrintContentMatchesTableProjectCap pins the PROJECT cap: on a TTY
// an oversized or multi-line project name is collapsed and truncated so
// it cannot starve the snippet column, while non-TTY output keeps the
// full (whitespace-collapsed) value.
func TestPrintContentMatchesTableProjectCap(t *testing.T) {
	res := &service.ContentSearchResult{
		Matches: []db.ContentMatch{
			{SessionID: "s1", Project: strings.Repeat("p", 200) + "\nq",
				Location: "message", Ordinal: 1, Snippet: "hit"},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printContentMatchesTable(&buf, res, 200, renderNow))
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 2)
	assert.NotContains(t, lines[1], strings.Repeat("p", 200), "project is capped")
	assert.NotContains(t, lines[1], "\nq", "project whitespace collapsed")
	assert.Contains(t, lines[1], "…")
	assert.Contains(t, lines[1], "hit", "snippet survives a huge project name")
	assert.LessOrEqual(t,
		strings.Index(lines[1], "hit"), 100,
		"fixed columns stay bounded ahead of the snippet")

	buf.Reset()
	require.NoError(t, printContentMatchesTable(&buf, res, 0, renderNow))
	lines = strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 2)
	assert.Contains(t, lines[1], strings.Repeat("p", 200)+" q",
		"piped output keeps the full collapsed project")
}

// TestPrintContentMatchesTableWideRunesAlign pins display-width alignment:
// a project of full-width CJK runes occupies more terminal cells than its
// rune count, and the following column must still start at the same
// display column in every row.
func TestPrintContentMatchesTableWideRunesAlign(t *testing.T) {
	res := &service.ContentSearchResult{
		Matches: []db.ContentMatch{
			{SessionID: "s1", Project: "日本語", Location: "message",
				Ordinal: 1, Snippet: "hit"},
			{SessionID: "s2", Project: "ascii", Location: "message",
				Ordinal: 2, Snippet: "hit"},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printContentMatchesTable(&buf, res, 0, renderNow))
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 3)
	col := func(line string) int {
		i := strings.Index(line, "message")
		require.GreaterOrEqual(t, i, 0)
		return runewidth.StringWidth(line[:i])
	}
	assert.Equal(t, col(lines[1]), col(lines[2]),
		"LOCATION starts at the same display column despite wide runes")
}

// TestPrintContentMatchesTableWideSnippetBudget pins display-width
// truncation: a snippet of full-width runes must be cut so the whole row
// fits the terminal in display cells, not rune count.
func TestPrintContentMatchesTableWideSnippetBudget(t *testing.T) {
	res := &service.ContentSearchResult{
		Matches: []db.ContentMatch{
			{SessionID: "s1", Project: "p", Location: "message",
				Ordinal: 1, Snippet: strings.Repeat("界", 200)},
		},
	}
	const width = 100
	var buf bytes.Buffer
	require.NoError(t, printContentMatchesTable(&buf, res, width, renderNow))
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 2)
	assert.LessOrEqual(t, runewidth.StringWidth(lines[1]), width,
		"row fits the terminal in display cells")
	assert.True(t, strings.HasSuffix(lines[1], "…"))
}

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
			{
				SessionID: "s2", Project: "proj", Location: "message",
				Ordinal: 2, Snippet: "no timestamp", Score: &score,
			},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printContentMatchesHuman(&buf, res, renderNow))
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 4)
	assert.Contains(t, lines[2], emDash,
		"missing timestamp renders an em dash on the match line")
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
